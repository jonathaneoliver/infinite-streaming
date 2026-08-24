# Orphaned WDA xcodebuild blocks new sessions as XCTDaemon Code=41

**Observed** 2026-08-21, real iPhone (`00008120-000242DE1152201E`), Appium 3.4.2.
**Disposition:** confirmed — killing the orphan fixed it on the next attempt.

## Symptom

Every `POST /session` failed:

```
Unable to start WebDriverAgent session. Original error: A new session could not
be created. Details: Error Domain=XCTDaemonErrorDomain Code=41
"Not authorized for performing UI testing actions."
```

## Why this misleads

Code=41 is documented everywhere as *the device is locked* or *Settings →
Developer → Enable UI Automation is off*. Both are device-side, so the error
sends you to the phone. Here both were already correct and the fault was on the
host — an hour was spent asking the operator to unlock the phone and hunt for a
settings toggle that was already on.

The host side all looked healthy, which reinforced the misread:

- `ios tunnel ls` — up, RSD port live
- `ios list` — device present
- `curl localhost:4799/status` — `ready: true`

## Cause

A `xcodebuild build-for-testing test-without-building` process for
WebDriverAgentRunner, **orphaned from a previous Appium instance**, still held
the device's XCTest session. The daemon permits one, so it refused the new one —
and expresses that refusal as "not authorized" rather than "already in use".

The tell, visible in `ps` from the start: the orphan was **older than the running
Appium**. Appium started 14:52; the xcodebuild started 18:15 the previous day.
Nothing Appium spawned can predate Appium, so that process belonged to a dead
parent and nothing was going to reap it.

## Check, before touching the phone

```sh
ps -o pid,lstart,etime,command -p "$(pgrep -f 'xcodebuild.*WebDriverAgent' | head -1)"
```

If its start time precedes the running Appium (`pgrep -f 'appium --port'`), it is
an orphan. Kill it and retry:

```sh
pkill -f 'xcodebuild.*WebDriverAgent'
```

Confirm with a session that reads the home screen rather than a status ping —
`/status` returns ready regardless, because the refusal happens on the device:

```sh
cd tests/characterization && go run ./cmd/demo-device -platform iphone -list-tiles
```

## Distinguishing it from the genuine device-side causes

Read the Appium log, not the client's truncated error. If it contains a line
tagged `[Xcode]` from `WebDriverAgentRunner-Runner[<pid>]`, **WDA reached the
phone and launched** — which already proves pairing, signing, Developer Mode and
the tunnel are fine. A locked screen or a missing UI-Automation toggle can still
produce Code=41 at that point, but an orphan holding the session is the cause
that host-side checks cannot see, so eliminate it first.

Related: `.claude/memory/reference_real_iphone_usbmux_pairing_wipe.md` covers the
genuinely device-side failure after an iOS update, where the device vanishes from
Appium entirely rather than refusing a session.
