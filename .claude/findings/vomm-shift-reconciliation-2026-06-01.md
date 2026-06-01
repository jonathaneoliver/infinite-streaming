# bitrate_shifts vs request-visible rendition changes — #508 VOMM gate — 2026-06-01

## Summary
For the #508 sequence-anomaly model, the #508 Phase-0 spike flagged AMBER: play
`0a9c4308` reported 19 `bitrate_shifts` but every segment was 2160p, raising the fear
that the request-ordering model would be *blind* to ABR behaviour. Reconciling across
1015 archived plays **refutes that fear for real sessions**: the structural ΔP signal
is real and abundant, and the apparent divergence was a stub-play artifact. The actual
risk is the opposite — the request stream carries **more** rendition events than the
player counts as shifts (startup ramps, backward-jump re-fetches, loop boundaries), and
a naive ΔP tokenizer would mis-read them as up/down shift pairs. These surplus events are
**not noise to suppress** — at least one class (the backward-jump-and-replay re-fetch) is
a real, possibly-pathological behaviour. Proceed with the model, but tokenize these
events *faithfully and distinctly* and let the surprise model decide; do not launder them
into "normal".

## Evidence
Three signals compared: `bitrate_shifts` (player ABR-decision counter),
`resolution_changes` (player displayed-resolution counter), and **URL-visible rendition
changes** parsed from `/{content}/<rendition>/segment_*.m4s` — the only signal the model
sees.

1. **`bitrate_shifts == upshifts + downshifts` for 100% of 1015 plays** — it is exactly
   the up/down decision counter.
2. **Binary divergence (`bs>0 & rc==0`) is a stub artifact.** 28% (blank-engine) / 13%
   (AVPlayer) across all plays → **0% on every engine** once `playing_time_ms ≥ 30s`
   (n=65). The 918 blank-engine rows are sub-30s stubs (~9–13 net events, 0 playing
   time).
3. **Structural signal is rich** — shifting plays show up to 43 URL rendition
   transitions, 5–6 distinct renditions.
4. **Surplus events, not blindness:**
   - Startup ramp `360p#21 → 2160p#21` then steady — a real URL ΔP transition that *no*
     player counter scores as a shift (`97c5507b`: bs=0/rc=0, 1 URL change).
   - Backward-jump-and-replay re-fetch — ExoPlayer `32afb15f`: after fetching 2160p
     through seg46 the player re-fetches seg43 (360p landing grab + full 2160p), then
     re-downloads 43→44→45→46→47 at 2160p. Displayed rendition never leaves 2160p (player
     bs=1), but naive ΔP reads each excursion as a down+up shift pair → 7 phantom
     transitions. Co-located with this play's `timejump×3 / buffering_long_scrub×3 /
     4 stalls` — consistent with a backward seek/scrub.
   - Loop boundary `seg54 → seg00001` (harness loops content).
   - On AVPlayer busy sessions the changes are genuinely *sustained* (real ABR sweeps):
     `URL-transitions ≈ bitrate_shifts` (29≈30, 43=43, 4≈5); `resolution_changes` runs
     slightly lower (coalesces to displayed switches).
5. **The re-fetches are COMPLETE, not aborted/partial.** All three 360p grabs return
   `status 200` with full 360p-sized bodies (360–594 KB, same class as startup 360p),
   and each is followed ~0.3–0.4s later by a *complete* 2160p fetch of the same segnum
   (15.7 MB). So the player fully downloads the segment twice; the real waste is the
   ~3 full 2160p segments (~50 MB) re-downloaded per backward jump (~150 MB across the
   three jumps), not the 0.5 MB 360p.
6. **Not a timestamp-ordering artifact** (verified): the re-fetch is provable from
   segment identity + byte counts *alone* — segs 43–46 each appear twice as complete
   2160p deliveries with identical byte counts, independent of any sort. Additionally:
   zero duplicate timestamps (ms resolution, single server clock); the 360p→2160p gap
   (~2.4 s) dwarfs the slowest 2160p transfer (~1.7 s); and completion-vs-start stamping
   could only *mask* a backward jump (fast 360p sorts earlier), never manufacture one.
   The per-row `total_ms`/`transfer_ms` fields are internally inconsistent (mixed units)
   and were deliberately NOT relied on.

Reproduce: `harness --insecure --json query plays --from 2026-01-01T00:00:00Z --to
2026-06-02T00:00:00Z --limit 5000`, then per-play `harness --insecure --json query
network <uuid> --limit 3000` and parse the rendition dir + segment number in `ts` order.
Completion check: inspect `status` + `bytes_out`. Fault classification is available via
`query network --json` — faulted rows carry `fault_type`/`fault_category`/`fault_action`
(`omitempty`-dropped on clean rows); the live human view (`tail`/`network`) now renders
them too. `response_content_range` and the raw `faulted` flag are still not projected by
the read API (a small forwarder add if PARTIAL-delivery detection needs them).

## Hypothesis
The request-ordering premise for #508 is sound (**confirmed** on n=65 substantial plays,
2 engines). The tokenization hazard is that a naive ΔP-per-URL-change tokenizer
mis-reads non-shift rendition events as up/down shifts. The fix is NOT to suppress them:
distinguish a *sustained* rendition switch from a *single-segment excursion* so each is a
**distinct token** (e.g. `V_SEG` vs `V_PROBE`/re-fetch + `STARTUP_RAMP` + `LOOP_BOUNDARY`),
then let the tail/peak-surprise model score them — one probe is nothing, a *cluster* of
backward-jump re-fetches around stalls is exactly the structural anomaly #508 should
catch. Whether such a cluster is benign (user scrub) or pathological (involuntary
re-fetch / unstable recovery) **cannot be decided from ordering alone** — it needs
playhead direction, seek-control events, and buffer level (timing/state → #445 HMM or an
enriched alphabet). The original `0a9c4308` (19 shifts / all-2160p) is best explained as
this re-fetch pattern at scale, not blindness — but that play is outside the queried
window, so the reinterpretation is **needs-test**.

## Caveats
- Deep URL-parse was a 9-play sample; the 0%-divergence aggregate is n=65.
- Substantial corpus is **2 engines only** (AVPlayer 58, ExoPlayer 7) — no Roku, no
  hls.js. Per-engine model R&D is corpus-blocked (matches #508's P3 corpus gate).

## Action items
- [ ] Harness-generate Roku + hls.js + more organic ExoPlayer sessions (corpus is the
      binding constraint).
- [ ] Tokenize sustained-switch vs re-fetch-excursion as *distinct* tokens (not
      suppression) + `LOOP_BOUNDARY` + `STARTUP_RAMP`; let the surprise model score
      clusters. Done in `analytics/tools/tokenize.py` (prototype).
- [ ] needs-test: classify backward-jump re-fetch benign (user scrub) vs pathological —
      requires playhead/seek-control/buffer state (#445 HMM or enriched alphabet).
- [ ] #507: map `fault_type`/`fault_category` → `ABORT/PARTIAL(surface)` token (capture
      exists; token does not). Fault columns ARE accessible via `query network --json`
      (faulted rows) and `raw GET /api/v2/network_requests?faulted_only=true`; the CLI
      human view now renders them. No CH-direct access or schema change needed.
- [ ] needs-test: re-pull `0a9c4308` if it can be located and confirm the re-fetch
      reinterpretation.

## See also
- `a45a161d-progressive-stall-wedge-2026-05-20.md` — separate stall finding; notes
  `bitrate_shifts=43` with the iOS up/down asymmetry, useful context for the AVPlayer
  busy-session shape seen here.
- GitHub #508 (reconciliation comment), #507 (abort token), #442 (revised modeling note).
