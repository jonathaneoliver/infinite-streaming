# Architecture

InfiniteStream runs as a **single Docker container** with four cooperating processes. A host-mounted volume holds source media and encoded outputs. nginx fronts everything.

## Processes

| Process | Internal port | Responsibility |
|---|---|---|
| `go-live` | 8010 | Dynamic HLS/DASH manifest generation (LL + 2s + 6s segment variants) from short VOD content |
| `go-upload` | 8003 | Upload API, encoding job orchestration, content discovery |
| `go-proxy` | 30081 (base) + per-session ports | Failure-injection proxy + traffic shaping; holds the per-session state map in-process and broadcasts changes via SSE |
| `nginx` | 30000 | Static dashboard + routing to the three Go services |

All three Go services + nginx run in the same container. The only optional external dependency at runtime is a Cloudflare Worker for client-side server discovery (see [Server discovery](#server-discovery) below) — disable it by leaving `INFINITE_STREAM_RENDEZVOUS_URL` unset.

## High-level flow

```
                   ┌───────────────────────┐
                   │       browser         │
                   └─────────┬─────────────┘
                             │
                      :30000 (nginx)
                             │
        ┌────────────────────┼──────────────────────────────┐
        │                    │                              │
    /dashboard/         /go-live/                      /api/...
     static files     (manifests + segments)      upload / sessions / nftables
                             │                              │
                   ┌─────────┼─────────┐            ┌───────┼────────┐
                   │                   │            │                │
             go-live:8010       nginx serves     go-upload       go-proxy
           (manifest gen)     segments directly   :8003           :30081
                                  from disk                (+ session ports
                                                            30181..30881)
                             │
                 /media/dynamic_content/{content}/...   (host-mounted volume)
```

When a player requests a manifest, nginx routes to `go-live`, which spawns a **per-content worker** on first request. The worker generates every segment variant (LL, 2s, 6s for both HLS and DASH) from a shared clock, so cross-variant comparisons stay aligned. Workers shut down after an idle timeout.

Segment files (`.m4s`, `.ts`, `.mp4`) are **served directly by nginx** from the host-mounted output directory — go-live never sees segment traffic. Only manifest generation is dynamic.

## nginx routing

Defined in [`nginx-content.conf.template`](../docker/nginx-content.conf.template). Summary:

| Path | Target | Notes |
|---|---|---|
| `/go-live/**/*.m3u8`, `*.mpd` | `go-live:8010` | Dynamic manifest generation |
| `/go-live/**/*.{m4s,ts,mp4,m4a,cmfv,cmfa,webm,m4v,aac,webvtt}` | nginx alias | Direct disk serve, `expires 1y` |
| `/go-live/` (other) | `go-live:8010` | Status/API endpoints |
| `/api/content`, `/api/sources`, `/api/upload`, `/api/jobs`, `/api/setup` | `go-upload:8003` | |
| `/api/sessions*`, `/api/session/*`, `/api/session-group/*`, `/api/failure-settings/*`, `/api/nftables/*`, `/api/clear-sessions`, `/api/external-ips`, `/api/version` | `go-proxy:30081` | |
| `/api/sessions/stream` | `go-proxy:30081` | SSE — buffering disabled, 1h read timeout |
| `/dashboard/` | `/content/dashboard/` | Static files |
| `/testing/` | `/content/testing/` | Static files |
| `/` | `/content/` | Static root |

Manifest responses carry `no-cache, no-store, must-revalidate`. Segments are immutable (`expires 1y`). Uploads bypass buffering and carry 300s timeouts for large files.

## Per-session proxy

The testing dashboard gives each browser session a dedicated `go-proxy` port. When the player loads `testing-session.html?player_id=<uuid>`, the proxy allocates the session a port in `30181..30881` (internal) — the browser talks to that port for stream requests, not the shared `30081`. All failure injection and traffic shaping applied to that session are scoped to its port.

Internal / external port mapping:

| Environment | External UI | External session ports | Internal |
|---|---|---|---|
| Docker Compose | 30000 | 30181..30881 | same |
| k3d release | 30000 | 30181..30881 | same |
| k3d dev | 40000 | 40181..40881 | 30181..30881 |

For k3d, the cluster's `--port` flag does the host→cluster remapping (e.g. host `:40081` → in-cluster NodePort `:30081` for the dev cluster). Inside both clusters the Service NodePorts are stable (30000-30881); the host-port mismatch is purely an external-port concern. go-proxy reads `EXTERNAL_PORT_BASE`, `INTERNAL_PORT_BASE`, and `PORT_RANGE_COUNT` env vars so the per-session URLs it advertises to clients use the right host port for the cluster's external mapping.

See [`docs/FAULT_INJECTION.md`](FAULT_INJECTION.md) for what go-proxy can do to a session; [`docs/API.md`](API.md) for the full endpoint surface.

## Content layout on disk

Host volume mounted at `/media` inside the container:

```
/media/
├── originals/                      # source files (uploaded or copied in)
│   └── my-show.mp4
├── dynamic_content/                # encoded ABR output (served as segments)
│   └── my-show_p200_h264/
│       ├── video/240p/…m4s
│       ├── video/480p/…m4s
│       ├── video/720p/…m4s
│       ├── audio/…m4s
│       └── manifest.json           # consumed by go-live as the source of truth
└── certs/                          # optional TLS certs (auto-generated if missing)
    ├── localhost.pem
    └── localhost-key.pem
```

Live manifests generated on the fly live in tmpfs at `/content/go-live/{content}/…` and are not persisted.

## Subprocess boundaries

go-live only **reads** `/media/dynamic_content`. go-upload **writes** to both `/media/originals` and `/media/dynamic_content` and shells out to ffmpeg + shaka-packager for encoding jobs. go-proxy is stateless for content — it just proxies requests to nginx's internal port and overlays fault behavior.

## Wire metric implementation

go-proxy's throughput metrics (`mbps_shaper_rate`, `mbps_shaper_avg`, `mbps_transfer_rate`, `mbps_transfer_complete`) are read directly from the kernel rather than via subprocess calls to `tc`:

- TC class byte/backlog counters are read via netlink (`vishvananda/netlink`) — no subprocess fork per poll.
- Per-port TC stats are cached with a 5 ms TTL to deduplicate concurrent readers.
- Only one `awaitSocketDrain` goroutine runs per port at a time (singleton guard).
- TC counters include packet-level transport/application overhead (TCP/IP + TLS/HTTP headers) but **not** physical link-layer overhead (Ethernet preamble / IFG / FCS).
- **Docker Desktop (macOS):** TC shaping works with `--cap-add NET_ADMIN` but the VM translation layer makes TC stats polling (100ms interval per session) significantly more expensive than on native Linux. On an M5 MacBook Pro, even a single session causes noticeable fan spin-up. This is inherent to Docker Desktop's Linux VM architecture, not a code issue. For sustained shaping tests, use a native Linux host.

Semantics and expected behaviour of the metrics themselves (what each series means, how they relate to the configured limit and the player's own estimate) are documented in [`README.md`'s Metrics reference](../README.md#metrics-reference).

## Dashboard

Static HTML/JS/CSS served by nginx from `content/dashboard/`:

| Page | Purpose |
|---|---|
| `dashboard.html` | Main entry, navigation |
| `playback.html` | Single-stream playback with protocol/codec/segment/engine selectors |
| `quartet.html` | Four-panel side-by-side comparison |
| `grid.html` | Mosaic view with filters |
| `testing-session.html` | Per-session failure injection and player characterization |
| `go-monitor.html` | Live worker status, idle timeouts, tick timings |
| `upload.html` | Upload + encoding jobs |

Global selection (content, URL, protocol, codec, segment) is kept in `localStorage` under `ismSelected*` keys so it persists across pages.

## Deployment modes

| Mode | How | UI | Notes |
|---|---|---|---|
| Docker Compose | `make run` or `docker compose up -d` | `localhost:30000` | Simplest, single-host |
| k3d release | `make deploy-release` | `$K3S_HOST:30000` | Independent k3d cluster, api `:6544`, 30x port range |
| k3d dev | `make deploy` | `$K3S_HOST:40000` | Independent k3d cluster, api `:6543`, 40x port range — coexists with release |
| GHCR compose | Pull `ghcr.io/jonathaneoliver/infinite-streaming:<tag>` | `localhost:30000` | No local build |

All modes mount the same content layout. See [`README.md`](../README.md#quick-start) for commands and [`docs/TROUBLESHOOTING.md`](TROUBLESHOOTING.md) for common operational issues.

## Server discovery

The native client apps (iOS/tvOS, Android TV, Roku) need a way to *find* the server URL on first launch — they can't ship hardcoded addresses. InfiniteStream uses a small **Cloudflare Worker + KV** acting as a public rendezvous, plus a server-side announce loop that publishes its URL there. Clients query the rendezvous and only see servers that share their public IP (the rendezvous filters by `CF-Connecting-IP`), giving same-WAN auto-discovery without LAN multicast.

> **Why not mDNS / Bonjour?** This was the first thing we tried (`_infinitestream._tcp` advertised via `github.com/grandcat/zeroconf` from `go-upload`). Docker's default bridge network filters multicast, so the advertisement never escapes the container — `dns-sd -B` on the host found nothing. Host-network mode is fragile across Linux/macOS/Windows and isn't available on every deployment target (k3d, Docker Desktop). The Cloudflare same-public-IP check delivers the same "servers on my network" semantics without needing multicast to survive the container boundary, so the mDNS advertiser was removed.

```
                                    Cloudflare Worker
                                  (pair-rendezvous + KV)
                                  ┌──────────────────────┐
            POST /announce        │                      │       GET /announce
   ┌──────► server_id, url ──────►│ keyed by hashed IP   │◄────── (same WAN ─►)
   │                              │                      │       returns matching
   │  POST /api/announce-now      │ POST /pair?code=XXX  │       servers
   │  ◄─ dashboard "Server Info"  │ GET  /pair?code=XXX  │
   │                              └──────────────────────┘                │
┌──┴────────┐                                                ┌────────────┴────┐
│  server   │                                                │   client app    │
│ go-upload │                                                │ iOS/tvOS/AndTV  │
│ announce  │                                                │ "+ Add server"  │
│   loop    │                                                │  "Pair…"        │
└───────────┘                                                └─────────────────┘
```

**Components**

| Piece | Path | Role |
|---|---|---|
| Worker | [`cloudflare/pair-rendezvous/`](../cloudflare/pair-rendezvous/) | `POST/GET/DELETE /pair?code=` for code-based pairing; `POST/GET /announce` for same-WAN discovery; standalone HTML pair page at `/` |
| Server announce | [`go-upload/internal/announce/`](../go-upload/internal/announce/) | Heartbeats `{server_id, url, label}` on boot, every 12h, and on demand. Persists `server_id` at `<data_dir>/server_id`. Opt-in via `INFINITE_STREAM_ANNOUNCE_URL`. |
| `POST /api/announce-now` | [`go-upload/internal/api/handler.go`](../go-upload/internal/api/handler.go) | Dashboard's Server Info modal pokes this on open so the user can recover from a missed boot announce. Coalesced. |
| Dashboard pair widget | [`content/shared/shared-nav.js`](../content/shared/shared-nav.js) | Server Info modal: shows host:port + QR, "Pair with code" form, LAN-only-URL warning, cloud-discovery callout. |
| Client discovery | iOS `RendezvousService.swift`, Android `RendezvousService.java` | `discoverServers()` calls `GET /announce`. Pairing UI lists discovered servers tap-to-add and falls back to a 6-char pairing code. |

**Cadence and cost.** Each server costs ~2 KV writes/day at the default cadence (12h heartbeat, 24h TTL, plus boot + on-demand from Server Info). Cloudflare KV's free plan allows 1,000 writes/day across the account.

**Same-WAN check.** The rendezvous derives a stable hash from `CF-Connecting-IP`; entries are stored under `announce:<ip-hash>:<server_id>`. A `GET /announce` from the client only lists entries with the caller's same IP hash, so the discovery list is automatically scoped to "servers visible from your public IP". Code-based pairing uses the same check (different public IP → 403, with `RENDEZVOUS_ALLOW_CROSS_NETWORK=1` to opt out).

**Distinct `server_id` per deployment.** Multiple deployments sharing a data directory (typical of dev + release k3d clusters on the same host that both mount `/media`) end up with the same persisted `<data_dir>/server_id` and overwrite each other on the rendezvous. Set `INFINITE_STREAM_SERVER_ID` explicitly per deployment (e.g. `infinite-streaming-dev` / `infinite-streaming-release`) to make them appear as independent entries in the announce list.

**Cleartext HTTP and ATS.** The server defaults to plain HTTP on every listening port. iOS/tvOS' App Transport Security blocks plain HTTP to public hostnames unless the domain is in the app's `NSExceptionDomains`; the iOS/tvOS Info.plists in this repo include an exception for `infinitestreaming.jeoliver.com` so the upstream public deployment is reachable. Forks that ship apps pointing at a different public-HTTP host need to add their own exception (or — better — terminate TLS at the server so HTTPS works without exceptions; the `certs-vol` mount in the k3d manifests is the hook for that). Android has `usesCleartextTraffic="true"` so it isn't affected.

See the [Server discovery section in the README](../README.md#server-discovery) for the user-facing summary.
