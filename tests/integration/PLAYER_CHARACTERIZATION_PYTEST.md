# Player Characterization (Host Pytest)

This is a host-side pytest port of the dashboard Player Characterization loop.

## Test File

- `tests/integration/test_player_characterization_pytest.py`

## What it does

- Opens `dashboard/testing-session.html` on the HLS base port (for example `:40081`) with generated `player_id` + stream URL to start real playback session
- Launches the page with `open_folds=network-shaping,bitrate-chart,player-characterization` so those sections open immediately
- Subscribes to `/api/sessions/stream` (SSE) on the API base port (for example `:40000`) to detect the created session for that `player_id`
- Loads ladder variants from session `manifest_variants` (or from the HLS master playlist fallback)
- Builds a variant-aware sweep schedule (down / up / both)
- Applies shaping via `PATCH /api/session/{id}` (`nftables_*` fields)
- Waits for control-rate confirmation
- Runs dataplane validation after each limit change (wire-throughput sample window)
- Collects per-second samples from `/api/session/{id}`:
  - throughput
  - rendition bitrate
  - buffer depth
  - stall count/time
- Emits settle/hold monitoring heartbeat logs in verbose mode (`-v`)
- Detects variant switch events
- Logs loop completion per cycle when rendition reaches top variant and stays there for 30s (`ABRCHAR loop_complete ...`)
- Assigns each run a persistent run number (`Run #`) and friendly run name
- Writes JSON + Markdown reports to pytest temp artifacts
- Restores shaping to a safe high bandwidth at the end

## Run

```bash
pytest tests/integration/test_player_characterization_pytest.py -m abrchar -v \
  --host lenovo --scheme http --api-port 30000 --hls-port 30081 --follow-redirects
```

Or against local Docker compose:

```bash
pytest tests/integration/test_player_characterization_pytest.py -m abrchar -v \
  --host localhost --scheme http --api-port 21081 --hls-port 21081 --follow-redirects
```

If auto-discovery returns HTTP 429, provide a known-good URL directly:

```bash
pytest tests/integration/test_player_characterization_pytest.py -m abrchar -v \
  --host lenovo --scheme http --api-port 40000 --hls-port 40081 \
  --url "http://lenovo:40081/go-live/redbull_p200_h264/master_6s.m3u8" --follow-redirects
```

Run step-jump mode (10 cycles of down then up):

```bash
pytest tests/integration/test_player_characterization_pytest.py -m abrchar -v \
  --host lenovo --scheme http --api-port 40000 --hls-port 40081 \
  --abrchar-test-mode steps --abrchar-repeat-count 10
```

## Useful options

- `--abrchar-hold-seconds` (default `8`)
- `--abrchar-smooth-step-seconds` (smooth mode only; seconds each smooth step lasts)
- `--abrchar-step-gap-seconds` (smooth mode only; extra wait between one limit change and the next)
- `--abrchar-settle-timeout` (default `30`)
- `--abrchar-settle-tolerance` (default `0.25` = ±25%)
- `--abrchar-test-mode` (`smooth` or `steps`)
- `--net-overhead` (`5` or `10`; JS-parity overhead for converting ladder Mbps to shaping limits)
- `--abrchar-overhead-pct` (default `10`)
- `--abrchar-max-steps` (default `0`, `0` = unlimited)
- `--abrchar-repeat-count` (default `10`; repeats the full step schedule this many times)
- `--abrchar-run-name` (optional friendly name override, e.g. `"Hotel WiFi Evening Run"`)
- `--follow-redirects` (default enabled; follow HTTP 30x during probing/discovery)
- `--abrchar-open-browser` (default enabled; launch browser playback page before session lookup)
- `--abrchar-browser-wait` (default `2.5`; seconds to wait after browser launch)
- `--abrchar-session-id` (attach directly to an existing session; skips browser warmup)
- `--abrchar-player-id` (attach to an existing `player_id`, useful for iPad simulator app)
- `--abrchar-attach-timeout` (default `60`; seconds to wait when attaching)
- `--abrchar-launch-ios-simulator` (if supplied IDs are missing, attempt to launch iOS simulator app first)

## iPad Simulator

If playback is running in iPad simulator, run in attach mode.

Attach behavior is now:

1. If `--abrchar-session-id` is provided and exists, attach immediately.
2. Else if `--abrchar-player-id` is provided and exists, attach immediately.
3. Else if `--abrchar-launch-ios-simulator` is set, try launching iOS simulator app and attach.
4. Else (default), fallback to browser warmup/session discovery.

Attach by `player_id`:

```bash
pytest tests/integration/test_player_characterization_pytest.py -m abrchar -v \
  --host lenovo --scheme http --api-port 40000 --hls-port 40081 \
  --abrchar-player-id "<simulator-player-id>" \
  --abrchar-launch-ios-simulator \
  --abrchar-attach-timeout 90
```

Attach by known `session_id`:

```bash
pytest tests/integration/test_player_characterization_pytest.py -m abrchar -v \
  --host lenovo --scheme http --api-port 40000 --hls-port 40081 \
  --abrchar-session-id "<existing-session-id>"
```

## Notes

- This is API/session driven and starts playback via the dashboard browser page by default.
- Port split is intentional: player page/stream on `--hls-port`; session polling + SSE on `--api-port`.
- For JS parity on throughput targeting, prefer `--net-overhead` (`5` or `10`); when set, it overrides `--abrchar-overhead-pct`.
- Any `player_id` embedded in a provided `--url` is normalized/overridden to this run's generated `player_id` to prevent duplicate sessions.
- It is marked `integration`, `slow`, and `abrchar`.
- A live playback session must exist for the generated `player_id` fixture.
- If a cycle does not satisfy the loop completion criterion (top rendition stable for 30s), the run logs `ABRCHAR loop_incomplete ...` and records a warning in JSON.

### Steps Behavior

- Top limit is set to `2x` top variant bitrate.
- Bottom limit is set to midpoint between the lowest two variants.
- Per limit change, runner waits up to `240s` for target rendition:
  - down-step target rendition: bottom variant
  - up-step target rendition: top variant
- After target rendition is reached, runner holds and samples for `30s` on that step.
- A cycle is one down-step and one up-step.
- During polling, every rendition change emits `ABRCHAR rendition_change ...` with:
  - time from limit change
  - frames presented delta
  - average buffer depth
  - average throughput
- End of run prints and writes huge-step summary tables with separate down/up sections and timing variation stats.

### Smooth Behavior

- Smooth probes around each adjacent transition using offsets above the next variant.
- Down begins with top variant reverse block, then per-transition sequence: `+50%, +20%, +15%, +10%, +5%, exact`.
- Up sequence starts at `V1` and uses: `exact, +5%, +10%, +15%, +20%, +50%` for each variant block.
- Limit-to-switch timing uses `server_video_rendition_mbps` as the primary signal (fetch-time rendition), with player variant as fallback.
- Auto-recovery is owned by the player page/app. When enabled, it restarts playback after `60s` of zero buffer depth.
- The runner observes `player_restarts` and tags transition summaries with `Restarts Δ` per step.
