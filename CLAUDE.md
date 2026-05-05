# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Product Behavior Source of Truth

Before making UI or product-behavior changes, read `PRD.md` and align implementation to it.

## Build & Run Commands

```bash
# Build Docker image
make build

# Run (Docker Compose)
make run         # docker compose up -d
make stop

# Shell into running container
make shell

# One-time bootstrap — installs k3d on $K3S_SSH_HOST and creates the
# `dev` and `release` clusters with their host port mappings.
make k3d-bootstrap

# Deploy to k3d (dev cluster — host ports 40000/40081/40181-40881)
make deploy

# Deploy to k3d (release cluster — host ports 30000/30081/30181-30881)
make deploy-release

# Wipe a single cluster (k3d cluster delete) for clean reinstall
make teardown-dev
make teardown-release
```

UI is available at `http://localhost:30000/` (Docker Compose) or `http://$K3S_HOST:30000/` (k3d release) / `http://$K3S_HOST:40000/` (k3d dev).

## Testing

Tests are integration tests that require a running server. Navigate to the test directory first:

```bash
cd tests/integration
pip install -r requirements.txt

# Run smoke tests (fastest)
pytest -m smoke

# Run specific categories
pytest -m http
pytest -m "not slow"
pytest -k test_name

# Run player ABR characterization tests
pytest test_player_characterization_pytest.py -m abrchar -v \
  --host $K3S_HOST --scheme http --api-port 40000 --hls-port 40081

# Run against local Docker Compose
pytest test_player_characterization_pytest.py -m abrchar -v \
  --host localhost --scheme http --api-port 30000 --hls-port 30081

# Available abrchar test modes: smooth, steps, transient-shock, startup-caps,
#   downshift-severity, hysteresis-gap, emergency-downshift
pytest test_player_characterization_pytest.py -m abrchar -v \
  --abrchar-test-mode steps --abrchar-repeat-count 10
```

Default server target for tests is `$K3S_HOST:30000/30081`. Override with `--host`, `--api-port`, `--hls-port`, or `--api-base`.

## Code Style

- **Go**: standard `gofmt`
- **Shell**: POSIX-friendly where possible
- **JS/CSS/HTML**: keep changes explicit and readable

## Architecture

### Services (all run inside a single Docker container)

| Service | Port | Role |
|---------|------|------|
| `go-live` | 8010 | LL-HLS + LL-DASH generator (2s/6s/LL variants) |
| `go-upload` | 8003 | Upload API, encoding job orchestration, content discovery |
| `go-proxy` | 30081 | Per-session testing proxy — failure injection, traffic shaping (nftables), SSE session stream, in-process session-state map |
| nginx | 30000 | Routing, static dashboard |

Plus the optional **analytics sidecar** (separate compose services): `clickhouse` (archive, 30-day TTL), `forwarder` (SSE → ClickHouse + read API), `grafana` (provisioned dashboards). All under [`analytics/`](analytics/). The live streaming path is independent of the sidecar — if the forwarder dies, the live UI keeps working, archival just pauses.

### nginx routing (`nginx-content.conf.template`)

- `/go-live/{content}/*.m3u8` and `*.mpd` → proxied to `go-live:8010` for dynamic generation
- `/go-live/{content}/*.m4s`, `*.ts`, `*.mp4`, etc. → served directly from the output directory by nginx
- `/api/content`, `/api/jobs`, `/api/sources`, `/api/upload`, `/api/setup` → `go-upload:8003`
- `/api/sessions*`, `/api/session/*`, `/api/failure-settings/*`, `/api/session-group/*`, `/api/nftables/*` → `go-proxy:30081`
- `/analytics/api/*` → `forwarder:8080` (read-only ClickHouse query proxy + bundle ZIP streaming)
- `/grafana/*` → `grafana:3000` (with `GF_SERVER_SERVE_FROM_SUB_PATH=true`)
- `/dashboard/` → `content/dashboard/` static files
- HTTP Basic auth on `/dashboard/`, `/analytics/api/`, `/grafana/` is opt-in via `INFINITE_STREAM_AUTH_HTPASSWD` env var; player-app endpoints stay public.

### go-live (`go-live/`)

Single binary that manages all LL-HLS and LL-DASH generation. Internal packages:
- `internal/manager` — per-content worker lifecycle (spawn/stop/status)
- `internal/api` — HTTP handlers
- `internal/dash`, `internal/generator`, `internal/parser` — manifest generation logic

On the first request for a content item, go-live spawns a per-content worker. Workers generate all manifests (LL, 2s, 6s) in sync on a shared clock. Workers shut down after an idle timeout.

### go-upload (`go-upload/`)

Handles uploads, encoding job orchestration via ffmpeg/shaka-packager, and content discovery. Internal packages:
- `internal/api`, `internal/app`, `internal/config`, `internal/store`, `internal/util`

Content is stored at `/media/dynamic_content/{content}/` inside the container (host-mounted volume).

### go-proxy (`go-proxy/`)

Per-session failure injection and traffic shaping proxy. Each testing session gets a dedicated port (`30181`–`30881`). Controls:
- HTTP failure injection (errors, hung, corrupted responses)
- Transport faults via nftables (DROP/REJECT)
- Network rate limiting via `tc`
- Server-sent events (SSE) stream at `/api/sessions/stream` for real-time session state

The `player_id` query param on `testing-session.html` binds the browser session to a proxy port.

### Dashboard (`content/dashboard/` + `content/shared/`)

Static HTML/JS/CSS pages served by nginx:
- `dashboard.html` — main entry
- `testing-session.html` + `testing-session-ui.js` — failure injection UI and player characterization
- `sessions.html` — picker over archived sessions; per-row bad-event chips, bundle download
- `session-viewer.html` — replays one archived play through the same charts as the live page
- `player-characterization.js` — ABR ramp sweep logic
- `grid.html`, `quartet.html`, `playback.html`, `go-monitor.html`, etc.

Shared modules (`content/shared/`):
- `session-shell.js` — chart rendering core (Chart.js + vis-timeline) used by both live and replay
- `session-replay.js` — brush + events dropdown + rail markers for the replay page
- `session-live.js`, `play-id.js` — live/replay glue

### Analytics sidecar (`analytics/`)

- `clickhouse/init.d/01-schema.sql` — `session_snapshots` + `network_requests` schema, 30-day TTL.
- `go-forwarder/` — Go binary that subscribes to `/api/sessions/stream`, batches inserts into ClickHouse, and serves `/api/sessions`, `/api/snapshots`, `/api/session_events`, `/api/network_requests`, `/api/session_heatmap`, `/api/session_bundle` (ZIP) read-only via parameterized `{name:Type}` SQL placeholders.
- `grafana/provisioning/` — dashboards-as-code; reload with `make analytics-update`.
- See [`analytics/README.md`](analytics/README.md) for ops, schema, and the WAN-deploy auth runbook.

Make targets: `make analytics-rebuild-forwarder` (rebuild + recreate forwarder only, no go-server restart), `make analytics-update` (Grafana reload), `make analytics-migrate SQL='ALTER …'` (one-line schema change).

### Encoding Pipeline (`generate_abr/`)

- `create_abr_ladder.sh` — main pipeline (ffmpeg + shaka-packager)
- `create_hls_manifests.py` — HLS manifest generation helper
- `convert_to_segmentlist.py` — DASH SegmentTemplate → SegmentList conversion

Defaults: 6s segment, 200ms partial, 1s GOP.

### Client Apps

- `apple/InfiniteStreamPlayer/` — SwiftUI iOS/tvOS app
- `roku/InfiniteStreamPlayer/` — BrightScript Roku channel

## GitHub Workflow

When creating issues/PRs/comments with `gh`, pass the body using a heredoc or `--body-file`; do not use `\n` in quoted strings.

## Deployment Environments

| Environment | UI | HLS/Proxy | Cluster |
|---|---|---|---|
| Docker Compose | `localhost:30000` | `localhost:30081` | n/a |
| k3d release | `$K3S_HOST:30000` | `$K3S_HOST:30081` | `release` (api `:6544`) |
| k3d dev | `$K3S_HOST:40000` | `$K3S_HOST:40081` | `dev` (api `:6543`) |

The two k3d clusters run as Docker containers on `$K3S_SSH_HOST`, share the host's local image registry (`$K3S_REGISTRY`, HTTP-only), and share nothing else — separate kubeconfigs, separate Services, separate ClickHouse PVCs, separate Grafana state. Per-cluster kubeconfigs live at `~/.config/k3d/smashing-{dev,release}-kubeconfig.yaml` on the remote host. Run `make k3d-bootstrap` once to install k3d (no sudo, into `~/.local/bin`) and create both clusters.

k3d images are pushed to `$K3S_REGISTRY` (local registry) or GHCR (`ghcr.io/jonathaneoliver/`).
