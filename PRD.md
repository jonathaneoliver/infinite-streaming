# Product Requirements Document (PRD)

**Product:** InfiniteStream — HLS/DASH media test server + dashboard

## 1) Purpose & Vision
InfiniteStream provides a deterministic, configurable environment for testing HLS/DASH players under controlled live‑like conditions. It generates LL‑HLS and LL‑DASH streams alongside 2s/6s segment variants, and offers a dashboard for side‑by‑side playback and diagnostics.

The system is intended for:
- Player QA and regression testing
- Live latency comparison across protocols and players
- Encoding pipeline validation
- Controlled reproduction of streaming edge cases

## 2) Goals
- **Deterministic live simulation**: looping VOD with a stable, sliding window and repeatable timing.
- **Protocol coverage**: LL‑HLS, LL‑DASH, and non‑LL 2s/6s variants for both HLS and DASH.
- **Single source of truth**: one worker per content generates all manifests/playlists on a shared clock.
- **Dashboard visibility**: clear playback comparisons, diagnostics, and monitoring of live workers.
- **Fault injection**: configurable, repeatable failure modes to exercise player error handling.
- **Operational simplicity**: Dockerized runtime with minimal external dependencies.

## 3) Non‑Goals
- Production‑grade streaming origin/CDN or DRM.
- Fully standards‑compliant LL‑HLS/LL‑DASH in every edge case.
- Automatic adaptive bitrate optimization beyond fixed ladder presets.

## 4) Users & Use Cases
**Primary users:** video engineers, QA, player developers.

**Core use cases:**
- Compare latency between HLS and DASH for the same content.
- Validate segment window behavior at loop boundaries.
- Verify player behavior under LL vs 2s/6s segmentation.
- Trigger re‑encodes and inspect results quickly.
- Monitor active stream generators and idle timeouts.

## 5) System Overview
### Runtime services
- **go-live**: Generates LL‑HLS + LL‑DASH + 2s/6s variants for any content on demand.
- **go-upload**: Uploads + job orchestration + content discovery APIs.
- **nginx**: Routes requests and serves static dashboard assets.

### Storage & Content
- **Originals**: source files in `/boss/originals`.
- **Dynamic content**: encoded outputs under `/boss/dynamic_content/{content}`.
- **Generated live outputs**: `/content/go-live/{content}/...` (tmpfs).

## 6) Streaming Behavior (Functional Requirements)
### 6.1 Live generation
- Any request for HLS or DASH content starts a single **per‑content worker** that generates **all** manifests/playlists (LL + 2s + 6s) using the same clock.
- The worker updates:
  - **LL-HLS / LL-DASH** every 200ms (or per configured partial duration).
  - **2s/6s variants** only when their segment boundary advances.
- Workers stop after an idle timeout (configurable) if there are no active requests.

### 6.2 URL patterns
**HLS**
- `/go-live/{content}/master.m3u8` (LL)
- `/go-live/{content}/master_2s.m3u8`
- `/go-live/{content}/master_6s.m3u8`
- Variant playlists: `playlist_{variant}.m3u8`, `playlist_2s_{variant}.m3u8`, `playlist_6s_{variant}.m3u8`

**DASH**
- `/go-live/{content}/manifest.mpd` (LL)
- `/go-live/{content}/manifest_2s.mpd`
- `/go-live/{content}/manifest_6s.mpd`

### 6.3 Sliding window
- Window size is fixed (e.g., 36s) and moves forward as time advances.
- Loop handling: window can span a discontinuity at the end of VOD and wrap to the start.
- HLS playlists and DASH MPDs must reflect the same window boundaries across audio/video.

### 6.4 Segment alignment
- HLS audio/video segments align by window position (same logical segment indices).
- DASH Periods split when the window spans a loop boundary.

### 6.5 LL specifics
- LL‑HLS: partial segments are listed with `#EXT-X-PART` and aligned to partial duration (e.g., 200ms).
- LL‑DASH: MPD includes `availabilityTimeOffset` and per‑partial timeline entries.

## 7) Dashboard Application (Functional Requirements)
### 7.1 Global selection state
- A global selection is stored in localStorage:
  - `bossSelectedContent`, `bossSelectedUrl`, `bossSelectedProtocol`, `bossSelectedSegment`, `bossSelectedCodec`.
- Selection persists across pages; selecting a content item updates the global state and should not be overridden by other pages.

### 7.2 Core pages
**Dashboard**
- High‑level navigation and entry points into playback/monitoring.

**Playback**
- Single player view.
- Selector for protocol (HLS/DASH), codec, segment duration (LL/2s/6s), player engine.
- Auto‑play on change; mute state persists across pages.

**Testing session**
- Per‑session failure injection (segments/playlists/manifests) with repeatable timing.
- Failure modes include HTTP error codes, hung responses, and corrupted segment payloads.
- Failure timing supports failures‑per‑second (separate frequency vs consecutive units).
- Per‑port transport fault injection (DROP/REJECT) via nftables.
- Transport faults support consecutive units in seconds or packets, with frequency in seconds.
- Transport fault packet counters (drop/reject) are surfaced in API/UI for observability.
- Server‑authoritative control updates with per‑field PATCH + control‑revision conflict handling.
- Session grouping controls (group/ungroup/merge) with group badges and group‑aware control propagation.
- Player selector: HLS.js, Shaka, Video.js, Native.
- Logs player errors and HTTP failure details in the testing UI.

### 7.6 Player Characterization (ABR)
- A per‑session **Player Characterization** panel is available in the Testing session UI.
- Purpose: generate deterministic ABR ramp runs and capture switch/stall behavior under controlled limits.
- Controls include:
   - direction (`down`, `up`, `down+up`)
   - hold duration
   - network overhead selector (`5%` or `10%`)
   - min/max limit bounds
- Ramp generation model:
   - Parse active HLS ladder from master playlist.
   - Convert media ladder values to wire targets using selected overhead:
      - `wire_variant_mbps = media_variant_mbps / (1 - overhead_pct)`
   - For each adjacent ladder pair, generate interpolation points:
      - `0%, 5%, 10%, 25%, 50%, 75%, 90%, 95%`
   - Apply direction ordering (down/up) to produce the step sequence.
- Run output requirements:
   - live progress log
   - switch/stall event capture
   - per‑period throughput and buffer-depth stats
   - downloadable report artifacts (JSON + markdown)
- Restart behavior during characterization:
   - detect persistent stalled conditions from cumulative `stall_time_s` sample deltas and stalled-like sample streaks
   - optionally restart playback without changing active shaping limits when thresholds are crossed

**Quartet**
- Side‑by‑side comparison of multiple encodings/players.
- Uses best‑fit player by protocol (e.g., Shaka for DASH, HLS.js for HLS).

**Mosaic (Grid)**
- Multi‑tile playback with filters for protocol, codec, segment duration, resolution.
- Clicking a tile selects audio and updates global selection.
 - Developer context menu includes an HLS.js demo link for the selected test URL (developer=1).

**Live Offset Comparison**
- Compare live offset and buffering for LL‑HLS vs 2s/6s HLS vs DASH variants.

**Go‑Monitor**
- Active stream worker list, grouped by content.
- Shows last request, request counts, idle timeout, tick/avg generation timings.

### 7.3 Development tools (developer=1)
- Local HLS.js demo page that loads selected content.
- Shaka analysis page with playback diagnostics.

### 7.4 Rationale: Unified error injection
Many platforms already expose their own failure tools (player debug features, browser dev tools, OS or router shaping). InfiniteStream still provides a unified error‑injection layer because it:
- Makes scenarios **deterministic and repeatable** across players and environments.
- Targets **streaming‑specific faults** (playlist/segment corruption, timing, response codes) instead of only network‑level issues.
- Works **cross‑team and in CI** without requiring per‑device tooling.
- Keeps test setup **documented and portable** with the content itself.

### 7.5 Session Data Distribution & Shaping Control
- **Session store**: all session state is stored in memcache under `session_list` and broadcast on change.
- **SSE updates**: `/api/sessions/stream` emits full session snapshots (no 10‑session cap). UI should treat each event as authoritative.
- **Polling fallback**: when SSE is unavailable, the UI polls `/api/sessions` and applies the same normalization logic.
- **Control vs data**: UI control widgets sync only when `control_revision` changes; data metrics update every tick.
- **Rate limit application**: `/api/nftables/shape/{port}` applies user changes; per‑request shaping only re‑applies when the desired config changes.
- **Pattern shaping**: the pattern loop updates the port only when a step’s target rate/delay/loss changes. Runtime fields (`nftables_pattern_step_runtime`, `nftables_pattern_rate_runtime_mbps`) are written back to session data for UI charts.
- **Shaping cache**: the Go proxy keeps a per‑port cache of the last applied rate/delay/loss to avoid redundant `tc`/netem operations.
- **Wire metrics sampling**: throughput sampler runs every 100ms using per-port `tc` class counters.
- **Wire metric scope**: sampled bytes include packet-level transport/application overhead seen at that interface (for example TCP/IP headers and TLS/HTTP bytes), but exclude physical link-layer overhead (for example Ethernet preamble/IFG/FCS).
- **Wire sustained (18s)**: `mbps_wire_sustained`/`mbps_wire_sustained_18s` are wall‑time sustained rates over the last 18s (bytes / elapsed time).
- **Wire active (18s)**: `mbps_wire_active` divides active bytes by active seconds only.
- **Short windows**: `mbps_wire_sustained_6s`, `mbps_wire_sustained_1s`, and `mbps_wire_active_6s` are computed from 6s/1s sample windows.
- **Wire throughput**: `mbps_wire_throughput` is defined as max(`mbps_wire_active_1s`) over a rolling 6s window.
- **Port reconciliation**: session throughput hydration chooses the freshest sample across external and internal port keys.
- **Metric semantics**:
   - **Limit**: shaping target configured by control plane (`/api/nftables/shape/{port}`); prescriptive, not measured.
   - **Wire throughput**: measured interface throughput from `tc` counters (`mbps_wire_*`), including packet-level transport/application overhead visible at that interface.
   - **Player estimate**: player-reported ABR network estimate (`player_metrics_network_bitrate_mbps`), algorithmic and client-side.
- **Expected differences**:
   - `wire throughput` generally converges toward but does not exceed `limit` for sustained intervals.
   - `player estimate` should follow `wire throughput` trends over time but may diverge transiently due to smoothing, startup bias, rebuffering, or adaptation hysteresis.

## 8) Encoding & Packaging (Functional Requirements)
- `generate_abr/create_abr_ladder.sh` builds ladders for H.264/H.265/AV1.
- Segment duration default: 6s.
- Partial duration default: 200ms.
- GOP default: 1s (configurable in UI).
- Packaging uses Shaka Packager for DASH outputs when available.

## 9) Monitoring & Logging
- Per‑content worker logs:
  - LL‑HLS tick time, avg_5m
  - LL‑DASH tick time, avg_5m
  - 2s/6s HLS/DASH update timings when generated
- Go‑Monitor exposes status for each content/variant.
 - Testing session logs per‑player errors and HTTP failure details.

## 10) Security & Access
- Local development focus; no auth required.
- Intended for trusted environments only.

## 11) Configuration
- Idle timeout for workers (configurable, visible in Go‑Monitor).
- Output directories for generated manifests (tmpfs).
- External/internal port mapping for NodePort deployments (go‑proxy uses `EXTERNAL_PORT_BASE`, `INTERNAL_PORT_BASE`, `PORT_RANGE_COUNT`; external `4xxxx` ports map to internal `3xxxx` ports).

## 12) Known Limitations & Expected‑but‑Missing Features
These are features commonly expected in LL streaming origins but **not currently implemented** (or incomplete):

### LL‑HLS
- `_HLS_msn` / `_HLS_part` request handling and blocking semantics.
- `#EXT-X-PRELOAD-HINT` / `#EXT-X-SERVER-CONTROL` tuning per player.
- HTTP/2 chunked transfer for partials.

### LL‑DASH
- Chunked CMAF transfer (in-flight partials rather than byte‑range splits).
- Accurate `availabilityTimeComplete` handling for partial publication.
- Full DASH‑IF conformance checks.

### Player/Origin
- Network shaping is Linux‑only (tc/netem); non‑Linux environments will report shaping as disabled.
- CDN cache behavior simulation and stale‑while‑revalidate flows.
- DRM signaling or encryption.

## 13) Success Criteria
- All dashboard pages load consistently and honor the global selection.
- HLS + DASH LL and 2s/6s variants are produced on demand with aligned windows.
- Go‑Monitor reflects active generators and idle shutdown correctly.
- Playback works in Chrome and Safari for HLS and DASH (via Shaka where required).

## 14) Licensing
- Attribution required.
- Internal use allowed.
- Commercial productization prohibited without permission.

See `LICENSE` for details.

## Appendix A: LL‑HLS Features Not Implemented (Apple Reference Gaps)

These behaviors are present in Apple’s reference LL‑HLS origin example but are **not** implemented in go‑live:

1) **Blocking playlist reload**
   - `_HLS_msn` + `_HLS_part` query params to block until a segment/part is available.
   - Recommended 3× target duration timeout (return 503 if not ready).

2) **Skip parameter support**
   - `_HLS_skip=YES` handling and `#EXT-X-SKIP:SKIPPED-SEGMENTS` emission.
   - Playlist version bump to 9 when skipping.

3) **Rendition reports**
   - `#EXT-X-RENDITION-REPORT` for other media playlists.

4) **Preload hints**
   - `#EXT-X-PRELOAD-HINT` for upcoming partials/segments.

5) **Full SERVER‑CONTROL metadata**
   - `CAN-BLOCK-RELOAD=YES` and `CAN-SKIP-UNTIL=...` tags.
   - Additional `PART-HOLD-BACK` tuning beyond the fixed value.

6) **Segment readiness gating**
   - A dedicated low‑latency segment endpoint that blocks until a part appears in the playlist.
   - Ensures segments are only fetched after they are advertised.

7) **Blocking/time headers**
   - `block-duration` response header.
   - Cache control changes based on blocking and update cadence.

8) **Dedicated LL segment endpoint**
   - Apple uses `lowLatencySeg` with `?segment=...` indirection for parts.
   - go‑live links directly to segment paths and byte‑ranges.

These gaps are intentional simplifications. If compatibility with strict LL‑HLS clients is required, these behaviors should be added.

## Appendix B: Not Implemented (Expected by Some Players)

- **I‑frame only playlists** (`#EXT-X-I-FRAMES-ONLY`), no I‑frame-only manifests generated.
- **DRM signaling and key delivery** (FairPlay/Widevine/PlayReady), no key server integration or encryption.
