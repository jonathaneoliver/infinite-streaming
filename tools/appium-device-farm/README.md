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
tools/appium-device-farm/run.sh
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

## Using it from the harness

With Device Farm on `:4723`, a test no longer needs to pin a UDID or a port — it
requests a platform and lets Device Farm allocate. To let the farm choose, run
*without* `CHARACTERIZATION_DEVICE_UDID` / `CHAR_FLEET_UDIDS` so the session is
matched by capability. (The harness's own fleet-port offset becomes redundant
once Device Farm owns port allocation.)

## Notes

- Device Farm IS the Appium server — run **one** instance per port. Don't also
  run a plain `appium` on the same port.
- Real iOS devices need a signed WebDriverAgent (same prerequisite as plain
  Appium); simulators build WDA on demand.
- Plugin version pinned at install time; re-run the install command to update.
