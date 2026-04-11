# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Product Behavior Source of Truth

Before making UI or product-behavior changes, read `PRD.md` and align implementation to it. If `AGENTS.md` and `PRD.md` conflict, follow `AGENTS.md` and explicitly call out the conflict in your summary/PR notes.

## Build & Run Commands

```bash
# Build Docker image
make build

# Run (Docker Compose)
make run         # uses start.sh
make stop

# Shell into running container
make shell

# Deploy to Lenovo k3s (dev stack)
make deploy

# Deploy to Lenovo k3s (release)
make deploy-release
```

UI is available at `http://localhost:21081/` (Docker Compose) or `http://lenovo.local:30000/` (k3s release) / `http://lenovo.local:40000/` (k3s dev).

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
  --host lenovo --scheme http --api-port 40000 --hls-port 40081

# Run against local Docker Compose
pytest test_player_characterization_pytest.py -m abrchar -v \
  --host localhost --scheme http --api-port 21081 --hls-port 21081

# Available abrchar test modes: smooth, steps, transient-shock, startup-caps,
#   downshift-severity, hysteresis-gap, emergency-downshift
pytest test_player_characterization_pytest.py -m abrchar -v \
  --abrchar-test-mode steps --abrchar-repeat-count 10
```

Default server target for tests is `lenovo:30000/30081`. Override with `--host`, `--api-port`, `--hls-port`, or `--api-base`.

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
| `go-proxy` | 30081 | Per-session testing proxy — failure injection, traffic shaping (nftables), SSE session stream |
| nginx | 21081 (compose) / 30000 (k3s) | Routing, static dashboard |

### nginx routing (`nginx-content.conf.template`)

- `/go-live/{content}/*.m3u8` and `*.mpd` → proxied to `go-live:8010` for dynamic generation
- `/go-live/{content}/*.m4s`, `*.ts`, `*.mp4`, etc. → served directly from the output directory by nginx
- `/api/content`, `/api/jobs`, `/api/sources`, `/api/upload`, `/api/setup` → `go-upload:8003`
- `/api/sessions*`, `/api/session/*`, `/api/failure-settings/*`, `/api/session-group/*`, `/api/nftables/*` → `go-proxy:30081`
- `/dashboard/` → `content/dashboard/` static files

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

### Dashboard (`content/dashboard/`)

Static HTML/JS/CSS pages served by nginx:
- `dashboard.html` — main entry
- `testing-session.html` + `testing-session-ui.js` — failure injection UI and player characterization
- `player-characterization.js` — ABR ramp sweep logic
- `grid.html`, `quartet.html`, `playback.html`, `go-monitor.html`, etc.

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

| Environment | UI | HLS/Proxy |
|---|---|---|
| Docker Compose | `localhost:21081` | `localhost:21081` |
| k3s release | `lenovo.local:30000` | `lenovo.local:30081` |
| k3s dev | `lenovo.local:40000` | `lenovo.local:40081` |

k3s images are pushed to `100.111.190.54:5000` (Lenovo local registry) or GHCR (`ghcr.io/jonathaneoliver/`).
