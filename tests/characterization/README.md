# Player characterization framework

Go test suite replacing the python `test_player_characterization_pytest.py`. Drives the harness CLI to apply network shapes and samples the v2 stream to characterize player ABR behaviour across iPhone / iPad / Apple TV / Android TV / Web.

Tracks issue #482. Roku is out of scope.

## Status

Phases 0, 0.5, 1, 2, 3, 5 landed: scaffolding, both `Manual` + `CLI` launchers, all 7 characterization modes wired per platform, and the aggregator binary. Phase 4 (Appium) is the optional upgrade and not yet implemented.

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

# Smooth sweep on first iPad sim found — no env vars needed.
# Artifacts land in tests/characterization/artifacts/.
go test -C tests/characterization ./modes/... \
    -v -run TestSmoothIPadSim -timeout 90m -count=1

# Unit + smoke (no live player needed)
go test -C tests/characterization ./runner/... -v

# Target a specific device (e.g. for parallel runs across two sims)
CHARACTERIZATION_DEVICE_UDID=8C792303-...                       \
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

## Launch modes

The framework supports three launch modes, picked by `$LAUNCH_MODE`:

| Mode | Env | What it needs | What it does |
|---|---|---|---|
| `manual` | `LAUNCH_MODE=manual` | nothing | prompts the operator + observes via harness |
| `cli` (default) | `LAUNCH_MODE=cli` or unset | xcrun + simctl + adb on $PATH | kills + relaunches the app; relies on `skipHomeOnLaunch=true` for auto-resume |
| `appium` | `LAUNCH_MODE=appium` | Appium server + WDA + xcuitest/uiautomator2 drivers | full UI automation |

`manual` and `cli` are implemented. `appium` is the optional Phase 4.

The CLI launcher expects each player app to be built with `skipHomeOnLaunch=true` so a cold launch picks up `lastPlayed` automatically — that's how it can recover from a wedged player without driving UI.

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
| `CHARACTERIZATION_OUTDIR` | `t.TempDir()` | persistent artifacts directory (set for CI / aggregator use) |
| `CHARACTERIZATION_DEVICE_UDID` | unset = first-match | target a specific device by UDID. Use to run parallel tests across multiple sims of the same platform — each terminal exports its own UDID, no race for the same device. |

## Why a separate module

`tests/characterization` has its own `go.mod` so test-only deps (eventually Selenium / chromedp / testify) don't leak into the harness CLI or go-proxy binaries. The repo's `go.work` joins it for local builds.
