---
name: appium-farm
description: Setup / reset / shutdown the Appium Device Farm (the iOS-sim pool on :4723) and recover its known failure modes — "app unknown" despite an installed app, the config-on-connect 503 on a large fleet bootstrap, sims stuck busy after a killed run, a deleted sim lingering in the roster, cold WDA. Invoke when a fleet / char-matrix / sweep run fails to launch or bootstrap on the sims, or to return the farm to a known-good state before/after a run.
last_reviewed: 2026-07-01
---

# appium-farm — get the iOS-sim device farm back to a known-good state

One tool does everything: **`tools/appium-device-farm/farm.sh <status|setup|reset|shutdown|free|unblock>`**.
It wraps `run.sh` (the DF server) + `boot-pool.sh` (boot sims, install app, warm WDA) and adds the recovery steps this project learned the hard way.

**Conventions:** follows [`.claude/skills/CONVENTIONS.md`](../CONVENTIONS.md). Lead every command with `tools/...farm.sh` / `xcrun` / `curl` (first token matches the allowlist). Never `cd`/`VAR=…`-prefix — pass env inline (`DF_BUILD_APP=1 tools/.../farm.sh reset`).

## What "known-good state" means

DF up on :4723 · the `Fleet iPhone 15` sims **booted** with the app **installed** and **WDA warm** · **no** sim stuck `busy` · **no** orphaned go-proxy config-on-connect sessions holding pool slots · the real-iPhone `ios tunnel` and the `appium-mcp` server **still running** (never killed).

## Subcommands (what each does)

| Command | Does | Reach for it when |
|---|---|---|
| `status` | DF up? full roster; which `Fleet` sims are booted + app-installed; `ios tunnel` up?; **count of active go-proxy sessions** | first move — diagnose before acting |
| `setup` | start DF if down → `boot-pool` (boot sims + warm WDA; `DF_BUILD_APP=1` to also reinstall the app) → unblock | cold start / first run of the day |
| `free` | **stop the app on each sim** + **release orphaned go-proxy sessions** (`DELETE /api/session/<id>`) + unblock stuck devices — **no teardown** | the **config-on-connect 503** on a large-fleet bootstrap (pool exhausted by dangling sessions); a run left the proxy dirty |
| `reset` | the full known-good sequence: stop apps → free proxy sessions → kill DF/off-farm-appium/WDA/orphaned-runs → shut sims → restart DF → `boot-pool` (**`DF_BUILD_APP=1` default** = fresh app reinstall + WDA warm) → unblock | **"App … unknown"** despite the app being installed (stale WDA); a **deleted/stray sim lingering** in the roster (only a DF restart flushes it); accumulated churn |
| `shutdown` | stop apps → free proxy sessions → kill the DF stack → shut sims | done for the session |
| `purge-foreign` | **DELETE** every non-Fleet iPhone/iPad sim (the default Xcode sims Xcode auto-creates per runtime) + restart DF to flush the roster | the DF is pooling/allocating sims that aren't yours → `App … unknown` on sims without the app. Real devices + Fleet sims are never touched. |
| `unblock` | clear sims left `busy=true` by a `pkill`'d run | after you had to kill a run mid-flight |

## Symptom → command

- **`proxy returned 503` on every arm at bootstrap, before any sim is touched** → `free` (config-on-connect **pool exhaustion** — orphaned sessions; `runner/bootstrap_test.go:TestConfigOnConnectCapacity` documents the 503-when-not-released). If it persists, `reset`.
- **`App with bundle identifier '…' unknown`** while `xcrun simctl launch <udid> <bundle>` works fine → the DF allocated a **non-Fleet sim without the app**. Xcode auto-creates a full set of default sims per installed iOS runtime, `bootedSimulators: true` does **not** exclude them, and the DF will **boot one on demand** for `ipad-sim` (so just shutting them down fails — it re-boots them). Fix: **`purge-foreign`** (deletes them) or **`FARM_PURGE=1 farm.sh reset`**. They only come back when a new iOS runtime is installed / Xcode recreates them — re-run `purge-foreign` then.
- **A run got a sim that's shut down / a stray sim (e.g. an old "iPhone 17 Pro")** → `reset` (flushes the DF's in-memory roster; a plain `simctl delete` does NOT — the DF caches it).
- **`context deadline exceeded` acquiring a device / sims stuck `busy`** after a killed run → `unblock` (or `free`).
- **Cold WDA blows a launch timeout** on first session → `setup` (pre-warms WDA).

## Gotchas the script encodes (so you don't have to remember)

- **`pkill` skips `reapDeviceFarm`** → devices stay `busy=true`; `unblock`/`free`/`reset` clear them via `POST /device-farm/api/unblock`.
- **The go-proxy is REMOTE** (test-dev), not localhost — session release reads `HARNESS_BASE_URL` (default `https://dev.jeoliver.com:21000`). Set it if your deploy differs.
- **NEVER kill** `ios tunnel start` (the real-iPhone RemoteXPC tunnel — re-establishing it needs a USB re-pair, see [[reference_real_iphone_usbmux_pairing_wipe]]) or `appium-mcp`. The script's `pkill` patterns are scoped (`appium --config`, `appium --port 4799`, `xcodebuild.*WebDriverAgent`) to spare both.
- **`DF_BUILD_APP=1`** rebuilds the iOS app via xcodebuild (~5–10 min) — `reset` defaults to it (clean install); `setup` defaults to `0` (reuse installed, fast).

## Out of scope
Real-device (iPhone) tunnel setup (that's the USB-pairing recipe in memory), running the actual characterization/sweep, and result analysis. This skill only owns the sim-farm's health.
