# Player characterization framework

Go test suite replacing the python `test_player_characterization_pytest.py`. Drives the harness CLI to apply network shapes and samples the v2 stream to characterize player ABR behaviour across iPhone / iPad / Apple TV / Android TV / Web.

Tracks issue #482. Roku is out of scope.

## What do I need installed?

Three personas — install only what your devices require:

| persona | tools needed | how to install | `LAUNCH_MODE` |
|---|---|---|---|
| **Web only** (Chrome) | `harness` CLI | `make harness-cli` | `manual` |
| **Sim / real iOS** (iPad sim, iPhone) | + Xcode (gives `xcrun`) | App Store, or `xcode-select --install` | `cli` (default) |
| **Android TV** | + Android Platform Tools (`adb`) | `brew install --cask android-platform-tools` | `cli` |
| **Apple TV with automation** ¹ | + Appium server + signed WebDriverAgent for tvOS | `brew install node && npm install -g appium && appium driver install xcuitest`, then build `WebDriverAgent.xcodeproj` once via Xcode | `appium` |

¹ Apple TV works on `cli` too if you wake the device manually before each test. `appium` adds programmatic wake + screenshots.

**Don't know what you need?** Run the preflight diagnostic — it tells you what's available and recommends a mode:

```sh
go test -C tests/characterization ./runner/... -v -run TestPreflight
```

Sample output:

```
PREFLIGHT — what your environment supports
  ✓ harness CLI          /Users/me/.local/bin/harness
  ✓ proxy via harness    reachable
  ✓ xcrun                Xcode command-line tools
  ✗ adb                  Android Platform Tools     (fix: `brew install --cask android-platform-tools`)
  ✗ Appium server        not reachable              (fix: only needed for Apple TV automation)

DEVICES (currently discoverable)
  iphone     My iPhone               <udid>
  appletv    Apple TV                <udid>

RECOMMENDED LAUNCH_MODE=cli
  why: Xcode and/or adb available; CLI handles sim + real iOS + Android TV
```

## Status

Phases 0, 0.5, 1, 2, 3, 4 (minimum-viable), 5 landed: scaffolding, all three launchers (`Manual` / `CLI` / `Appium`), all 7 characterization modes wired per platform, the aggregator binary, and the preflight diagnostic. Appium covers Launch / Kill / Screenshot — full UI automation (content selection, settings, 911 / Reload buttons) needs accessibility identifiers in the player apps; not yet implemented.

## Layout

```
tests/characterization/
├── runner/
│   ├── device.go      Platform + LaunchMode + Device types
│   ├── launcher.go    Launcher interface (Mode/Discover/Launch/Kill/Close)
│   ├── manual.go      ManualLauncher — operator-driven, no platform tooling
│   ├── cli.go         CLILauncher — xcrun devicectl/simctl + adb
│   ├── mode.go        PickMode() — env-driven launcher selection
│   ├── shape.go       Session.ApplyRate / ClearShape (harness wrappers)
│   ├── sample.go      Sampler — 1Hz polling, builds []Sample
│   ├── report.go      Report writer (JSON + Markdown), Finalize summary math
│   ├── harness.go     wrapper for the harness CLI
│   ├── session.go     Session (device + player_id + launcher)
│   └── *_test.go      runner-layer smoke + unit tests
├── modes/             one *_test.go per characterization mode
│   ├── sweep.go              shared OpenSession / RunMode / RunSweep helpers
│   ├── smooth_test.go        7 modes × 4 platforms = 28 Test* funcs
│   ├── steps_test.go
│   ├── transient_shock_test.go
│   ├── startup_caps_test.go
│   ├── downshift_severity_test.go
│   ├── hysteresis_gap_test.go
│   └── emergency_downshift_test.go
└── cmd/characterize-report/   matrix aggregator binary
```

## Running

```sh
# From the repo root:
make harness-cli                                                # installs `harness` to ~/.local/bin

# Convenience make targets (wrap overnight.sh per platform; results post to
# the dashboard's Automated Testing page):
make characterize-ipad-sim      # or -iphone / -appletv / -androidtv / -web
make characterize-server        # server_* control-surface checks (tests/server_behavior) vs test-dev
make automated-testing          # one-shot: server checks, then iPad-sim players

# Smooth sweep on first iPad sim found — no env vars needed.
# Artifacts land in tests/characterization/artifacts/.
go test -C tests/characterization ./modes/... \
    -v -run TestSmoothIPadSim -timeout 90m -count=1

# Unit + smoke (no live player needed)
go test -C tests/characterization ./runner/... -v

# Target a specific device (e.g. for parallel runs across two sims)
CHARACTERIZATION_DEVICE_UDID=<device-udid>                      \
  go test -C tests/characterization ./modes/... \
      -v -run TestSmoothIPadSim -timeout 90m -count=1

# Aggregate per-test reports into one matrix:
go run -C tests/characterization ./cmd/characterize-report \
    -charts ./artifacts > matrix.md
```

Each mode test skips with `t.Skip` if no device of its platform is reachable, so a partial-coverage run (e.g. "iPhone + iPad-sim only") doesn't fail.

## Modes

| Mode | What it sweeps | Approx runtime |
|---|---|---|
| `smooth` | 12-step linear ramp 6 → 0.5 Mbps, 10 s each | ~2 min |
| `steps` | 6-step linear ramp 6 → 0.5 Mbps, 30 s each | ~3 min |
| `transient-shock` | 8 → 0.5 → 8 Mbps brief dip | ~1.5 min |
| `startup-caps` | apply cap, kill+relaunch, observe variant at 5 cap levels | ~5 min |
| `downshift-severity` | cliff drop 8 → 1 Mbps, count overshoot | ~3 min |
| `hysteresis-gap` | stairs up 0.5 → 6, then down, find gap per variant | ~6 min |
| `emergency-downshift` | drop to 0.05 Mbps briefly, measure recovery | ~2.5 min |

## Declarative matrix runner (`harness char matrix`, #811)

Instead of hand-rolling a nested bash loop of `CHAR_*` env vars to sweep a
combinatorial matrix (segment × live_offset × lever × …), describe it in YAML and
let the harness expand + drive it:

```bash
# Sanity-check the expansion — pure, touches no session:
harness char matrix tests/characterization/matrix/live-offset.yaml --dry-run

# Run it sequentially: per-arm config-on-connect bootstrap → appium probe →
# achieved-offset table.
harness char matrix tests/characterization/matrix/live-offset.yaml
```

The spec format (`matrix/live-offset.yaml` is a worked example):

- **`axes:`** → cartesian product (an odometer over sorted axis names, so arm ids
  are reproducible). Known axes: `platform`, `protocol`, `content`, `segment`,
  `mode`, `class`, `lever`, `live_offset`, `peak_bitrate_mbps`, `duration_s`,
  `reps`. An unknown axis fails fast.
- **`defaults:`** → a base arm every expanded/explicit arm is layered over.
- **`arms:`** → explicit-arm escape hatch (appended after the cartesian product),
  each layered over `defaults`. Nested `shape:` / `fault:` /
  `content_manipulation:` / `transfer_timeouts:` blocks decode straight into the
  reused `internal/sweep` recipe types (same `json:` tags, no dual-tagging).
- **`lever:`** routes a `live_offset` value — `proxy` to the server manifest
  hold-back (config-on-connect, no relaunch), `app` to the client
  `-is.flag.live_offset_s` override (cold launch per arm). The post-run
  manipulation check (#793) measures the achieved offset either way.

Each arm reuses the sweep's bootstrap path (`experimentPlayerPatch` →
`shaperBootstrapURL`) for the server recipe and `TestSweepProbe` (via
`runner.ProbeLaunchArgs`) for the client launch args. Measurement is keyed by
`player_id` (survives play_id rotation + cross-traffic).

**Sequential vs parallel.** `parallel: false` (default) runs arms one at a time
on a single device. `parallel: true` fans every arm out **simultaneously** on the
fleet backend — the CLI bootstraps each arm's server recipe up front, then runs
`TestCharMatrixFleet` once with `CHAR_FLEET_COUNT=N` and the per-arm knobs in
`CHAR_ARM_<i>_*` env (one arm per device, gated to a common start by the fleet
HOME barrier), then measures all. A parallel matrix must be **single-platform**
(the fleet draws from one device pool) and needs at least N booted devices of
that platform — boot more sims or split the matrix by platform otherwise.

## Per-play client reconfig without relaunch (#800)

Client-side config (segment / live_offset / protocol / peak_bitrate) is normally
forced via a cold-start launch arg (`-is.segment` etc., #797) — one cold launch
(~25–30 s) per matrix cell. The server-side half (cap, faults, content) already
reconfigures per-play with no restart; #800 brings the client half in line.

`runner.Session.ApplyAppConfig(ctx, runner.AppConfig{Segment: "s2", ...})` PATCHes
the bound player's `app_config` (via `harness app-config <pid> --segment s2 …`);
the app overlays it at its **next play boundary** with no relaunch. Use it to vary
a client-side axis between plays of a long-running app instead of relaunching per
cell.

> **Timing.** The proxy has no session to hold `app_config` until the player's
> first bootstrap request, so this targets *subsequent* plays of a running app —
> the very first play still uses config-on-connect (`app.<field>` bootstrap args)
> or the launch arg. A multi-play sweep driver that loops cells on ONE launch
> (PATCH → fresh play → repeat) is the natural consumer; the single-play modes
> below still cold-launch per run.

## Launch modes

The framework supports three launch modes, picked by `$LAUNCH_MODE`:

| Mode | Env | What it needs | What it does |
|---|---|---|---|
| `manual` | `-launch-mode=manual` (or `LAUNCH_MODE=manual`) | nothing | prompts the operator + observes via harness |
| `cli` (default) | `-launch-mode=cli` (or unset) | xcrun + simctl + adb on $PATH | kills + relaunches the app; relies on `skipHomeOnLaunch=true` for auto-resume |
| `appium` | `-launch-mode=appium` (or `LAUNCH_MODE=appium`) | Appium server + WDA + xcuitest/uiautomator2 drivers | Launch + Kill + Screenshot; full UI automation needs accessibility identifiers in the apps |

The `-launch-mode` flag is preferred over the env var — keeps `go` as the first token of the bash command so Claude Code (and similar tooling with command allowlists) doesn't re-prompt every invocation.

`manual` and `cli` are implemented. `appium` is the optional Phase 4.

The CLI launcher expects each player app to be built with `skipHomeOnLaunch=true` so a cold launch picks up `lastPlayed` automatically — that's how it can recover from a wedged player without driving UI.

### Device Farm (`CHAR_DEVICE_FARM=1`)

Layered on top of `appium` mode: instead of the harness hand-picking a UDID and offsetting WDA/MJPEG ports, the [`appium-device-farm`](../../tools/appium-device-farm/) plugin arbitrates devices — capability-based allocation, queuing, and auto port assignment (like Selenium Grid for browsers). Turn it on with `CHAR_DEVICE_FARM=1` alongside `-launch-mode=appium`. The plain-appium path stays the default.

Under the flag the harness requests a device by capability (`platformName` + latest `platformVersion` for sims, overridable via `CHAR_DF_IOS_VERSION` / `CHAR_DF_TVOS_VERSION`; real hardware unconstrained), reads back the allocated UDID, and builds the fleet roster as N **logical** devices (`CHAR_FLEET_COUNT`, default 1) — no UDID pinning, port offsets, `staggerFleetLaunch`, or per-UDID `seedFleetServer` (the server is set by the app's server-picker navigation). `CHAR_FLEET_UDIDS` identities are ignored under DF.

Workflow:

```sh
tools/appium-device-farm/boot-pool.sh   # boot N latest-OS sims + verify app + warm WDA
tools/appium-device-farm/run.sh         # start the DF server on :4723
CHAR_DEVICE_FARM=1 go test -C tests/characterization ./modes/... \
  -run TestStartupIPadSim -launch-mode=appium -timeout 20m
# or: CHAR_DEVICE_FARM=1 make characterize-ipad-sim   (overnight.sh boots the pool for you)
```

DF only allocates among already-booted sims, so `boot-pool.sh` is the required first step for sim pools; it also warms each sim's WebDriverAgent so the first session doesn't cold-build WDA inside a launch timeout. See [`tools/appium-device-farm/README.md`](../../tools/appium-device-farm/README.md).

### Bundle IDs (overridable via `BundleIDs[platform]`)

| Platform | Bundle ID |
|---|---|
| iPhone / iPad / iPad sim | `com.jeoliver.InfiniteStreamPlayer` |
| Apple TV | `com.jeoliver.InfiniteStreamPlayerTV` |
| Android TV | `com.infinitestream.player` |

## Env vars

| Var | Default | Purpose |
|---|---|---|
| `HARNESS_BIN` | `harness` | path to the harness CLI binary |
| `HARNESS_INSECURE` | unset (=on) | disable with `HARNESS_INSECURE=0` against a public-cert deploy |
| `LAUNCH_MODE` | `cli` | `manual` \| `cli` \| `appium` |
| `CHAR_DEVICE_FARM` | unset | `1` routes `appium` launches through the device-farm plugin (capability allocation; see **Device Farm** above) |
| `CHAR_DF_IOS_VERSION` | latest installed | override the iOS `platformVersion` DF pins for sims (major.minor, e.g. `26.4`) |
| `CHAR_DF_TVOS_VERSION` | unset | pin a tvOS `platformVersion` under DF (sims only) |
| `CHAR_FLEET_COUNT` | `1` | fleet size — N parallel devices. Under DF these are logical (DF allocates); without DF, the first N booted sims of the platform |
| `CHARACTERIZATION_OUTDIR` | `t.TempDir()` | persistent artifacts directory (set for CI / aggregator use) |
| `CHARACTERIZATION_DEVICE_UDID` | unset = first-match | target a specific device by UDID. Use to run parallel tests across multiple sims of the same platform — each terminal exports its own UDID, no race for the same device. |
| `CHAR_RAMPUP_REPS` | `3` | rampup cycles per run, on ONE live play. Between cycles the cap drops top→floor and the player re-climbs — the inter-cycle transition is the instructive part. |
| `CHAR_RAMPDOWN_REPS` | `3` | rampdown cycles per run, on ONE live play. Between cycles the cap jumps floor→top and the player re-descends (buffer + variant carry across). |
| `CHAR_PYRAMID_REPS` | `2` | pyramid (up-then-down) cycles per run, on ONE live play. Confirms climb/descent behaviour is stable run-to-run. |
| `CHAR_OTEL_ENDPOINT` | unset | OTLP HTTP collector URL — e.g. `http://localhost:4318`. When set, every cycle emits an OpenTelemetry span to the configured backend (alongside the cycle_id label PATCH). See **Tracing** below. |
| `CHAR_OTEL_STDOUT` | unset | non-empty enables the stdout span exporter (verbose, debug only). |
| `CHAR_OTEL_DISABLE` | unset | non-empty forces the no-op tracer regardless of `CHAR_OTEL_ENDPOINT`. |

## Tracing (OpenTelemetry, issue #493)

Each cycle emits an OpenTelemetry `cycle` span, nested under a `test_run` span carrying the run-scope identity. Spans are **additive** to the existing cycle_id label PATCH — the dashboard's CycleBandsRail still reads from `control_events`; the spans are the cross-cycle / cross-run query surface.

Standard backends consume these directly: Tempo, Jaeger, Honeycomb, Datadog, Grafana Trace View, GitHub Actions OTel exporter.

**View locally with Jaeger** (no other infra required):

```sh
docker run --rm -d -p 4318:4318 -p 16686:16686 \
  --name jaeger jaegertracing/all-in-one:1
export CHAR_OTEL_ENDPOINT=http://localhost:4318

# run any characterization test
go test -C tests/characterization ./modes/... -v -run TestAbortIPadSim -timeout 30m -launch-mode=appium

# browse: http://localhost:16686 — Service: characterization
docker rm -f jaeger
```

**Span shape**:

- `test_run` (root) — attrs: `test`, `platform`, `run_id`, `clip_target` (where applicable)
- `cycle` (child × N) — attrs: `test`, `cycle_id`, `cycle_idx`, `rep`, `boundary` (startup-style) OR `fault` (abort/retry-style), `cap_mbps`. `status=error` on cycles that didn't meet pass criteria.

See `.claude/standards/characterization-principles.md § 9` for the cycle-label schema the spans mirror.

## Why a separate module

`tests/characterization` has its own `go.mod` so test-only deps (eventually Selenium / chromedp / testify) don't leak into the harness CLI or go-proxy binaries. The repo's `go.work` joins it for local builds.
