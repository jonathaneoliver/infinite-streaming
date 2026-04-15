# Fault Injection Reference

InfiniteStream can inject faults at three layers: HTTP (application-layer errors and socket misbehavior), transport (nftables DROP/REJECT), and network (`tc` rate limiting, delay, packet loss). All faults are per-session: each browser testing session binds to a dedicated proxy port via `player_id`, so concurrent testers don't collide.

Faults are configured in the **Testing Session** UI, or directly via the HTTP API below.

## Layers

```
player ──► proxy port (per session) ──► upstream
             │
             ├── tc qdisc (rate, delay, loss)      ← network shaping
             ├── nftables rules (DROP/REJECT)      ← transport faults
             └── HTTP handler                      ← HTTP faults
                  ├── status-code injection
                  ├── socket misbehavior
                  └── payload corruption
```

Layers compose. A session can have, for example, 4 Mbps shaping + periodic DROP windows + a 404 injected on every 10th segment.

## HTTP faults

### Status-code faults

Return a specific HTTP status instead of proxying upstream:

| Type | Response |
|---|---|
| `404` | 404 Not Found |
| `403` | 403 Forbidden |
| `500` | 500 Internal Server Error |
| `timeout` | 504 Gateway Timeout |
| `connection_refused` | 503 Service Unavailable |
| `dns_failure` | 502 Bad Gateway |
| `rate_limiting` | 429 Too Many Requests |

The upstream request is not made when one of these fires — the network log marks the entry as **Injected by proxy**.

### Socket misbehavior

Break the TCP connection at specific points in the response lifecycle:

| Type | Phase | Action |
|---|---|---|
| `request_connect_reset` | before headers | TCP RST |
| `request_connect_hang` | before headers | black-hole, connection hangs (~90s) |
| `request_connect_delayed` | before headers | 12s pause, then close |
| `request_first_byte_reset` | after headers | RST |
| `request_first_byte_hang` | after headers | drop, no body |
| `request_first_byte_delayed` | after headers | 12s pause, then close |
| `request_body_reset` | after 64 KB of body | RST |
| `request_body_hang` | after 64 KB of body | drop |
| `request_body_delayed` | after 64 KB of body | 12s pause, then close |

Constants (hard-coded): mid-body threshold = 64 KB, hang duration = 90s, delay duration = 12s.

### Payload corruption

| Type | Behavior |
|---|---|
| `corrupted` | Fetch the full segment from upstream, but stream **zeros** of the same length to the client. Timing and byte counts reflect a real transfer; the network log shows timing bars *and* a `corruption` fault badge. Segment-only. |

### Request targeting

The HTTP fault configuration is split across three request kinds, each with its own fault type and URL allowlist:

| Kind | Field prefix | Notes |
|---|---|---|
| Media segments | `segment_*` | Supports all HTTP fault types including `corrupted` |
| Variant playlists (HLS) / adaptation set manifests (DASH) | `manifest_*` | |
| Master playlists / top-level manifests | `master_manifest_*` | |

`{prefix}_failure_urls` is an allowlist of URLs (or the tokens `All` / `audio`) that the fault applies to. An empty list means "all requests of this kind."

## Transport faults (nftables)

Port-wide DROP or REJECT applied via nftables (table `go_proxy_faults`, chain `transport_faults`):

| Type | Effect |
|---|---|
| `drop` | Packets are silently dropped |
| `reject` | TCP RST returned to client |
| `none` | No transport fault |

Transport faults apply to the entire session port — every request through that port is affected for the duration of the active window.

## Network shaping (`tc`)

Per-session port, via `tc` qdiscs:

| Field | Mechanism | Units |
|---|---|---|
| `nftables_bandwidth_mbps` | TBF rate ceiling | Mbps |
| `nftables_delay_ms` | netem delay | ms |
| `nftables_packet_loss` | netem loss | percent |

Bandwidth can also be driven by a **step pattern** (`nftables_pattern_*`), where each step has a `rate_mbps` and `duration_seconds`. Used by the Player Characterization ABR ramp feature.

Linux-only. The capabilities endpoint (`GET /api/nftables/capabilities`) reports whether shaping is available.

## Timing model

Every fault configuration has the same six-field shape. Field names are prefixed by request kind (`segment_`, `manifest_`, `master_manifest_`, `transport_`):

| Field | Meaning |
|---|---|
| `{prefix}_failure_type` | Which fault to inject (e.g. `404`, `drop`, `none`) |
| `{prefix}_failure_mode` | How to count failures (see modes below) |
| `{prefix}_consecutive_failures` | Width of the fault window |
| `{prefix}_failure_frequency` | Gap between fault windows |
| `{prefix}_consecutive_units` | Unit for `consecutive` (`requests`, `seconds`, `packets`) |
| `{prefix}_frequency_units` | Unit for `frequency` (`requests`, `seconds`) |

### Modes

| Mode | `consecutive` | `frequency` | Semantics |
|---|---|---|---|
| `requests` | # of requests | # of requests | Fail `N` requests in a row, then pass `M` requests, repeat |
| `seconds` | seconds | seconds | Fail for `N` seconds, then clear for `M` seconds, repeat |
| `failures_per_seconds` | # of failures | seconds | Emit up to `N` failures per `M`-second window |
| `failures_per_packets` | # of packets | seconds | Transport-only: drop `N` packets per `M`-second window |

The handler resets its internal counters once the frequency window expires, so the pattern is periodic and repeatable across runs.

## Session grouping

Sessions can be **grouped** (link/unlink/merge) so that changes to one member propagate to all. Propagated state:

- All HTTP fault fields (`segment_*`, `manifest_*`, `master_manifest_*`)
- Transport fault fields (`transport_*`)
- All network shaping (`nftables_*`, including patterns)

Limit: 10 sessions per group.

Endpoints:
- `POST /api/session-group/link` — body `{session_ids: [...], group_id?: "..."}` (auto-generates if omitted)
- `POST /api/session-group/unlink` — body `{session_id: "..."}`
- `GET /api/session-group/{groupId}` — list members

## API

### Session patch (primary fault-config endpoint)

```
PATCH /api/session/{id}
```

Body:

```json
{
  "set": {
    "segment_failure_type": "404",
    "segment_failure_mode": "requests",
    "segment_consecutive_failures": 1,
    "segment_failure_frequency": 10,
    "segment_failure_urls": ["All"],

    "manifest_failure_type": "none",
    "master_manifest_failure_type": "none",

    "transport_failure_type": "drop",
    "transport_failure_mode": "failures_per_seconds",
    "transport_consecutive_failures": 5,
    "transport_failure_frequency": 30,

    "nftables_bandwidth_mbps": 4.0,
    "nftables_delay_ms": 0,
    "nftables_packet_loss": 0.0,
    "nftables_pattern_enabled": false,
    "nftables_pattern_steps": [
      { "rate_mbps": 8.0, "duration_seconds": 10 },
      { "rate_mbps": 2.0, "duration_seconds": 10 }
    ]
  },
  "fields": ["segment_failure_type", "transport_failure_type"],
  "base_revision": "<control-revision-token>"
}
```

- `set` — fields to update
- `fields` — explicit list of changed keys (the server uses this to detect conflicts)
- `base_revision` — control-revision token from the last observed session state; the server rejects the patch with a 409 if state has moved on

### Shaping shortcuts

| Method | Path | Body |
|---|---|---|
| `POST` | `/api/nftables/bandwidth/{port}` | `{rate_mbps: N}` |
| `POST` | `/api/nftables/loss/{port}` | `{percent: N}` |
| `POST` | `/api/nftables/shape/{port}` | full shape config |
| `POST` | `/api/nftables/pattern/{port}` | step-pattern config |
| `GET` | `/api/nftables/port/{port}` | current state |
| `GET` | `/api/nftables/status` | all ports |
| `GET` | `/api/nftables/capabilities` | what the host supports |

### Live updates

`GET /api/sessions/stream` is a Server-Sent Events stream of session state changes. Each event carries the full session payload, including the latest `control_revision` token to use on the next `PATCH`.

## Observability

Every proxied request is recorded in a per-session **network log** (ring buffer, 200 entries) with timing (DNS, Connect, TLS, TTFB, Transfer), byte counts, and fault metadata. Retrieve via:

```
GET /api/session/{id}/network
```

Rows include `faulted: true` + `fault_category` (`http`, `socket`, `corruption`, `transport`) when a fault was applied.

The dashboard's **Network Log** section renders this as a Chrome-DevTools-style waterfall with fault highlighting — useful to confirm that an injected schedule actually landed where expected.
