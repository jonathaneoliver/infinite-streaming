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
  iphone     Jonathans iPhone        EBB41BDC-...
  appletv    appletv                 7312834B-...

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
| `manual` | `-launch-mode=manual` (or `LAUNCH_MODE=manual`) | nothing | prompts the operator + observes via harness |
| `cli` (default) | `-launch-mode=cli` (or unset) | xcrun + simctl + adb on $PATH | kills + relaunches the app; relies on `skipHomeOnLaunch=true` for auto-resume |
| `appium` | `-launch-mode=appium` (or `LAUNCH_MODE=appium`) | Appium server + WDA + xcuitest/uiautomator2 drivers | Launch + Kill + Screenshot; full UI automation needs accessibility identifiers in the apps |

The `-launch-mode` flag is preferred over the env var — keeps `go` as the first token of the bash command so Claude Code (and similar tooling with command allowlists) doesn't re-prompt every invocation.

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
