# Progressive stall accumulation → non-recoverable wedge — iPhone a45a161d — 2026-05-20

## Summary

Under sustained pyramid bandwidth-shaping (12s steps, 0.998↔29.857 Mbps,
on a 100 Mbps baseline cap), the iPhone (iOS 26.4.2) ran cleanly for
~8 minutes, then accumulated four mid-play stalls of escalating
severity over the next ~10 minutes — culminating in a fourth stall
that AVPlayer did not recover from. The video froze on the last
decoded frame, the app process stayed alive, but metric POSTs to the
proxy ceased and the player vanished from `harness players list`.
No terminal `AVPlayerItem.error` was reported. **Tag: needs-test.**

Distinct from the Apple-TV first-cycle pinning behaviour (see
`716d59da-pyramid-cold-boot-pinning-2026-05-19`). This is the
*opposite* failure shape: the iPhone handled the first several
cycles correctly, then degraded.

## Timeline

All times PDT (UTC−7).

- **06:02:50** — play `222eff4f` starts. Variant climbs 720p → 2160p
  in ~3s. Buffer reaches 24s. `mbps_shaper_avg ≈ 91 Mbps` (100 Mbps
  baseline engaged).
- **06:03:30** (approx) — pyramid pattern applied on top of the 100 Mbps
  baseline. 11 steps × 12s = 132s/cycle.
- **06:11:01** — first stall. 8.3 seconds. Recovered at 360p (deep
  downshift from 2160p). Buffer rebuilt to 31s on 360p (more lead
  than 2160p had).
- **06:13:47** — second stall. **37.8 seconds**. Recovered at 360p.
- **06:15:43** — third stall. 21.4 seconds. Recovered at 360p.
- **06:20:33** — last heartbeat received. `last_state: stalled`. No
  recovery. App still on video screen showing frozen last frame.
- **06:21:48** (approx) — `info=*session_end` label fires server-side
  when proxy times out the dead session.

Stall-gap progression (time between consecutive stall ends):
2m46s → 1m56s. Trend was for stalls to come more frequently as the
test progressed.

Stall-duration progression: 8.3s → 37.8s → 21.4s → ∞ (no recovery).
Not monotonic — the 37.8s was the worst single stall, but the
fourth was the unrecoverable one.

## Evidence

Sibling JSON: `a45a161d-progressive-stall-wedge-2026-05-20.json`
(captured at moment of wedge confirmation, ~06:22 PDT).

### Play-level metrics

```
play_id      = 222eff4f-6d06-455d-abf8-42e48373a2aa
duration     = 17m 43s (06:02:50 → 06:20:33 PDT, single attempt)
stalls       = 4
bitrate_shifts = 43 (29 up / 14 down — typical iOS 2:1 asymmetry)
avg_quality  = 27.7%   (most of test spent on lower rungs)
min_quality  = 3.4%    (360p)
dropped_frames = 7
restart_count = 0
attempt_count = 1     (no retry / reload during the test)
last_state    = stalled
last_player_error = ""   (no terminal AVError)
```

### Label histogram (failure-relevant subset)

```
critical=*stall_severe_midplay  3        ← 3 of the 4 stalls were ≥4s
warning=fault_incomplete         1
warning=*transport_disconnect    1        ← only one client-side abort
warning=*segment_failure         1
warning=timejump                11       ← many recoveries seeked to live
warning=slow_segment             2
info=*pattern_step              81        ← ~7 full pyramid cycles ran
info=*session_end                1        ← proxy timed out the dead session
```

### Stall timeline (last_stall_time_s transitions)

```
ts (UTC)               last_stall  cumulative_stalls  variant_at_recovery
2026-05-20T13:11:01Z   8.252       1                  640x360
2026-05-20T13:13:47Z   37.756      2                  640x360
2026-05-20T13:15:43Z   21.369      3                  640x360
2026-05-20T13:20:33Z   (wedge)     4                  (frozen — no recovery)
```

### Buffer high-water marks across the test (refuting buffer-shrink hypothesis)

```
13:02:54  18.6s  2160x3840      ← initial climb
13:03:00  24.6s  2160x3840      ← top of first cycle
13:11:27  28.0s  640x360        ← after 1st stall — 360p with bigger buffer
13:11:32  29.2s  640x360
13:11:54  31.0s  640x360        ← BIGGER than the 2160p high-water
```

The buffer did NOT shrink across stalls — actually grew because the
later 360p variant required far less bandwidth to refill. So the
"each stall shrinks buffer headroom → next stall sooner" hypothesis
is **refuted** for this run.

## Hypothesis

Tag: **needs-test**.

Candidates ranked by plausibility:

1. **AVPlayer estimator-state corruption after many cap oscillations.**
   The iPhone is the only device that ran through ~7 full pyramid
   cycles before failing. Earlier devices (Apple TV) failed on the
   FIRST cycle for a different reason (cold-boot pinning). This
   suggests a different accumulator-state problem that emerges only
   after several cycles. Predicted by: progressive stall-frequency
   compression (which we observed: 2m46s → 1m56s gap). Confirmable
   by: running pyramid for >20 minutes on a baseline-cap iPad and
   seeing if the same progression appears.

2. **CFNetwork resource leak.** Each abandon / abort / mid-flight
   downshift may leave a small amount of decoder or socket state
   un-reclaimed. Eventually the pipeline can't process new segments.
   The single `warning=*transport_disconnect` (only 1 in 81 pattern
   steps) suggests this wasn't an abort-storm — but the leak could
   come from successful-but-discarded fetches too. Confirmable by:
   capturing `xcrun simctl spawn` / device console memory + CFNetwork
   warnings during the run (this iPhone is a real device, so
   `idevicesyslog -u <UDID> -n` would catch it).

3. **iOS 26.4.2 specific regression** in AVPlayer's recovery state
   machine when invoked >3 times in quick succession. Apple TV runs
   tvOS 26.4 (one minor older) and showed different failure mode.
   Distinguishing test: same scenario on iPhone with iOS 26.1 or
   26.3. Out of scope without a second test device.

## To confirm or refute

### Reproduction recipe

```sh
# Set realistic baseline first (mandatory — confirmed today that
# unlimited baseline produces different ABR estimator state).
harness --insecure shape <iphone> --rate 100

# Pyramid 12s steps, 100 Mbps baseline, observe ≥20 minutes.
harness --insecure shape <iphone> --pattern pyramid --step-seconds 12

# Watch metric POSTs and labels:
harness --insecure ts <iphone> --streams events,network,control --bundles events
```

**Expected if accumulator-state hypothesis (#1) is right**: stalls
appear after ~7-10 minutes, stall-gap compresses, around minute 15-20
the iPhone wedges in `state=stalled` without recovery and without a
terminal `last_player_error`.

**Expected if CFNetwork leak hypothesis (#2) is right**: alongside the
above, `idevicesyslog` should show CFNetwork memory warnings or
nw_endpoint_flow_failed messages in the minutes leading up to the
wedge.

### Recovery path

`harness undo <iphone>` rolls the pattern back. iPhone app needs to
be manually restarted (kill and relaunch) to clear the wedged
AVPlayer state — the play_id rotation timer also fires every 5 min
but cannot recover a wedged state (it can only mint a new play_id
for a session that's still POSTing heartbeats, which this one isn't).

## Action items

- [ ] Reproduce on iPad simulator with identical pyramid + 100 Mbps
      baseline + 20+ minute run. If same wedge appears, it's not
      iPhone-specific — it's a CFNetwork/AVPlayer family bug.
- [ ] If reproducible: capture `idevicesyslog` for the iPhone during
      the wedge window — specifically looking for CFNetwork warnings,
      memory pressure events, or nw_endpoint_flow_failed messages
      near the third stall.
- [ ] Consider whether the proxy should detect a "no heartbeats for
      90s but tcp still alive" pattern and emit a `critical=*wedge`
      label, since `info=*session_end` after a heartbeat timeout
      conflates "user closed the app cleanly" with "AVPlayer wedged."
- [ ] If confirmed: add note to `.claude/standards/avplayer-quirks.md`
      about progressive degradation under sustained ABR oscillation.

## See also

- `.claude/standards/avplayer-quirks.md` — iOS-specific reporting gaps
- `.claude/standards/abr-decision-model.md` — why downshifts cascade
- `716d59da-pyramid-cold-boot-pinning-2026-05-19.md` (if filed) —
  Apple TV's contrasting first-cycle failure mode
- Conversation log 2026-05-20 ~06:00 PDT — full session transcript
  including baseline / pattern setup choices
