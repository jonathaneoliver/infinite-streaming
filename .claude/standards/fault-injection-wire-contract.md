# Fault-injection wire contract (proxy HTTP fault types)

**Read this before touching `go-proxy/cmd/server/main.go § applySocketFault` or any of the fault-type case branches.** Each fault's on-the-wire shape is **contract** — characterization tests interpret player responses against the specific shape, and silently changing a fault's behaviour silently invalidates the test data and any conclusions drawn from it.

If you need to add a new behaviour, add a **new fault-type name**. Don't repurpose an existing one.

## The contract — what each fault MUST look like on the wire

### Connection-level (no HTTP response written)

| Fault type | TCP shape |
|---|---|
| `request_connect_reset` | Hijack TCP socket, set `SO_LINGER=0`, close → **RST**. No HTTP bytes ever transmitted. |
| `request_connect_hang` | Hijack TCP socket, hold open for `socketHangDuration`, then graceful close → **FIN**. No HTTP bytes ever transmitted. |
| `request_connect_delayed` | Hijack TCP socket, hold open for `socketDelayDuration`, then graceful close → **FIN**. No HTTP bytes ever transmitted. |

Real-world correspondences: load balancer rejecting connection / firewall block / midpoint TCP filter.

### Headers-only (headers sent, no body)

| Fault type | Wire shape |
|---|---|
| `request_first_byte_reset` | Send `HTTP/1.1 200 OK` + `Transfer-Encoding: chunked` headers. Then `SO_LINGER=0` close → **RST before any body byte**. |
| `request_first_byte_hang` | Send `HTTP/1.1 200 OK` + chunked headers. Hold socket open for `socketHangDuration` with no body bytes. Then graceful close → **FIN with no body**. |
| `request_first_byte_delayed` | Send `HTTP/1.1 200 OK` + chunked headers. Wait `socketDelayDuration`. Then graceful close → **FIN with no body**. |

Real-world correspondences: server accepted the request but stalled before writing any payload (TLS handshake oddities, server-side queueing fault, head-of-line blocking).

### Body-truncated (headers + partial REAL body bytes)

| Fault type | Wire shape |
|---|---|
| `request_body_reset` | Send chunked headers. **Fetch real upstream segment** (2 s timeout); write up to `socketMidBodyBytes` (= 64 KB) of those upstream bytes to the client. Then `SO_LINGER=0` close → **RST after a parseable prefix**. |
| `request_body_hang` | Send chunked headers + 64 KB of real upstream bytes. Hold socket open `socketHangDuration`. Then graceful close → **FIN after parseable prefix**. |
| `request_body_delayed` | Send chunked headers + 64 KB of real upstream bytes. Wait `socketDelayDuration`. Then graceful close → **FIN after parseable prefix**. |

Real-world correspondences:
- `*_reset` mid-body: server crash / load-balancer kill / hard TCP drop. Client sees "connection reset by peer."
- `*_hang` mid-body: stuck CDN edge / server thread blocked / nginx idle timeout misconfigured. Client sees data flowing then nothing.
- `*_delayed`: server-side scaling event / pod drain / CDN graceful keepalive close mid-transfer. Client sees normal close after partial real data.

**Real bytes, NOT synthetic filler.** When upstream fetch fails (DNS, connect, non-2xx, timeout), the fault falls back to `bytes.Repeat([]byte("X"), 64 KB)` so the close shape still applies — but a `[GO-PROXY] fault upstream-prefetch-failed url=…` log line is emitted so the operator knows the partial-body content was synthetic. Don't lower the timeout or remove the fallback path without filing a bug.

### Body-corrupted (different surface — DON'T confuse with body_*)

| Fault type | Wire shape |
|---|---|
| `corrupted` | Full proxy round-trip: fetch upstream, stream the FULL response to the client, but with every byte **replaced by zero** before write. Headers honest (full Content-Length), close graceful. |

Real-world correspondences: bit-flip on a path, transcoder mis-output, media corruption in flight.

Note: `corrupted` and `request_body_*` are **NOT interchangeable.** `corrupted` sends garbage in place of a full response. `request_body_*` sends a valid prefix and then aborts. Players respond very differently (demux error vs partial-decode-then-restart). Tests assert against the specific behaviour.

### Time-triggered (not in the fault-rule list — uses transfer_timeouts)

| Fault type | Wire shape |
|---|---|
| `transfer_active_timeout` | Normal proxy passthrough until `transfer_active_timeout_seconds` elapses. Then the proxy cancels its upstream context and closes the client connection → **FIN at whatever byte count was reached**. |
| `transfer_idle_timeout` | Same shape, but the timer resets on each upstream write. Closes on a `transfer_idle_timeout_seconds` gap with no upstream activity. |

These behave functionally similar to `request_body_delayed` but are time-driven (not arming-driven) and run on the existing copy path (no socket hijack).

## Real-world failure modes — what each fault MODELS

This is the most important section: each fault shape exists to reproduce a class of real-world failure. The wire behaviour is engineered to be indistinguishable from what the client would see in that real scenario. When considering whether a behaviour change is safe, ask "does this still model the same real failure?"

### `request_connect_reset` — Firewall / midpoint RST

**Real scenarios:**
- Corporate firewall sends RST on outbound connection to a flagged domain.
- ISP-level deep packet inspection injects RST on blocked traffic.
- Cloud load balancer returns RST on a non-existent backend (rare; usually it's connection_refused).
- NAT table eviction on a long-running connection causes the gateway to RST a retry.

**What the client OS surfaces:** `ECONNRESET` immediately after TCP handshake. No HTTP status. AVPlayer logs `kCFErrorDomainCFNetwork` errors like `kCFURLErrorNetworkConnectionLost` or `-1005`.

### `request_connect_hang` — Black-hole / silent drop

**Real scenarios:**
- Stateful firewall silently drops packets without a response (typical "stealth" rule).
- Network partition where SYN-ACKs are lost in transit.
- Load balancer overwhelmed and not responding (no RST, no FIN — just nothing).
- Half-broken TCP state on either side that survives connect but won't respond.

**What the client OS surfaces:** Connect succeeds, then the read-call hangs until the OS-level timeout (typically 30-75s for `URLSession`). Eventually `NSURLErrorTimedOut` (-1001).

### `request_connect_delayed` — High-RTT path / overloaded server

**Real scenarios:**
- Satellite link (500-1500ms RTT) — connection establishes but first response is slow.
- Server-side queue backup (cold-start, autoscaling lag) where SYN-ACK is fine but the server takes seconds to dispatch.
- Distant CDN edge being used as fallback after the close one failed.

**What the client OS surfaces:** Connect succeeds, response trickles in slowly. AVPlayer may flag this as a slow-network condition and downshift before any data has actually arrived.

### `request_first_byte_reset` — Server crash after accept

**Real scenarios:**
- Web server process crashes between accepting the connection and writing the response (OOM, SIGSEGV, panic).
- Reverse proxy rejects the response from upstream after headers were committed.
- Application-layer firewall (WAF) decides post-accept to RST the connection because of a rule match.

**What the client OS surfaces:** Got `HTTP/1.1 200 OK` headers (so it knows the request was accepted), then `ECONNRESET` reading the body. Distinct from connect-reset because the client may have already committed buffer space.

### `request_first_byte_hang` — Server thread blocked

**Real scenarios:**
- Backend thread stuck on a lock / database query after sending headers.
- File-serving CDN edge that's still locating the file on its local disk (slow first-byte but typically <2s; if >30s, this fault).
- Stream-origin worker that's still seeking inside a large file before yielding bytes.

**What the client OS surfaces:** Headers received. Then read-call hangs for the configured duration. iOS's segment-fetch timeout (~30s) eventually fires.

### `request_first_byte_delayed` — Slow origin / cold cache

**Real scenarios:**
- CDN cache miss → fetch from origin → re-warm before serving (8-15s delay common).
- Tiered storage where a "cold" object has to be fetched from S3 Glacier or similar.
- Origin server is slow to compute the response (signing, range-recomposition).

**What the client OS surfaces:** Headers received quickly, body arrives after several-second delay. Tests AVPlayer's reaction to "this URL is real but slow" — does it wait, downshift, or skip?

### `request_body_reset` — Server crash mid-transfer / hard load balancer kill

**Real scenarios (most operationally relevant — most common cause of "stutter then re-fetch"):**
- Backend pod gets `SIGKILL` mid-stream (no graceful drain, no FIN).
- Stateful firewall drops connection mid-flow because of session expiry.
- Network gear failure mid-transfer (BGP route flap, FRR convergence) — TCP stack on the close side sees RST.
- Load balancer connection-tracking table fills, forcibly RSTs the oldest connection in the middle of its transfer.

**What the client OS surfaces:** Headers + some real body bytes, then `ECONNRESET`. AVPlayer's response varies: some versions retry from byte 0 immediately, some downshift first, some wait briefly then retry. THIS is what we're characterizing in the abort test.

### `request_body_hang` — Stuck mid-stream

**Real scenarios:**
- Backend writer's TCP buffer is full and never drains (recv window deadlock).
- CDN edge mid-stream stalls because upstream object went away mid-fetch.
- Stream-origin process running out of memory but not crashing (paging hard, no progress).

**What the client OS surfaces:** Headers + initial bytes, then no more data, no close. Connection stays open silently. iOS's idle-segment-fetch timeout (~10-20s on top of normal latency) eventually fires.

### `request_body_delayed` — Mobile handoff / CDN scaling event

**Real scenarios (one of the most common in real production):**
- **Mobile WiFi→LTE handoff**: phone moves out of WiFi range during a download. The OS terminates the TCP connection on the WiFi interface; the receiving side gets FIN at whatever byte count had been transferred. AVPlayer must reconnect on the new interface.
- **CDN edge keepalive cycle**: many CDNs close connections after N requests or M seconds, even mid-body. The client gets a graceful close with partial real data.
- **Server scaling / pod drain**: backend gracefully shuts down a worker, completing in-flight responses partially before closing.
- **nginx `proxy_send_timeout` / `request_terminate_timeout`**: server-side timeout fires mid-response, returns 200 headers but cuts the body and closes.

**What the client OS surfaces:** Headers + valid partial body + clean FIN. The most "ambiguous" failure shape — looks like a successful but truncated download. Player may attempt Range-resume here (this is exactly when partial-resume helps). This is one of the most under-characterized real-world failure modes.

### `corrupted` — Bit-flip / transcoder corruption

**Real scenarios:**
- Memory ECC fault on a CDN cache node returns wrong bits.
- Transcoder mis-output: H.264 / fMP4 chunks contain invalid bytes (mux bug).
- TLS truncation attack returns bytes the server didn't write.
- Storage corruption at rest (object store bit-rot).

**What the client OS surfaces:** Full successful response that decodes wrong. The demuxer / decoder fails. This is the only fault that exercises the player's media-corruption path; the abort faults exercise the network-error path.

### `transfer_active_timeout` / `transfer_idle_timeout` — Slow-link disconnect

**Real scenarios:**
- Mobile network throughput drops mid-segment (entering an elevator, train tunnel) — transfer slows below the buffer-fill rate; client or server eventually times out.
- Server-side max-request-time policy (e.g. nginx's 30-second cap on a stream).
- ISP-level QoS that throttles long-running transfers.

These are TIME-triggered, complementary to the byte-triggered `request_body_*` faults. Useful when you want to model "this connection is technically fine but takes too long" rather than "this connection died at byte N."

## Identifiers + constants

```
socketMidBodyBytes  = 64 * 1024
socketHangDuration  = 30 * time.Second
socketDelayDuration = 10 * time.Second  (verify in source — these can drift)
```

`socketMidBodyBytes` controls how much real-prefix data we write before the close. Don't change it without re-validating the abort-characterization test results — the player's response depends on whether the demuxer got enough bytes to parse the segment's initial structures.

## How tests interpret each shape

- `request_connect_*` → **TCP-level error before HTTP** — client never gets a status line. AVPlayer typically retries with backoff.
- `request_first_byte_*` → **Got headers, no data** — AVPlayer often interprets this as "this URL is permanently bad" and may skip the segment outright.
- `request_body_*` → **Real partial data + close** — AVPlayer demuxes what it got, decides whether it can play any of it, then re-requests. Range-resume is theoretically available here but iOS doesn't use it in practice (per abort_test characterization).
- `corrupted` → **Full response, every byte zero** — demuxer fails on garbage; this is a hard "media decode error" path.

## When you're about to change this code

1. **Don't change wire shapes silently.** A two-line change to "improve" how an existing fault behaves is the highest-risk class of edit in this file.
2. **Add new fault-type names** for new behaviours. The proxy's fault enum and the docs above all key on the string name.
3. **Re-run** `tests/characterization/modes/abort_test.go` after any change — compare results against the previous run's `report.json` to spot drift.
4. **Add new entries to this doc** for new fault-type names AND update the fault-rule schema in `api/openapi/v2/proxy.yaml`.

## Related code

- `go-proxy/cmd/server/main.go § applySocketFault` — the dispatch + wire writes (4595 in current revision).
- `go-proxy/cmd/server/main.go § fetchUpstreamBodyPrefix` — bounded upstream fetch for body_* faults.
- `go-proxy/cmd/server/main.go § isSocketFaultType` — the allowlist; keep in sync with the table above.
- `tests/characterization/modes/abort_test.go` — the test that consumes this contract. Driver for change validation.

## See also

- `.claude/standards/avplayer-quirks.md` — how AVPlayer reacts to specific failure shapes.
- `.claude/standards/hls-taxonomy.md` — taxonomy of HLS-level failures the player surfaces.
- `.claude/standards/harness-cli.md` — `harness fault add` flag reference.
