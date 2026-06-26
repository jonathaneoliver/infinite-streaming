# Real iPhone on appium-device-farm — why we abandoned it for the hybrid

**Observed behaviour:** trying to drive a real iPhone *through* the
appium-device-farm plugin (so it's a first-class farm citizen alongside the
sims) fails on iOS 17+/26. We peeled through five real layers, fixed each, and
hit a sixth that's a **plugin gap** in appium-device-farm 11.3.2. We pivoted to
a **hybrid**: drive the real iPhone via a plain Appium (off-farm) on the
xcodebuild WDA path, while sims stay on the farm. The working code lives on
`feat/mixed-platform-fleet` (`d32b2c6f`). This branch (`ce7c5aff`) keeps the
farm-allocation approach so the attempt isn't lost.

Device: "Jonathans iPhone", hw UDID `00008120-000242DE1152201E`, iOS 26.5,
connected over **WiFi** (`transportType: localNetwork`). Farm Appium on :4723.

## Device prerequisites — all confirmed OK (this was never the problem)

`xcrun devicectl device info details --device <udid>`:
- `developerModeStatus: enabled`
- `ddiServicesAvailable: true` (Developer Disk Image mounted)
- `pairingState: paired`

That's why no passcode/Trust prompt ever appeared — the one-time steps were
already done. (Confirmed independently: the standalone xcodebuild path brings WDA
up fine.)

## The layered blockers (each fixed, in order)

1. **WDA must be resigned for a team that provisions THIS device.** The farm's
   `wda-resign.ipa` was mis-signed (an `826LZR9BXT` cert against a `63328J83Q8`
   profile, host id `com.facebook.WebDriverAgentRunner.xctrunner`) → invalid → the
   device rejects the install → downstream `appBundleId` is null.
   - Fix: build WDA unsigned, then applesign / xcodebuild auto-sign with team
     **`63328J83Q8`** (owns `com.jeoliver.*`, wildcard profile `63328J83Q8.*`,
     the iPhone is in its ProvisionedDevices). Host id
     `com.jeoliver.WebDriverAgentRunner.xctrunner`, xctest id
     `com.jeoliver.WebDriverAgentRunner`. Only the `826` "Apple Development" cert
     is in the keychain locally, but xcodebuild automatic signing
     (`-allowProvisioningUpdates`) mints/uses the right one for 63328.
   - Asset path: `~/.cache/appium-device-farm/assets/wda-resign.ipa`.

2. **`DeviceHelper.js:536` crash — `Cannot destructure property 'appBundleId' of
   '(intermediate value)' as it is null`.** Decoded from the obfuscated bundle,
   the farm does:
   `const { appBundleId } = await prisma.appInformation.findFirst({ where: { fileName: "wda-resign.ipa" } })`
   then builds a go-ios `runwda` command. The `AppInformation` table was **empty**
   because the ipa was copied into the assets dir, not uploaded through the
   dashboard (which records the row). `findFirst` → null → destructure crash.
   - Fix: insert the row into the Prisma sqlite DB
     `~/.cache/appium-device-farm/device-farm-latest.db`:
     ```sql
     INSERT INTO AppInformation (id, fileName, uploadedFileName, path, platform, fileSize, appBundleId)
     VALUES ('<uuid>', 'wda-resign.ipa', 'wda-resign.ipa',
             '/Users/<you>/.cache/appium-device-farm/assets/wda-resign.ipa',
             'ios', '<bytes>', 'com.jeoliver.WebDriverAgentRunner.xctrunner');
     ```
   - NOTE the global plugin config `wdaBundleId` (appium.config.json) is read into
     pluginArgs but is NOT copied onto the device record / used here — it does not
     fix this. The DB row is what matters.

3. **Device must be unlocked.** `devicectl process launch` is denied on a locked
   device: `Unable to launch ... because the device was not, or could not be,
   unlocked (FBSOpenApplicationErrorDomain error 7 (Locked))`. Set Auto-Lock →
   Never.

4. **iOS 17+/26 RemoteXPC tunnel.** Appium logs `Tunnel registry port not found.
   Please run the tunnel creation script first`. appium-xcuitest needs a
   CoreDevice secure tunnel + tunnel registry (default port **42314**).
   - Fix: `appium-ios-remotexpc` ships it (needs sudo for TUN):
     ```
     cd ~/.appium/node_modules/appium-xcuitest-driver/node_modules/appium-ios-remotexpc
     sudo node scripts/tunnel-creation.mjs --keep-open
     ```
     Verify: `GET http://127.0.0.1:42314/remotexpc/tunnels` shows the device's
     tunnel (`connectionType: Network`, an `rsdPort`). A boot-time LaunchDaemon to
     automate it is in the conversation (Label `com.appium.ios-tunnel`,
     `node scripts/tunnel-creation.mjs --keep-open`, KeepAlive + 30s throttle).
   - Necessary but **not sufficient** — see 5/6.

5. **The farm launches WDA as a plain app, not as a test.** With `GO_IOS` unset
   the farm falls back to `launchWithPreinstalledWDA` →
   `xcrun devicectl device process launch ... com.jeoliver.WebDriverAgentRunner.xctrunner`.
   That launches the WDA *host app* (the "flash" on the phone) but does NOT run
   the embedded `WebDriverAgentRunner.xctest`, so WDA's HTTP server never starts →
   Appium loops `RemoteXPC upstream connect error: Connection failed with code 2`
   and tears WDA down. WDA only runs as a test via go-ios `runwda` (testmanagerd)
   or xcodebuild.

6. **TERMINAL BLOCKER — go-ios `runwda` needs a tunnel the farm never starts, on a
   port it picks dynamically.** Install go-ios (`npm i -g go-ios`, binary `ios`),
   set `GO_IOS=<path>/ios` + `ENABLE_GO_IOS_AGENT=user` (the iPhone is on WiFi, so
   go-ios needs its userspace agent to even enumerate it). The farm then runs:
   ```
   ios runwda --udid=... --bundleid=com.jeoliver.WebDriverAgentRunner.xctrunner
     --testrunnerbundleid=com.jeoliver.WebDriverAgentRunner.xctrunner
     --xctestconfig=WebDriverAgentRunner.xctest --env USE_PORT=... --tunnel-info-port=53090
   ```
   which fails: `cannot create a tunnel connection to testmanagerd: missing tunnel
   address and RSD port. To start the tunnel, run 'ios tunnel start'`. go-ios needs
   its OWN tunnel daemon (`ios tunnel start`, tunnel-info API default :28100). But
   **the farm passes `--tunnel-info-port=53090` from its dynamic port pool** (same
   family as that session's `wdaLocalPort`/`mjpeg`), and **never starts a go-ios
   tunnel there** (the log shows only the `runwda` call). An operator cannot
   pre-start a daemon on an unpredictable per-session port. That's a contradiction
   baked into appium-device-farm 11.3.2's go-ios real-device path on iOS 26.

## Decision

Short of patching the obfuscated device-farm bundle to start `ios tunnel start`
on the port it allocates (forking/maintaining a third-party dep, with a second
tunnel stack to coordinate against appium-ios-remotexpc), the pure-farm path is
blocked. The standalone **xcodebuild path works in one step** (WDA runs as a
test, server on :8100, no Prisma row / go-ios / tunnel daemon, self-healing
signing). So we drive the real iPhone via a plain Appium off-farm and use the
farm's `userBlocked` flag only as a cross-run lock — see `feat/mixed-platform-fleet`
(`runner/farmlock.go`, the hybrid routing in `runner/appium.go`).

## To resume the farm path later

The remaining work is entirely in appium-device-farm's real-device launch: make
it (a) start `ios tunnel start --tunnel-info-port=<the port it passes>` (or a
fixed port + a persistent daemon) before `runwda`, and (b) reconcile that go-ios
tunnel with the appium-ios-remotexpc tunnel used to reach WDA's HTTP server.
Everything below that (signing, the Prisma row, unlock, the remotexpc registry)
is already understood and reproducible from the notes above.
