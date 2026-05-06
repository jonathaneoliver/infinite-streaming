# analytics

Sidecar analytics tier for InfiniteStream. Subscribes to the SSE session
stream emitted by `go-proxy`, writes session-state snapshots into
ClickHouse, and exposes them to Grafana (ad-hoc dashboards) and to
`testing.html` (historical replay mode).

The streaming app does not store anything itself — if `forwarder` is
down the live UI keeps working, archival just pauses until it restarts.

## Components

- `go-forwarder/` — standalone Go binary. Subscribes to
  `/api/sessions/stream`, dedupes by snapshot fingerprint, batches
  inserts into ClickHouse. Also serves the read-only HTTP API at
  `:8080` that nginx proxies as `/analytics/api/*`. All ClickHouse
  queries use parameterized `{name:Type}` placeholders — no string
  interpolation of user input.
- `clickhouse/init.d/01-schema.sql` — bootstrap schema. ClickHouse
  applies it on first start. One wide `session_snapshots` table with
  hot fields as typed columns and a `session_json` column for the long
  tail. 30-day TTL.
- `grafana/provisioning/` — datasource + starter dashboard, picked up
  automatically by Grafana.

## Local (Docker Compose)

`make run` brings up the analytics services alongside the main stack:

| Service     | Port  | Notes                                    |
|-------------|-------|------------------------------------------|
| ClickHouse  | 30123 | HTTP interface (native 9000 not exposed) |
| Grafana     | 30300 | Anonymous editor; pre-provisioned        |

Then:

- Grafana dashboard: <http://localhost:30300/d/is-session-analytics>
- Replay a session: <http://localhost:30000/testing.html?replay=1&session=SESSION_ID>

## k3d

`make deploy` and `make deploy-release` apply `k8s-analytics.yaml`
into the matching k3d cluster automatically — one analytics tier per
cluster (each cluster has its own ClickHouse PVC, forwarder, and
Grafana). The forwarder's SSE source is the same in-cluster URL in
both clusters (`http://infinite-streaming:30081/api/sessions/stream`)
because each cluster has exactly one `infinite-streaming` Service at
NodePort 30081.

## Replay mode

`testing.html?replay=1&session=<id>[&from=<rfc3339>][&to=<rfc3339>]`
fetches snapshots from `/analytics/api/snapshots`, then re-feeds them
through the same `applySessionsList` renderer the live stream uses.
`Date.now` is briefly patched per snapshot so chart point timestamps
line up with recorded `ts`.

## Re-provisioning the dashboard

Edits to `grafana/provisioning/dashboards/*.json` are picked up
automatically by the running Grafana container (every 30s). However,
adding/removing files in this directory after `docker compose up`
sometimes leaves the bind mount stale. If a new dashboard or datasource
isn't showing up:

```sh
docker compose up -d --force-recreate grafana
```

## Schema notes

- Hot columns (used by charts and dashboards) are typed: `buffer_depth_s`,
  `network_bitrate_mbps`, `dropped_frames`, `player_state`, etc.
- Everything else lives in `session_json`. Promote a column when a
  query starts using it frequently.
- `ORDER BY (session_id, ts)` makes per-session replay queries cheap.
- `TTL toDateTime(ts) + INTERVAL 30 DAY` enforces retention; tune via
  `ALTER TABLE ... MODIFY TTL`.

### Reading the RTT chart (issues #401, #404)

Two independent measurement schemes plotted on the same chart:

- **TCP_INFO family** (`client_rtt_*_ms`, purple lines) — what the
  *streaming TCP connection* actually experiences. Self-loaded:
  every sample includes the application's own queue contribution.
- **Path ping** (`client_path_ping_rtt_ms`, cyan line) — out-of-band
  ICMP echo from go-proxy → player_ip at 1 Hz, routed through a
  high-priority band (TC_PRIO_INTERACTIVE → tc band 0) inside the
  per-port shaping class. Sees the configured netem delay but jumps
  the bulk queue. The closest thing to "what the path could deliver
  if you weren't loading it."

#### The two probe paths through the kernel

Both probes share the same physical egress (eth0) and the same per-
port HTB class, but they take different *bands* through the prio
scheduler that sits inside the class as the leaf qdisc:

```
egress eth0
└── HTB root (1:1)
    └── HTB class 1:<port>     (rate-limited at configured Mbps)
        └── prio leaf qdisc     (3 bands, default priomap)
            ├── band 0 (1810:1)  ← PROBE LANE
            │   └── netem (configured delay/loss + 5% jitter)
            ├── band 1 (1810:2)  ← BULK DATA LANE
            │   └── netem (same delay/loss/jitter)
            └── band 2 (1810:3)  unused

filters at parent 1:0:
  ip sport <port>           → flowid 1:<port>     (TCP segments → bulk lane via priomap default)
  ip protocol icmp dst <ip> → flowid 1:<port>     (ICMP probes → probe lane via IP_TOS=0x10 → priomap band 0)
```

How a packet picks its band:

- **Bulk segment data**: leaves the proxy with `skb->priority = 0`
  (`TC_PRIO_BESTEFFORT`). Default prio priomap routes that to band 1
  (the middle band) → classid `1810:2` → bulk netem.
- **Path-ping ICMP**: socket has `IP_TOS = 0x10` set. Kernel's
  `rt_tos2priority` table maps that to `TC_PRIO_INTERACTIVE` (priority
  6). Default priomap routes that to band 0 → classid `1810:1` →
  probe netem.

Both bands carry **identical netem configuration** — `UpdateNetem`
writes the same `delay/loss/jitter` to all three bands in lockstep,
so the configured network conditions apply uniformly regardless of
priority. The only thing the prio scheduler changes is *queueing
order* when both bands have eligible packets: strict priority means
band 0 always drains first.

Net effect:

- **Bulk** sits in band 1's netem queue, gets the configured delay,
  then waits for HTB's rate-limit token. If the rate is full it
  queues behind earlier bulk packets too — bufferbloat.
- **Probe** sits in band 0's netem queue, gets the same configured
  delay, then jumps every queued bulk packet on the next HTB dequeue.
  Worst case it waits one MTU's serialization (≈ 12 ms at 1 Mbps,
  2.4 ms at 5 Mbps) for a bulk packet already on the wire.

So the ping line shows you *path + netem*, almost free of bufferbloat.
The TCP_INFO lines show you *path + netem + bufferbloat + ACK overhead*
— what bulk segment data really pays.

#### Why we ship both signals

They answer different questions. Either alone is misleading:

- **Just TCP_INFO** would conflate "the path is bad" with "I'm
  loading the path" — a player ABR algorithm seeing high RTT can't
  tell whether to back off or just wait for its own queue to drain.
  Lifetime min approximates the path floor but is sticky and slow
  to update during sustained load.
- **Just ping** would tell you nothing about what the streaming
  connection actually experiences — the ABR doesn't react to ICMP,
  it reacts to TCP behavior. A perfectly healthy ping line above a
  stalling player would be a useless diagnostic.

Together they decompose the latency budget: ping is the *physical
network's contribution* (path + configured delay), and the gap up
to TCP_INFO is the *application stack's contribution* (queueing
under throttle, delayed ACKs, receiver load, reverse-path queuing).
When bitrate drops or buffer drains, the chart tells you *which
component* moved — was it the path getting longer, or my own
queue piling up?

#### Field reference

- `client_rtt_ms` — kernel's smoothed RTT (RFC 6298 SRTT). Current
  path latency with EWMA, lags real changes by a fraction of a second.
- `client_rtt_max_ms` / `client_rtt_min_ms` — peak/trough of smoothed
  RTT *within the 1 s emit window*. Catches sub-second spikes the
  kernel's EWMA would otherwise mask.
- `client_rtt_min_lifetime_ms` — connection's *path floor*: the best
  RTT ever seen on that TCP connection. Sticky-low, never climbs back.
- `client_rtt_var_ms` — kernel's smoothed mean deviation (jitter).
- `client_rto_ms` — current retransmit timeout. Rises during a wedge
  while smoothed RTT flatlines because no fresh ACKs are coming back.
- `client_path_ping_rtt_ms` — ICMP echo round-trip, 1 Hz cadence.
  Zero / absent when ICMP is filtered on the path.

#### Why the ping line is *always* lower than TCP_INFO RTT

Even on a perfectly idle, unshaped LAN, expect TCP_INFO RTT to sit
above the ping line. Sources of inflation, all real, all working as
designed:

- **Delayed ACKs** — receiver TCP holds ACKs up to 40–200 ms (Linux/iOS
  default ~40 ms) to coalesce them. ICMP echo replies aren't subject
  to this; they go out immediately.
- **TCP_INFO is the smoothed SRTT** — exponentially averaged across
  recent ACKs, including ACKs generated under load. ICMP samples a
  single round-trip on the kernel fast path.
- **Receiver processing latency under load** — at multi-Mbps the
  player's TCP stack is busy demuxing segment data; ACK generation
  slips behind. ICMP echo handling skips userspace entirely.
- **Reverse-path queuing** — the player's egress (ACKs going *back*)
  has its own tiny outbound queue. ICMP replies skip it.

So `TCP_INFO − ping` on healthy unshaped LAN ≈ delayed-ACK + receiver
load + reverse queueing. This is the network stack's overhead, not
a fault.

#### Expected behavior under shaping

The two signals respond differently to the two shaping knobs (netem
delay and HTB rate limit). Useful test recipes:

| Action | TCP_INFO RTT | Path ping | Why |
|---|---|---|---|
| **No shaping** | `path + ACK overhead + receiver load` (typically 5–50 ms LAN) | `~path RTT` (sub-ms LAN) | Baseline. The ping line is the floor; TCP_INFO is everything else the stack adds. |
| **Set netem delay = 25 ms** | rises by ~25 ms (mean) | rises by ~25 ms (mean) | Both packets traverse the same per-band netem inside the HTB class. Matched movement. Per-packet variance is ±5 % of mean (~1 ms stddev at 25 ms — see jitter note below). |
| **Set throttle = 1 Mbps** (no netem) | climbs into bufferbloat range (often 100s of ms during downloads) | unchanged from baseline | Bulk segment data fills the HTB queue; each MTU waits for a rate token. The probe escapes via prio band 0 — at most one MTU's serialization (~12 ms at 1 Mbps for 1500 B). |
| **Throttle + netem combined** | `path + netem + bufferbloat + ACK overhead` (compounded) | `~path + netem + at-most-one-MTU` | Effects stack additively on bulk data. The ping line shows you what's *just* the configured delay so you can subtract bufferbloat by eye. |
| **Toggle shaping mid-stream** | step changes correlate visibly with bitrate / buffer drops on the chart above | flat through bandwidth changes; steps on netem changes only | Whole point of having both signals. Bitrate dropped because shaping was applied → both lines confirm in different ways. |
| **Drop packets via fault-injection** | smoothed RTT eventually flatlines (no fresh ACKs); `client_rto_ms` climbs as kernel doubles its timeout | `0` / gap (echo replies dropped too) | RTO − RTT divergence is the canonical wedge indicator. |

Reading the chart in one sentence:
**ping = network's contribution; (TCP_INFO − ping) = stack + bufferbloat
contribution.**

#### A note on jitter

`UpdateNetem` adds a fixed 5 % normal-distributed jitter (`stddev =
delay/20`). For configured 25 ms delay: stddev ≈ 1 ms, so ~99.7 % of
per-packet delays land in [22, 28] ms. The ping per-window min sits
at the configured floor; per-window max shows the tight scatter above.
Configured delays ≤19 ms get zero jitter (integer divide rounds to 0)
— fine for low-RTT testing where any noise would dominate the signal.

If a future test needs higher jitter (ABR resilience to RTT scatter,
out-of-order arrivals via wider Gaussian draws), add a separate
`jitter_ms` parameter to the shaping API rather than re-deriving from
the delay value. The 5 % default is tuned for "I configured 25 ms,
the chart should read ~25 ms," not for stress-testing variance.

#### Reading the RTO line (wedge detection)

`client_rto_ms` is hidden by default — toggle it on from the chart
legend when you suspect a wedge. RTO answers a different question
than the rest of the chart: not *how long does a round-trip take*
but *how long is the kernel willing to wait for one before giving
up and retransmitting*.

What it is. RTO is the kernel's **retransmission timeout** for the
TCP connection. After sending a segment, the sender starts a timer;
if no ACK arrives before the timer expires, the segment is
retransmitted and the timer doubles. RFC 6298 sets the steady-state
value to roughly `SRTT + 4 × RTTVAR` (smoothed RTT plus four times
its smoothed deviation), with a kernel floor of 200 ms and ceiling
of 120 s. So on a quiet, healthy connection RTO sits a small
multiple of RTT above the smoothed-RTT line — *not* zero.

Why it's the wedge canary. When ACKs stop flowing entirely (transport
fault, dropped packets, broken middlebox), `tcpi_rtt` flatlines —
no fresh ACK round-trips means no new samples to update the EWMA.
Looking at the smoothed-RTT line alone, you can't tell whether the
connection is healthy-and-idle or wedged-and-silent. RTO has its
own state machine driven by the kernel's retransmission timer, not
by ACK arrivals: every retry doubles it (`200ms → 400ms → 800ms →
1.6s → 3.2s → 6.4s → 12.8s → 25.6s → 51.2s → 102.4s`, capped at
the kernel max). So when the connection wedges:

```
RTT (purple) ──flat───────────────────────────────  ← no ACKs, no fresh samples
RTO  (red) ───────⌐──┘─⌐──┘──⌐──┘──⌐────┘────  ← kernel doubling on each retry
```

The growing gap between the two is the unambiguous "kernel suspects
this connection is stalling" signal. It appears within seconds of
the wedge starting — much faster than a stall on the bitrate chart
above (which only triggers after the player's buffer drains).

What recovery looks like. Once ACKs start flowing again (fault
cleared, retry succeeds), the kernel resets RTO to `SRTT + 4×RTTVAR`
on the very next ACK. The red line snaps back down; the purple
smoothed-RTT line resumes updating. So a recovered wedge is visible
as a sawtooth-like RTO climb followed by an instant drop, with the
RTT line resuming its normal track.

Useful pairing. RTO + the path-ping line together disambiguate the
wedge cause:

| RTT line | RTO line | Path ping | What it means |
|---|---|---|---|
| flat | climbing | also gone (ICMP filtered/dropped) | Wedge or transport fault — entire path is dead from proxy's view. |
| flat | climbing | still arriving normally | Wedge is TCP-specific — ICMP gets through but TCP is stuck. Likely middlebox dropping the connection or a broken player TCP stack, not a network outage. |
| climbing slowly | tracking RTT (small multiple above) | climbing the same | Genuine path latency increase, not a wedge — RTO is just following healthy RTT growth. |

## Securing a WAN-exposed deployment

Default docker-compose binds ClickHouse to `127.0.0.1` (host-only) and
keeps Grafana off the host network entirely — Grafana is reachable only
via nginx at `/grafana/`. The dashboard, `/analytics/api/`, and
`/grafana/` routes can be gated with HTTP Basic auth so only known
clients can read or write through nginx. Player apps (Apple, Roku,
AndroidTV) keep working without credentials because their endpoints
(`/api/sessions/stream`, `/api/version`, segment fetches under
`/go-live/`) stay public.

To enable:

```sh
# 1. Generate an htpasswd file (bcrypt; pick your own user/pass)
docker run --rm httpd:alpine htpasswd -nbB myuser mypassword > ./htpasswd

# 2. Mount it into the go-server container and tell launch.sh where it is
#    (docker-compose.override.yml or similar):
services:
  go-server:
    environment:
      - INFINITE_STREAM_AUTH_HTPASSWD=/etc/nginx/auth.htpasswd
    volumes:
      - ./htpasswd:/etc/nginx/auth.htpasswd:ro
```

When `INFINITE_STREAM_AUTH_HTPASSWD` is unset (the default), auth is
disabled — same behaviour as before. With it set, the dashboard pages,
`/analytics/api/*`, and `/grafana/*` all return 401 without credentials.

### k3d deployment (lenovo)

Both clusters use the same Deployment name (`infinite-streaming`); pick the right kubeconfig per cluster.

```sh
# Pick a cluster (dev or release)
export KUBECONFIG=~/.config/k3d/smashing-release-kubeconfig.yaml
# (or smashing-dev-kubeconfig.yaml for the dev cluster)

# Generate htpasswd
docker run --rm httpd:alpine htpasswd -nbB myuser 'mypass' > /tmp/htpasswd

# Create a Secret holding the file (per-cluster)
kubectl create secret generic infinite-streaming-auth \
  --from-file=htpasswd=/tmp/htpasswd

# Patch the deployment (same name in both clusters)
kubectl patch deployment infinite-streaming --patch '
spec:
  template:
    spec:
      containers:
      - name: go-server
        env:
        - name: INFINITE_STREAM_AUTH_HTPASSWD
          value: /etc/secret/htpasswd
        volumeMounts:
        - name: auth-secret
          mountPath: /etc/secret
          readOnly: true
      volumes:
      - name: auth-secret
        secret:
          secretName: infinite-streaming-auth
'

# Verify
kubectl rollout status deployment/infinite-streaming
curl -s -o /dev/null -w '%{http_code}\n' http://lenovo.local:30000/dashboard/dashboard.html
# → 401
curl -s -o /dev/null -w '%{http_code}\n' -u myuser:mypass http://lenovo.local:30000/dashboard/dashboard.html
# → 200
```

Rotate the password by re-creating the secret and restarting the rollout (in the same cluster context):

```sh
docker run --rm httpd:alpine htpasswd -nbB myuser 'newpass' > /tmp/htpasswd
kubectl create secret generic infinite-streaming-auth --from-file=htpasswd=/tmp/htpasswd \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/infinite-streaming
```

Disable auth without removing the secret by setting the env var to an
empty string:

```sh
kubectl set env deployment/infinite-streaming INFINITE_STREAM_AUTH_HTPASSWD=
```
