# Abort characterization test

What the test measures, what each field means, and how to interpret a row of results. **Read this before reasoning about an abort_test result OR before editing `tests/characterization/modes/abort_test.go`.**

Related — the wire-level behaviour of each fault type is documented separately at `.claude/standards/fault-injection-wire-contract.md`. Read that first; this doc covers the test scaffolding around those wire shapes.

## What the test is doing

Characterizes the player's response to a single mid-segment fetch failure. Five fault SHAPES are tested per "rep"; each rep walks all five in order. With the default `CHAR_ABORT_REPS=1`, the test produces 5 cycles; `CHAR_ABORT_REPS=3` produces 15.

| Shape | Mechanism | Wire shape (full detail in fault-injection-wire-contract.md) |
|---|---|---|
| `server_timeout` | `transfer_active_timeout=5s` + `ApplyRate(0.1 Mbps)` | Real upstream bytes flow slowly; proxy closes after 5s mid-body → FIN |
| `request_first_byte_hang` | one-shot `fault add --type request_first_byte_hang --kind segment` | Proxy accepts → headers → silent socket for `socketHangDuration` → FIN |
| `request_first_byte_delayed` | one-shot fault rule | Headers → wait `socketDelayDuration` → FIN |
| `request_body_reset` | one-shot fault rule | Headers + ~64 KB of real upstream bytes → RST |
| `request_body_hang` | one-shot fault rule | Headers + ~64 KB of real upstream bytes → silent socket → FIN |

Each cycle:

1. Apply `100 Mbps` cap, clear faults/timeouts.
2. `WaitForTopAndBuffer` (current top variant + buffer ≥ 15 s).
3. Snapshot the pre-arm state (variant, buffer, position, the player's own bandwidth estimate).
4. Arm the fault (one-shot: `frequency=0 consecutive=1 mode=requests`, URL-scoped to the video variant directories only — audio segments excluded; see `runner.VideoVariantDirs`).
5. Observe 30 s — sampler collects player metrics, the test queries `network_requests` post-window.
6. Clear faults, release rate cap, wait for recovery (variant + buffer back).
7. → next shape.

Default: `CHAR_ABORT_REPS=1` (5 cycles, ~5 min wall clock). Override with `CHAR_ABORT_REPS=N` for repeatability.

## Per-cycle fields — what each one means

| Field | Source | What it tells you |
|---|---|---|
| `cycle_idx` | enumerated 1..N | log + report cross-reference |
| `fault_shape` | test config | the shape being exercised this cycle (vocab matches the wire contract doc) |
| `pre_variant` | `player_metrics_video_resolution` at the moment of arming | the variant the player was on before the fault hit |
| `pre_buffer_s` | `player_metrics_buffer_depth_s` at arming | buffer cushion before the fault |
| `pre_bw_est_mbps` | `player_metrics_network_bitrate_mbps` at arming | player's own bandwidth estimate before the fault — useful to compare against post-recovery |
| `armed_at` | wall clock of the `ArmFault` call | t=0 for the cycle's observation window |
| `abort_detected` | bool: did `ObserveAbortCycle` find a row matching the fault in the window | NO = the fault didn't bite (rule mis-scoped, or no segment fetch in the window). False values invalidate everything else in the row. |
| `abort_kind` | `fault_type` or `fault_action` from the matching network row | should match the armed `fault_shape`. Mismatch indicates a proxy bug. |
| `abort_at_s` | seconds from `armed_at` to the abort row's `ts` | how quickly the proxy applied the fault — typically <1s |
| `abort_url` | the URL the fault hit | confirms it was a video segment (not audio — scope should prevent audio) |
| `retry_found` | did a later network row hit the same `abort_url`? | reveals whether the player re-requests after the abort |
| `retry_had_range` | if a retry was found, did it carry an HTTP `Range` header? | reveals whether the player attempts partial-resume vs full refetch |
| `retry_range_start` | the `Range` start byte if `retry_had_range` is true | resume offset |
| `player_stalled` | bool: did any sample show position frozen >5s post-arm | did the user experience a stall |
| `downshifted_to` | first sample with a different `video_resolution` from `pre_variant` (post-arm) | what variant the player dropped to after the abort. Empty = no downshift. |
| `downshift_after_s` | when that resolution change happened, seconds from armed_at | the player's reaction time to the abort signal |
| `recovery_s` | seconds for `WaitForTopAndBuffer` to succeed after fault is cleared | how long the player took to climb back. 90s typical when downshift happened (the climb is slow); near 0s when no downshift. |
| `post_bw_est_mbps` | bandwidth estimate on the LAST sample of the observation window | what the player believes the link will sustain after the fault |

## How to interpret a row

Walk through these in order when reading a cycle's result:

1. **Did the fault actually fire?** `abort_detected = true` is the minimum signal. If false:
   - Check the in-cycle log lines — was there a "fault add" CLI error?
   - Check `network_requests` for the play_id in the window: any segments at all? If not, the player wasn't fetching (buffer too deep). If yes but none had `fault_type`, the rule mis-scoped (URL filter excluded everything, or the kind didn't match).
   - Anything else in the row is meaningless if abort_detected is false.

2. **Was it the right kind of fault?** `abort_kind` should equal `fault_shape` (with the exception that `server_timeout` produces `kind=transfer_active_timeout` — that's the wire-level vocab match). Mismatch means the proxy applied a different fault than asked — investigate the rule definition.

3. **Did the player retry?** `retry_found`. With most fault shapes the player WILL re-issue a GET for the same URL within a few seconds — that's the canonical recovery path. If retry_found is false, the player abandoned the URL (skipped that segment in playback, moved on). The behavior is platform-specific:
   - iOS AVPlayer: typically retries from byte 0 (no Range), then if that fails, downshifts.
   - hls.js: typically Range-resumes (retry_had_range = true).

4. **Did the player downshift?** `downshifted_to`. Empty = the player held its variant choice (either the fault was mild enough to ignore, or it gave up entirely on the segment without learning). A non-empty value = the player took the fault as a bandwidth signal:
   - One rung down = mild reaction (typical for `*_delayed`, `*_reset`).
   - Two rungs down = stronger signal (we've observed this for `*_hang` on iOS).
   - Skipping to the lowest variant = aggressive defense, unusual.

5. **Did the cycle stall the user?** `player_stalled = true`. Any true is operationally important — the user experienced a freeze. Cross-reference with `pre_buffer_s`: a stall from a 30 s buffer is much more concerning than a stall from a 0 s buffer.

6. **How long to recover?** `recovery_s` should be < 5 s when no downshift; 60–120 s when the player has to climb back from a downshift (each variant step takes ~10–20 s to clear and the cap-detection takes longer).

## Anomalous outcomes — what's interesting

- `abort_detected=false` on a `kind=segment` fault rule WHILE network rows show segments flowing — the URL scope is wrong. Pre-#491 fix, this happened when `segment_failure_urls` was empty. Post-fix, it should never happen unless the v2 fault_rules → v1 surface translator regresses.
- `retry_had_range=true` on iOS — surprising; would mean AVPlayer implemented Range-resume. None observed to date.
- `downshifted_to=""` on `request_body_*` shapes — the player ignored a mid-body abort. Suggests something is masking the failure signal (kept-alive HTTP/2 stream sticking around, or the player's failure-classifier collapsing the abort into "transient").
- `pre_buffer_s = 0` AND `player_stalled = false` — the cycle ran without a buffer cushion (test framework didn't wait long enough or the previous cycle's recovery didn't fully restore), but the player coped anyway. Often a sign the test wait-loop needs tuning, not a player behavior insight.

## Real-world correspondence

Each fault shape corresponds to a real-world failure mode — full detail in `fault-injection-wire-contract.md`. Summary:

- **server_timeout** ↔ ISP / cell-network slowdown that exceeds server's read deadline. Cell tower edge, congested WiFi.
- **request_first_byte_hang** ↔ server accepts but stalls (backend thread blocked, DB lock, slow signing).
- **request_first_byte_delayed** ↔ CDN cache miss → origin fetch → slow re-warm.
- **request_body_reset** ↔ mid-transfer server crash, load-balancer kill, TCP reset by network gear.
- **request_body_hang** ↔ stuck mid-stream (recv-window deadlock, OOM-on-the-cusp server).

(`request_body_delayed` was specced but cycles in the abort test default to the five above.)

## Anomalous-result diagnosis quick reference

| Symptom | Likely cause | Check |
|---|---|---|
| `abort_detected=false` across multiple cycles | URL filter scope is misconfigured | `segment_failure_urls` in session state should be non-empty AND match the segment paths |
| Same `downshift` across reps but different `recovery_s` | Recovery dependent on cumulative player state, not just the fault | Compare `pre_buffer_s` across reps — if degrading, the test framework's between-cycle recovery wait is too short |
| `pre_buffer_s = 0` consistently from cycle N onward | Player state degraded permanently — fault cascade left buffer collapsed | Kill+relaunch between reps (see plan: ~/.claude/plans/abort-characterization-test.md) |
| `abort_kind` consistently different from armed `fault_shape` | Proxy bug — translator wrote the wrong v1 surface field | Inspect `segment_failure_type` in session state right after arm |

## Limitations / known gaps

- **One-shot only.** `frequency=0` means each cycle injects one fault and waits — there's no characterization of "fault repeating every N requests" yet. That would be the **retry/backoff** test (planned, not implemented).
- **Top variant only.** All cycles begin with the player on the top variant (e.g. `3840x2160`). A per-variant matrix would tell us whether the player's response to a mid-segment abort depends on what variant it's currently rendering.
- **Audio segments excluded.** The test scopes faults to video variant directories only. Audio segment failures would surface different behaviors (audio-only playback continues silently? gap-out?). Not yet covered.
- **Bandwidth-estimate persistence across cycles.** The player's bandwidth estimate from a previous cycle's recovery may bias the next cycle's response. We don't reset between cycles.
- **Cumulative load.** After 5–6 cycles the player can enter a state from which it can't recover within the cycle's recovery window. The test framework's pass criterion catches this but the data quality from those later cycles is poor.

## When you're about to change this code

1. **New fault shapes** require entries here AND in `fault-injection-wire-contract.md`.
2. **Cycle structure changes** (e.g. moving to multi-shot rules) require updating the "What the test is doing" table.
3. **New AbortCycleResult fields** need an entry in the data-model table + a paragraph in "How to interpret a row."
4. **Re-run with `CHAR_ABORT_REPS=3` after any change** and compare the per-shape results against a previously-archived run. The shape-to-behavior mapping is contract; deviations are bugs (or new discoveries).

## Related code

- `tests/characterization/modes/abort_test.go` — the driver.
- `tests/characterization/runner/report.go § AbortCycleResult` — the struct.
- `tests/characterization/runner/cycle.go § ObserveAbortCycle` — builds the result from samples + network rows.
- `tests/characterization/runner/shape.go § ArmFault / ClearFaults / SetSegmentTimeout` — proxy mutations.
- `tests/characterization/runner/variants.go § VideoVariantDirs` — derives the URL scope to exclude audio.

## See also

- `.claude/standards/fault-injection-wire-contract.md` — wire-level behaviour per fault shape (the contract this test consumes).
- `.claude/standards/startup-characterization-test.md` — companion test for the cold-start path.
- `.claude/standards/avplayer-quirks.md` — known AVPlayer behaviors that affect failure recovery.
- `~/.claude/plans/abort-characterization-test.md` — the plan that introduced this test + the per-variant follow-on.
