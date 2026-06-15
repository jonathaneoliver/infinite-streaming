# Live-offset is currently untestable on Android TV — 2026-06-15

## Summary
A config-class sweep that sets `content_manipulation.live_offset` does **not** move the achieved
offset on Android TV (ExoPlayer): the recipe asked for 6 s, the player ran at ~21.5 s the whole play.
Two reasons stack: (1) the sub-spec hold-back is rejected/clamped to the floor, and (2) Android has
**no client-side offset lever** at all. So a sub-spec (or any not-honoured) `live_offset` is untestable
on Android TV until either the manifest path lands a *legal* value or an Android `LiveConfiguration`
target-offset lever is plumbed. Tag: **confirmed**.

## How we learned this
Triaging the androidtv finding `seed-config-androidtv-hls-live-offset-startup` (`live_offset=6`,
`startup`) + its rep batch `rep-8c0d9e-*`. It kept scoring `aberration` (`qoe_cirr_breach`, a
stall-continuity breach), but the achieved offset never matched the intended 6 s — so the stall was
being (wrongly) attributed to a live-offset that never took effect.

## Evidence
Play `b104c934` (rep) and `827b7269` (parent), androidtv / Google TV Streamer / ExoPlayer 1.2.1:
- `configured_offset_s` and `recommended_offset_s` (the parsed manifest hold-back) ≈ **21.464 s**;
  `true_offset_s`/`live_offset_s` tracked ~21.5 s. Intended: **6 s**. The IV never moved.
- The hold-back floor is `3 × max segment duration`. "6 s" segments round up to ~7 s, so the floor is
  ~21 s (not 18 s) — which is exactly where the player sat. `HOLD-BACK=6` is sub-spec and was
  rejected/clamped.
- Android has no client lever: `LiveConfiguration.targetOffsetMs/min/max` are hardcoded `C.TIME_UNSET`
  (`android/InfiniteStreamPlayer/app/src/main/java/com/infinitestream/player/state/PlayerViewModel.kt:783`),
  so ExoPlayer derives the offset from the manifest alone; `MainActivity.kt` reads only `is.player_id`
  as an intent extra — there is no `is.flag.live_offset_s` equivalent.
- iOS DOES have the client lever: `is.flag.live_offset_s` → `AVPlayerItem.configuredTimeOffsetFromLive`
  + a seek (`PlayerViewModel.swift:1868`), so iOS can be driven independent of the manifest.

## Hypothesis
**confirmed.** On Android TV the manifest is the only live-offset lever, and a sub-spec value won't
land; with no client lever, any offset the manifest doesn't honour is untestable there. The
`qoe_cirr_breach` stall on those plays is **not attributable to live_offset** (the IV didn't vary).

## Where this matters
- The #793 manipulation-check gate now marks these runs **`inconclusive` → `review`** automatically
  (achieved offset ≠ intended within a segment-aware tolerance), so they never become false findings.
- The segment × live-offset matrix runs on **ipad-sim**, where the manifest lever works for legal
  values (and iOS additionally has the client lever). Android-TV live-offset arms stay inconclusive
  until the Android client lever (#793 step 4) is plumbed.

## Action items
- [ ] (Optional, #793 step 4) Wire `is.flag.live_offset_s` → `LiveConfiguration.setTargetOffsetMs` on
      Android so it can be driven like iOS.
- [x] Manipulation-check gate gates findings on IV-moved (shipped, #793).

## See also
- `docs/sweep-design.md` §3.1 (manipulation check / validity gate).
- `.claude/findings/avplayer-startup-variant-selection-2026-06-07.md` (adjacent startup-behaviour finding).
