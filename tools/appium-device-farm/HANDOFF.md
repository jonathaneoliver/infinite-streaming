# Device Farm — harness simplification: handoff & plan

Status as of this handoff: **Device Farm is running and Stage 0 is validated.** The
next work is **Stage 1+ (simplify the characterization harness to lean on Device
Farm)**, not yet started. This doc is self-contained so a fresh session can pick
it up.

Branch: `feat/appium-device-farm` (off `origin/dev`). Worktree:
`/Users/jonathanoliver/Projects/smashing-device-farm`.

---

## 1. Goal (from the user)

> Auto-allocate devices — stop hand-picking UDIDs and offsetting ports. Two modes:
> (a) my **attached hardware** (1 iPhone + 1 Apple TV + 1 Android TV), or (b) **fire
> up multiple sims** (iPhone / iPad / Apple TV). All sims on the **latest** iOS/tvOS
> — never old runtimes.

Translation: the harness should request a device **by capability**
(`platformName` [+ latest `platformVersion`]) and let the `appium-device-farm`
plugin allocate, queue, and assign WDA/MJPEG ports — replacing the manual
`CHAR_FLEET_UDIDS` / `setXCUITestFleetPorts` plumbing.

---

## 2. What's already done

### Tooling (committed on `feat/appium-device-farm`)
- `257896b9` — `tools/appium-device-farm/run.sh` + `README.md` (launch wrapper).
- `537527cb` — `tools/appium-device-farm/appium.config.json` (the `bootedSimulators`
  allowlist config); `run.sh` launches via `appium --config`.
- `b48cd2c0` — dropped an `_comment` key (appium's config schema is strict).

### Running state (machine-global, not branch-isolated)
- Appium **3.4.2** + plugin **`device-farm@11.3.2`** installed into `~/.appium`.
- Server up on **`:4723`** via `tools/appium-device-farm/run.sh` (nohup; log
  `/tmp/appium-device-farm.log`). Dashboard: <http://localhost:4723/device-farm/>.
- Config in effect (`appium.config.json`): `platform/iosDeviceType/androidDeviceType:
  both`, **`bootedSimulators: true`**.
- Pool = **4 booted Fleet iPhone 15 sims on iOS 26.4** (the app is installed on all 4):
  - `#1 4D62CB39-BAB7-4294-99D7-8E28FBCD0FF0`
  - `#2 0EA208D3-6E04-48BF-8309-F6ACAF383A59`
  - `#3 7C6110A4-754C-47DA-B225-E95ED11F9F60`
  - `#4 B3A40CBF-87F4-414C-9C01-6A756060DBDF`
- Latest installed runtimes: **iOS 26.4, tvOS 26.4** (`xcrun simctl list runtimes`).

To (re)start the server: `tools/appium-device-farm/run.sh` (boot the Fleet sims
first — see §4 decision 3).

---

## 3. Stage 0 findings (these are the design constraints)

1. ✅ **`platformVersion` filtering works.** Requesting `26.4` only ever selects
   26.4 devices; the old 26.1 iPad was excluded.
2. ✅ **DF allocates by capability + arbitrates the pool.** Two `POST /session`
   with **no UDID** landed on **different** Fleet sims (#2 then #3), each with
   distinct **auto-assigned** ports (53708/53707, then 54172/54171), each running
   our actual app. No collisions, no hand-picking.
3. ✅ **`bootedSimulators: true` is the reliable model.** It restricts DF to
   already-booted sims (managed pool dropped 35 → 7). The booted Fleet set becomes
   the de-facto **allowlist**.
4. ⚠️ **DF's on-demand cold-boot is UNRELIABLE on this box.** Without
   `bootedSimulators`, DF naively picked a never-booted sim (iPhone 17 Pro 26.4)
   over free booted ones and the cold boot **timed out (120 s) / failed (code 22)**.
   → Don't rely on DF cold-booting; pre-boot the pool.
5. ⚠️ **Every pooled sim must have the app installed.** A booted sim without
   `com.jeoliver.InfiniteStreamPlayer` would be allocated and then fail at launch.
   Keep the pool = app-installed sims only.
6. ✅ **Attached hardware needs no special handling** — real devices are always
   "booted", so `iosDeviceType/androidDeviceType: both` picks them up.

---

## 4. Design decisions already made (recommendations the user accepted/leaned to)

1. **Latest-OS pinning** — compute the newest installed runtime per platform at
   runtime (`simctl list runtimes`), pin `appium:platformVersion` for **sims**;
   leave **hardware** unconstrained (a physical phone runs what it runs). Make it
   overridable: `CHAR_DF_IOS_VERSION` / `CHAR_DF_TVOS_VERSION`.
2. **Seeding → UI server-picker fallback.** Drop the per-UDID
   `seedFleetServer`/`simseed` path entirely; rely on the existing
   `navigateServerPickerIfPresent` (it needs no UDID up front, fits capability
   allocation). Seed the picker with **`https://dev.jeoliver.com:21000`**
   (cert-valid; `.local` gives an empty catalogue on sims — known TLS-hostname trap).
3. **`bootedSimulators: true` pool model.** The harness/operator boots the
   latest-OS, app-installed Fleet pool (a small "boot the pool" helper), then DF
   allocates among them. "Fire up N sims" = boot N Fleet sims.
4. **Gate behind `CHAR_DEVICE_FARM=1`; keep the plain-appium path as fallback.**
   Don't delete discovery/fleet/port code — gate it. DF path becomes the default
   for appium once proven; non-DF stays for CI / no-DF machines. Reversible, low
   blast radius.
5. **Keep `FleetIndex` as a *logical* index** (A/B arm assignment + fleet
   barriers), no longer device/port-bound. Assign arm[i] by session-creation
   order, not device identity.

---

## 5. Appium inventory — what changes vs stays

(Full map was done; key files below.)

**Becomes redundant under Device Farm (gate/remove in DF mode):**
- `tests/characterization/runner/appium.go`
  - `appiumCapabilities()` (~L935): `appium:udid` (L939), `setXCUITestFleetPorts()`
    (L963/987/991 → def L1012), `iosWDADerivedDataPath()` (L1027) per-index.
  - `CHAR_FLEET_PORT_OFFSET` (in `setXCUITestFleetPorts`).
- `tests/characterization/modes/fleet.go` — device discovery + roster:
  `CHAR_FLEET_UDIDS/COUNT/AUTOBOOT/STAGGER/SEED_SERVER`, sim booting,
  `seedFleetServer` (L376), `staggerFleetLaunch`.
- `tests/characterization/runner/simseed.go` — `SeedServerProfile` (replaced by UI
  picker).
- `CHARACTERIZATION_DEVICE_UDID` pinning (`fleet.go`).

**Must stay (above the allocation layer):**
- Config-on-connect / `wireConfigOnConnect` (`sweep.go` L91), `ConfigureOnConnect`
  (`bootstrap.go`) — mints `player_id`; app launches with `-is.player_id`; device-
  agnostic.
- A/B arm treatment — `armContentConfig` (`sweep.go` L221; note: the
  `CHAR_ARM_<i>_STRIP_AVG_BW` arm lives in the **`feat/char-strip-avgbw-arm`**
  branch / `smashing-char-avgbw` worktree, `96f96b7e`, not here).
- Fleet barriers (home/sweep sync), UI driving (`TapByAccessibilityID`,
  `ResumePlayback(Clip)`, `ReadPlayerID`, `SetSegmentLength`,
  `navigateServerPickerIfPresent`), `Screenshot`, `CHAR_CONTENT`.

**Callers (all 7 modes type-assert `*AppiumLauncher`, skip otherwise):**
`startup`, `pyramid`, `rampup`, `rampdown`, `fault_recovery`, `state_residency`,
`playback_end` `_test.go` + `sweep.go` helpers. Plus `overnight.sh`, `Makefile`
`characterize-*` targets, `tools/qe-offhours.sh`.

---

## 6. The staged plan

### Stage 1 — capability-based launch path (gated)
- Add `CHAR_DEVICE_FARM=1` detection (helper in `runner`, e.g. `deviceFarmEnabled()`).
- In `appiumCapabilities()`, when DF mode: **omit** `appium:udid`,
  `setXCUITestFleetPorts`, `appium:derivedDataPath`; **set** `platformName` +
  `appium:platformVersion=<latest computed>` (sims) — hardware unconstrained.
- After `createSession`, **read back the allocated UDID** from the returned session
  caps (`value.capabilities.udid` / `appium:udid`) and stash it on the `Session`/
  `Device` so logs, screenshots, and `ReleaseDevice` still work.
- `FleetIndex` stays logical (arm + barriers).
- **Smoke-test after Stage 1** before touching modes: run one mode (e.g.
  `TestStartupIPadSim`) with `CHAR_DEVICE_FARM=1`, no `CHAR_FLEET_UDIDS`.

### Stage 2 — seeding + retire port plumbing
- Under DF, seed via `navigateServerPickerIfPresent` (already called in
  `LaunchToHome`); drop `seedFleetServer`/`CHAR_FLEET_SEED_SERVER` from the DF path.
- Retire `CHAR_FLEET_PORT_OFFSET` + `staggerFleetLaunch` under DF (DF queues).
- Add a small **"boot the pool"** helper: boot N latest-OS Fleet sims (+ verify the
  app is installed) so "fire up N sims" is one command before the run.

### Stage 3 — migrate callers
- Point the 7 modes + `overnight.sh` / `Makefile` `characterize-*` at the DF path
  (default when `CHAR_DEVICE_FARM=1`). Keep plain-appium fallback intact.
- Update `tests/characterization/README.md` (+ this dir's `README.md`).

---

## 7. Open questions / risks

- **App-on-pool invariant:** pool must be app-installed sims only. The "boot the
  pool" helper should install the app if missing (or fail loudly).
- **A/B-by-index under capability alloc:** assign arm config by the order sessions
  are created, since you no longer choose which physical device is "index 1".
- **DF picking app-less booted sims:** if any non-Fleet sim is booted, DF may grab
  it. Keep the booted set clean, or use the `simulators` allowlist array in
  `appium.config.json` (schema confirms the key exists; element shape unverified —
  `bootedSimulators` was chosen instead because it's unambiguous).
- **Wedged sim:** `iPhone 17 Pro 26.4` (`BFC34E3A-638C-4921-A94D-CB6A64323829`)
  failed to boot cleanly; `simctl erase` it if it's ever wanted in the pool.
- **tvOS / Android TV pools:** only iOS is set up. tvOS sims would need booting too;
  attached Apple TV / Android TV appear automatically when connected.
- **Concurrent sessions on this machine** (a `TestSweepProbe` loop has run here):
  DF arbitration now prevents the 8100 port collisions that previously forced
  `CHAR_FLEET_PORT_OFFSET` — a bonus of this work.

---

## 8. Quick reference

```sh
# (re)start Device Farm (boot the Fleet pool first)
for u in 4D62CB39-BAB7-4294-99D7-8E28FBCD0FF0 0EA208D3-6E04-48BF-8309-F6ACAF383A59 \
         7C6110A4-754C-47DA-B225-E95ED11F9F60 B3A40CBF-87F4-414C-9C01-6A756060DBDF; do
  xcrun simctl boot "$u" 2>/dev/null
done
tools/appium-device-farm/run.sh        # serves :4723 + /device-farm dashboard

# capability-only allocation smoke test (no UDID, latest OS, our app)
curl -s -X POST http://localhost:4723/session -H 'Content-Type: application/json' -d '{
  "capabilities":{"alwaysMatch":{"platformName":"iOS","appium:automationName":"XCUITest",
  "appium:platformVersion":"26.4","appium:bundleId":"com.jeoliver.InfiniteStreamPlayer",
  "appium:noReset":true,"appium:forceAppLaunch":true,"appium:newCommandTimeout":300}}}'
# → allocates a booted Fleet sim, auto-assigns ports, launches the app. DELETE /session/<id> to release.
```

App bundle ids (`runner/simseed.go`): iOS/iPad `com.jeoliver.InfiniteStreamPlayer`,
Apple TV `com.jeoliver.InfiniteStreamPlayerTV`, Android TV `com.infinitestream.player`.
Harness base URL for seeding: `https://dev.jeoliver.com:21000` (`HARNESS_BASE_URL`).
