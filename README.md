# InfiniteStream

## AI No‑Code project

This project is primarily an **AI No‑Code** build. The Go services and web dashboard were generated using Codex / OpenCode and Claude Sonnet 4.5, with human direction and iterative testing.

InfiniteStream is a Docker‑based HLS/DASH media server for testing video players under deterministic live‑like conditions. It generates LL‑HLS and LL‑DASH streams alongside 2s/6s segment variants and includes a rich dashboard for playback comparison, diagnostics, and monitoring.

## Quick start

```bash
# Build
make build
# or
# docker build --no-cache -t infinite-streaming .

# Run
make run
# or
# docker compose up -d
```

Open the UI:
- Docker Compose: http://localhost:21081/
- k3s (Lenovo): http://lenovo.local:30000/

## Lenovo k3s: release vs dev

Run release and dev side-by-side by using separate NodePort ranges and image tags.

Release (stable):
- UI: http://lenovo.local:30000/
- Ports: 30000 (UI), 30081 (initial proxy), 30181-30881 (session ports)
- Images: `:latest` (or your pinned release tag)
- Deploy: `make deploy-release`

Dev (active development):
- UI: http://lenovo.local:40000/
- Ports: 40000 (UI), 40081 (initial proxy), 40181-40881 (session ports)
- Images: `:dev`
- Deploy: `make deploy`

The dev stack is defined in `k8s-infinite-streaming-dev.yaml`, while release uses `k8s-infinite-streaming.yaml`.

## Release tagging notes

Use an immutable tag for releases (for example, `v1.2.3` or a commit SHA) and keep `:latest` as a moving pointer.

Suggested flow:
1) Create a tag and push it (example: `git tag v1.2.3 && git push origin v1.2.3`).
2) Build/push images with that tag (example: `:v1.2.3`).
3) Deploy release using the pinned tag (set `LENOVO_SERVER_IMAGE` and `LENOVO_PROXY_IMAGE` to the release tag).

Example deploy (release tag `v1.2.3`):

```bash
make deploy-release \
  LENOVO_SERVER_IMAGE=100.111.190.54:5000/infinite-streaming:v1.2.3 \
  LENOVO_PROXY_IMAGE=100.111.190.54:5000/go-proxy:v1.2.3
```

## GitHub Container Registry (GHCR)

This repo ships a GitHub Actions workflow that builds and publishes images to GHCR on pushes to `main`.

Image tags:
- `ghcr.io/<owner>/<repo>:main`
- `ghcr.io/<owner>/<repo>:latest`
- `ghcr.io/<owner>/<repo>:sha-<short>`

To enable publishing in your fork:
1) Set the default branch to `main`.
2) In **Settings → Actions → General**, allow GitHub Actions to write packages.

## What it does

### Live stream generation
- **On‑demand**: the first request for a piece of content starts a per‑content worker.
- **Single worker, shared clock**: each content worker generates all HLS + DASH manifests (LL + 2s + 6s) in sync.
- **Low‑latency**: LL‑HLS and LL‑DASH update on partial boundaries (default 200ms).
- **Segmented variants**: 2s and 6s variants update on their segment boundaries only.
- **Sliding window**: fixed window (e.g., 36s) that moves forward and wraps on loop boundaries.
- **Auto shutdown**: workers stop after an idle timeout when no requests are active.

### Dashboard
- **Playback**: single‑stream view with protocol, codec, segment duration, and player selection.
- **Quartet**: multi‑panel comparison across encodings/players.
- **Mosaic (Grid)**: multi‑tile view with filters (protocol/codec/segment).
- **Live Offset**: compares live offset, buffer depth, and seekable ranges across variants.
- **Go‑Monitor**: shows active workers, request counts, last request time, idle timeout, and tick timings.
- **Testing session**: failure injection (HTTP errors, hung, corrupted segments), player selection (HLS.js/Shaka/Video.js/Native), and HTTP failure logging.
- **Transport faults**: port-wide DROP/REJECT via nftables with packet counters.

### Global selection
- The selected content + URL persists across pages. Choosing a tile or selector updates the global selection and is reflected in the header and dev tools.


## Host filesystem & content

InfiniteStream expects a **host-mounted volume** for originals and encoded outputs. In `docker-compose.yml` this is typically mapped to `/boss` inside the container:

- **Host path** (example): `/Volumes/4TB/Boss`
- **Container path**: `/boss`

Directory layout inside `/boss`:

- `/boss/originals` — source files (MP4, MOV, etc.)
- `/boss/dynamic_content/{content}` — encoded outputs

### How to add content

**Option A — Web upload**
1) Open **Upload Content** in the dashboard.
2) Choose a file and encoding options.
3) The server writes the source into `/boss/originals` and the encoded ladder into `/boss/dynamic_content`.

**Option B — Copy directly**
1) Copy files into the host folder (e.g. `/Volumes/4TB/Boss/originals`).
2) The content will appear in **Source Library** on refresh.
3) You can then trigger encodes from the UI or run the encoding scripts manually.

> Tip: If you copy into `/boss/originals` while the server is running, just refresh the Source Library page to see new items.


## Services

- **go-live** (port 8010): LL‑HLS + LL‑DASH generation, plus 2s/6s variants
- **go-upload** (port 8003): upload API, job orchestration, content discovery
- **nginx**: routing + static dashboard
  - **Host UI (docker-compose)**: `http://localhost:21081/`
  - **Host UI (k3s/NodePort)**: `http://lenovo.local:30000/`

## Primary endpoints (host)

### HLS (LL/2s/6s)
- Docker Compose: `http://localhost:21081/go-live/{content}/master.m3u8`
- k3s NodePort: `http://lenovo.local:30081/go-live/{content}/master.m3u8`
- k3s NodePort: `http://lenovo.local:30081/go-live/{content}/master_2s.m3u8`
- k3s NodePort: `http://lenovo.local:30081/go-live/{content}/master_6s.m3u8`

### DASH (LL/2s/6s)
- Docker Compose: `http://localhost:21081/go-live/{content}/manifest.mpd`
- k3s NodePort: `http://lenovo.local:30081/go-live/{content}/manifest.mpd`
- k3s NodePort: `http://lenovo.local:30081/go-live/{content}/manifest_2s.mpd`
- k3s NodePort: `http://lenovo.local:30081/go-live/{content}/manifest_6s.mpd`

### APIs
- `GET /api/content`
- `GET /api/jobs`

## Screenshots

> Screenshots are captured from the live dashboard and stored in `screenshots/`.

**Dashboard**

![Dashboard](screenshots/dashboard.png)

**Upload Content**

![Upload Content](screenshots/upload-content.png)

**Source Library**

![Source Library](screenshots/source-library.png)

**Encoding Jobs**

![Encoding Jobs](screenshots/encoding-jobs.png)

**Mosaic (Grid)**

![Mosaic](screenshots/mosaic.png)

**Playback**

![Playback](screenshots/playback.png)

**Testing Player**

![Testing Player](screenshots/testing-player.png)

**Live Offset**

![Live Offset](screenshots/live-offset.png)

## Testing Player (how to use)

Open via the Mosaic (Grid) right‑click menu → “Open in Testing Window”, or directly:

```text
# Docker Compose
http://localhost:21081/dashboard/testing-session.html?player_id=<uuid>&url=<encoded-stream-url>

# k3s
http://lenovo.local:30000/dashboard/testing-session.html?player_id=<uuid>&url=<encoded-stream-url>
```

The `player_id` is required. The proxy uses it to bind the playback session to a dedicated port, so requests to the original port are redirected to a session‑specific port. This allows per‑session failure injection and traffic shaping without affecting other sessions.

k3s NodePort mapping used by the testing flow:
- External NodePorts (browser): `40000` (UI) and `40081` + `40181`..`40881` (sessions)
- Internal container ports: `30000` (UI) and `30081` + `30181`..`30881` (sessions)
- go‑proxy maps external → internal using `EXTERNAL_PORT_BASE`, `INTERNAL_PORT_BASE`, and `PORT_RANGE_COUNT`

Controls:
- **Retry Fetch**: re‑issues the current stream request without resetting the player.
- **Restart Playback**: destroys and rebuilds the player, then reloads the current URL.
- **Reload Page**: full page reload with current query params.
- **Player selector**: choose HLS.js, Shaka, Video.js, Native, or Auto.

Failure injection (per session):
- Set **Failure Type** (must be non‑none to activate failures).
- Set **Units** (Requests / Seconds / Failures‑per‑Seconds).
- Set **Consecutive** (how many failures in a row).
- Set **Frequency** (spacing between failure windows).
Changes auto‑save.

Transport faults (per session port):
- **Fault Type**: None / Drop / Reject.
- **Units**: Seconds or Packets / Seconds.
- **Consecutive**:
  - seconds mode: active fault window duration
  - packets mode: packet threshold before disarm
- **Frequency (secs)**: cycle spacing (0 means one-shot/manual behavior based on consecutive).
- **Counters**: UI shows current/last `Drop pkts` and `Reject pkts`.

## Encoding pipeline

Encoding is driven by the bash pipeline:

- `/generate_abr/create_abr_ladder.sh`
- Python helpers:
  - `/generate_abr/create_hls_manifests.py`
  - `/generate_abr/convert_to_segmentlist.py`

Shaka Packager is bundled in the container (`packager v3.4.2`).

Defaults (UI + pipeline):
- Segment duration: **6s**
- Partial duration: **200ms**
- GOP duration: **1s**

## Known limitations (selected)

These are common LL‑HLS/LL‑DASH expectations that are **not fully implemented**:
- Blocking playlist reload (`_HLS_msn`, `_HLS_part`) and skip logic (`_HLS_skip`)
- `#EXT-X-RENDITION-REPORT` and `#EXT-X-PRELOAD-HINT`
- Chunked CMAF transfer for LL‑DASH partials

See `PRD.md` for the full list.

## Testing capabilities (recent additions)
- Failure injection modes for segments/playlists/manifests, including corrupted segment payloads and hung responses.
- Segment failure timing supports failures‑per‑second (separate frequency vs consecutive units).
- Transport fault injection supports port-wide DROP/REJECT with seconds or packet thresholds.
- Testing session player selector (HLS.js, Shaka, Video.js, Native) with error + HTTP failure logging.
- Developer context menu option in Mosaic (developer=1) to open the HLS.js demo with the test URL.
- Platform‑aware network shaping capabilities endpoint (Linux‑only support).
- Server‑authoritative session updates with control‑revision conflict handling and SSE session stream.
- Session grouping controls (group/ungroup/merge) that propagate failure settings and network shaping.

## Why unified error injection?
Many environments already provide failure simulation (player debug tools, OS/Browser dev tools, routers, or lab network appliances). InfiniteStream still ships a unified error injection layer because it:
- **Keeps tests deterministic** across players and environments (same failure schedule, same stream).
- **Targets the streaming domain directly** (segments/playlists/manifests), not just generic network faults.
- **Is portable and reproducible** in CI, on shared QA rigs, and across teams.
- **Decouples test setup from client device tooling**, so tests are easier to document and repeat.

## Client apps

InfiniteStream includes native client apps for testing on various platforms:

### iOS/tvOS App
Located in `apple/InfiniteStreamPlayer/`, this SwiftUI app provides:
- Protocol selection (HLS/DASH)
- Segment duration and codec options
- Server environment switching
- Content selection and auto-play
- Detailed playback diagnostics
- Testing session integration

### Roku Channel
Located in `roku/InfiniteStreamPlayer/`, this BrightScript channel provides:
- HLS and DASH playback support
- Protocol, segment, and codec selection
- Server environment switching
- Content selection and auto-play
- Remote control navigation
- Engineering-focused testing solution

See the README in each directory for platform-specific setup and usage instructions.

## Documentation index

- `PRD.md`
- `TESTING_GUIDE.md`
- `UPLOAD_BACKGROUND_IMPLEMENTATION.md`
- `LOOP_TEST_PLAN.md`
- `content/PLAYBACK-TEST-README.md`
- `content/AUTOMATIC-DETECTIONS.md`
- `go-live/IMPLEMENTATION_SUMMARY.md`
- `go-live/PLAN.md`
- `generate_abr/README.md`
- `generate_abr/QUICKSTART.md`
- `generate_abr/SEGMENTLIST_VS_TEMPLATE.md`
- `generate_abr/ENCODER_BURNIN_LABELS.md`
- `generate_abr/HARDWARE_ENCODING_QUICKREF.md`
- `generate_abr/HARDWARE_ENCODER_VALIDATION.md`
- `generate_abr/PACKAGER_COMPARISON.md`
- `generate_abr/DASH_PACKAGING_COMPARISON.md`

## License

See `LICENSE` for attribution, internal‑use, and redistribution terms.
