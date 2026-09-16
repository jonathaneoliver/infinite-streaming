# Appium Device Farm

A thin wrapper for running our Appium server with the
[`appium-device-farm`](https://github.com/AppiumTestDistribution/appium-device-farm)
plugin enabled, so Appium **arbitrates devices across independent test clients**
instead of each client hand-picking a UDID and dodging port collisions.

## Why

The characterization harness today talks to a plain Appium server, which does
**not** pool or allocate devices — every session must name its `appium:udid` and,
for parallel iOS, a unique `appium:wdaLocalPort` (else they all default to 8100
and collide). The harness works around this with `CHAR_FLEET_UDIDS` +
`CHAR_FLEET_PORT_OFFSET`. Two *independent* runs (e.g. a pyramid + a sweep loop)
still step on each other.

Device Farm replaces that manual allocation with real arbitration, like Selenium
Grid does for browsers:

- **Auto-discovery** of booted simulators + connected real devices.
- **Capability-based assignment**: a client requests `platformName=iOS` (and
  optionally `platformVersion`), Device Farm hands out a free matching device.
- **Queuing** when all matching devices are busy (no collision, no failure).
- **Automatic per-session WDA/MJPEG port allocation**.
- A **live dashboard** (device grid + session queue) at `/device-farm/`.

## Bring it up

```sh
tools/appium-device-farm/boot-pool.sh   # boot the sim pool (see below) — once per session
tools/appium-device-farm/run.sh         # start the DF server
```

Then open the dashboard: <http://localhost:4723/device-farm/>

Env overrides:

| Var | Default | Meaning |
|---|---|---|
| `DF_PORT` | `4723` | Appium port (the port the harness expects). |
| `DF_PLATFORM` | `both` | `ios` \| `android` \| `both`. |
| `DF_LOG` | `/tmp/appium-device-farm.log` | server log. |

`run.sh` frees `DF_PORT` first (stops any plain Appium already holding it), then
starts Appium with the plugin. The plugin is installed globally into the Appium
install on first run if missing:

```sh
appium plugin install --source=npm appium-device-farm
```

## Boot the pool

`bootedSimulators:true` means DF only allocates among ALREADY-BOOTED sims — the
booted set is the allowlist. `boot-pool.sh` is the "fire up N sims" step: it boots
N latest-OS Fleet sims, verifies the app is installed on each, and warms each
sim's WebDriverAgent via DF so the FIRST real session doesn't cold-build WDA and
blow a test's launch timeout. Idempotent — re-run anytime.

```sh
tools/appium-device-farm/boot-pool.sh
```

Env overrides:

| Var | Default | Meaning |
|---|---|---|
| `DF_POOL_COUNT` | `4` | how many sims to boot |
| `DF_POOL_MATCH` | `Fleet` | sim-name substring to pick from |
| `DF_POOL_OS` | latest installed | iOS runtime major.minor |
| `DF_BUNDLE_ID` | `com.jeoliver.InfiniteStreamPlayer` | app to verify |
| `DF_WARM_WDA` | `1` | warm WDA via DF (needs the server up) or skip |

WDA warming needs the DF server up, so for a cold start either run `run.sh` first
then `boot-pool.sh`, or run `boot-pool.sh DF_WARM_WDA=0`, start the server, and
re-run `boot-pool.sh` to warm.

## Using it from the harness

With Device Farm on `:4723`, a test no longer pins a UDID or a port — it requests
a platform (+ latest version for sims) and lets DF allocate. Turn it on with
`CHAR_DEVICE_FARM=1` (Stage 1). Under that flag the harness:

- requests by capability — drops `appium:udid`, `deviceName`, the FleetIndex
  WDA/MJPEG port offsets, and `derivedDataPath`; pins `appium:platformVersion` to
  the latest installed sim runtime (override: `CHAR_DF_IOS_VERSION` /
  `CHAR_DF_TVOS_VERSION`; hardware unconstrained);
- builds the fleet roster as N **logical** devices (`CHAR_FLEET_COUNT`, default 1)
  — no discovery/boot/per-UDID seeding, no `CHAR_FLEET_UDIDS` identities, no
  `staggerFleetLaunch` (DF queues). The server is set by the app's server-picker
  navigation in `LaunchToHome`, not the retired `seedFleetServer`.

## Notes

- Device Farm IS the Appium server — run **one** instance per port. Don't also
  run a plain `appium` on the same port.
- Real iOS devices need a signed WebDriverAgent (same prerequisite as plain
  Appium); simulators build WDA on demand. The first session per cold sim
  cold-builds WDA (~40 s) into DF's own DerivedData — `boot-pool.sh` warms it so a
  test's launch timeout isn't spent on the build.
- A test that dies before releasing its session leaves the sim `busy` (up to the
  2 h `newCommandTimeout`), so DF queues new requests against it. Unstick by
  reading the busy `sessionId`s from `GET /device-farm/api/device` and
  `DELETE /session/<id>` each, or just restart `run.sh`.
- Plugin version pinned at install time; re-run the install command to update.
