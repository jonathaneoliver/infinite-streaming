# channel_change vs cold startup — the cold wedge is a cold-start artifact — 2026-07-14

## Observation
The fragile startup cell (iphone-sim · s1 · network 2 Mbps · initial variant cap
16 Mbps) has a wild cold-start distribution. Two questions answered here:
1. **Does the cold number settle with more data?** (convergence, n=40)
2. **Does keeping the app running — channel_change, no relaunch — change it?** (n=30)

**Verdict: cold is bimodal (median settles, mean doesn't — a ~1-in-8 catastrophic
tail); channel_change nearly eliminates that tail.** The cold wedge is a
cold-START artifact (no bandwidth estimate → over-selection), not a property of
the config. Tag: **confirmed** (iphone-sim; cold n=40, channel_change n=30).

## Numbers
| boundary | n | median | mean | CoV | tail ≥10 s | worst |
|---|---|---|---|---|---|---|
| **cold** (relaunch each play) | 40 | 5.86 s | 9.13 s | **127 %** | **12 % (5)** | **71.4 s** |
| **channel_change** (no app exit) | 30 | 5.25 s | 5.66 s | **33 %** | **3 % (1)** | **13.8 s** |

- Cold tail events: **14, 17, 18, 39, 71 s** (5 of 40). 35 of 40 cluster tight at ~5.9 s.
- channel_change: one 13.8 s outlier; the rest 3.6–9.6 s.
- **Tail 12 % → 3 %** (4× rarer), **worst 71 → 14 s** (5× less severe), **CoV
  127 % → 33 %**, and the **mean settles** (9.1 → 5.7 s, now tracking the median).

## Convergence (cold, n=40) — median settles, mean never does
Running mean lurched 5.9 → 9.7 → 10.8 → 9.9 → 9.3 → 9.1 s as tail events landed.
The **median converges hard (~5.9 s)**; the **mean is tail-dominated and can't
settle** — it's a tight fast mode (~87 %) plus a rare catastrophic mode, not
noise around a value. **Report the median, not the mean, for cells like this.**
The earlier n=3 "7 / 9 / 76 s" was just 2 fast + 1 tail.

## Mechanism (confirmed from fetch trajectories)
- **cold** rep: fetches **14.09 @ t=0** (over-selects the top rung — no bandwidth
  estimate), corrects down; occasionally the over-commit wedges → the tail.
- **channel_change** rep: fetches **1.54 @ t=0** — starts at the correct low rung
  because it **kept the ~2 Mbps estimate** from the prior play. No over-selection,
  so it rarely over-commits. This is why the tail collapses.

## New capability used — char-matrix rep-loop (#963)
`char_matrix_fleet_test.go` now runs **CHAR_REP_COUNT plays in ONE app launch**:
rep 0 is the cold-launch play (constraints configured — `is.peak_bitrate_mbps`
/ `is.segment` launch args + `proxy.shape` config-on-connect), and with
`CHAR_START_MODE=warm` each rep>0 ends the prior play → home and starts a NEW
play **without relaunching** (app keeps the cap/segment, proxy keeps the shape,
AVPlayer keeps its estimate) = channel_change. Each rep emits
`ARM N RESULT … rep=k mode=cold|channel_change`. This lets channel_change run on
the **exact** char-matrix config — including the initial variant cap the sweep
queue can't carry (`is.*` is a client-only knob it rejects). `start_mode: warm`
on the sweep queue / #946 is a *warm session* (reuses WDA, still cold-launches
the app → fresh AVPlayer) and is NOT this; verified it over-selects like cold.

## Consequence
- **The cold-start wedge is avoidable in-session.** An app that keeps AVPlayer
  alive across content switches (channel_change) sidesteps the over-selection
  wedge under a tight link — a real product lever, not just a test artifact.
- **n=1 / small-n on cold cells is unreliable** for the tail regime; use the
  median and quote the tail rate. See [[feedback_n1_not_a_pattern]].

## Method / repro
```
# channel_change convergence: 1 cold + (reps-1) channel_change plays per launch
CC_ARMS=4 CC_REPS=8 python3 $CLAUDE_JOB_DIR/tmp/conv_cc.py   # harness char matrix
#   yaml: iphone-sim, is.segment s1, is.peak_bitrate_mbps 16, proxy.shape rate_mbps 2
#   env:  CHAR_REP_COUNT=8 CHAR_START_MODE=warm
# cold convergence (relaunch each play): $CLAUDE_JOB_DIR/tmp/conv_sweep.py, n=40
# video-start via `harness query events <play_id>` (video_start_time_ms);
# fetch trajectory via video_bitrate_mbps over the first ~15 s.
```

## See also
- `.claude/findings/cold-start-rung-nondeterminism-2026-07-13.md` — the cold
  over-selection hedge is non-deterministic; this quantifies its tail + the
  channel_change fix.
- `.claude/findings/startup-initial-cap-vs-network-limit-2026-07-12.md` — the
  cap × limit × segment study this cell comes from.
- [[reference_avplayer_cold_start_wedge]] — the cold-start over-selection wedge.
