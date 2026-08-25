# AVPlayer startup variant selection + bandwidth-estimate persistence — 2026-06-07

## Summary
On modern iOS (13+), AVPlayer does **not** start on the first-listed
master-playlist variant — it runs a startup heuristic that "optimizes
the startup experience" and can pick a HIGH variant from its (live,
in-memory) throughput estimate. The estimate is built from the current
playback's own segment downloads; Apple documents **no persistence**
of it across app launches or device reboot, and leaves the in-process
cross-`AVPlayerItem` case unspecified. There is **no `DEFAULT` tag for
video variants** — `DEFAULT=YES` is audio/subtitle only. Practical
upshot for the characterization rig: the only reliable way to make
startup land on a sustainable variant is to apply the rate cap BEFORE
the player's first segment fetch (true cold start), or to set the API
knobs (`preferredPeakBitRate` / `startsOnFirstEligibleVariant`).

## How we learned this
Chasing the #segments work: a mid-play **segment-length switch** (6s→2s
via the in-app picker) under a 1.932 Mbps floor cap left the player
pinned at **2160p (29.86 Mbps)**, buffer stuck at 0, never reached
first frame, terminated `abandoned_start` (play `34bb8801`, iPad-sim
`6cf2ddf5`, 2026-06-07 ~22:00 UTC). It heartbeat continuously the whole
time — it did NOT wedge/stop POSTing; it was alive and starving on a
variant it couldn't afford. That ruled out "needs longer" and pointed
at startup variant selection, which the research below explains.

## The facts (researched, sourced)

### Initial variant selection — three eras
- **Pre-iOS 13:** "AVFoundation's base algorithm is to pick the **first
  applicable variant** in the master playlist." (WWDC16 §503)
- **iOS 13+:** picks a variant that **"optimizes the startup
  experience"** — a heuristic, NOT first-listed. Can pick higher.
- **iOS 14+:** `AVPlayerItem.startsOnFirstEligibleVariant = YES`
  restores the deterministic first-variant behaviour.
- Our sim runs **iOS 26** → modern heuristic, not first-variant.

### Bandwidth estimate — what is/isn't stored
- Built **live from the current playback's segment downloads**
  ("once it's begun downloading segments, it can use the statistics
  from those downloads to adjust the choice of variant" — WWDC16 §503).
  Surfaced as `observedBitrate` in the access log.
- **No documented persistence** across app launch or device reboot —
  each fresh process re-estimates from its first downloads.
- **In-process, new `AVPlayerItem`:** Apple does not document whether a
  new item inherits the prior item's estimate. Our segment-switch is
  ONE data point consistent with carry-over, but confounded: a new
  item's first fetch can **burst before the kernel `tc` cap throttles
  it**, reading the link as fast regardless of any shared state. Not
  proven either way — see the experiment issue.

### No DEFAULT tag for video
- `EXT-X-STREAM-INF` (video bitrate-ladder rungs) has **no `DEFAULT`
  attribute** — the HLS spec provides no syntax to mark a default
  startup quality. Only **list order** + the player heuristic + the
  API knobs influence it.
- `DEFAULT=YES` / `AUTOSELECT=YES` exist only on `EXT-X-MEDIA`
  (audio / subtitle / closed-caption renditions). Our master playlists
  carry `DEFAULT=YES` on the AUDIO line only; video lines have none.
- Our `master_6s` / `master_2s` list variants ascending: 360p (998
  kbps) first → 2160p last. On a first-variant player that would start
  at 360p (safe); on iOS 26 the order is advisory at best.

## Where this matters
- **Cold-start modes (rampup / pyramid):** they work BECAUSE the floor
  cap is applied before the first manifest/segment fetch, so the iOS 13+
  heuristic measures a slow link and picks low. The control is
  cap-before-first-fetch, NOT playlist order.
- **Any mid-play manifest rebuild under a cap** (segment switch, future
  content swap): the player re-probes/keeps a high estimate and can
  grab an unaffordable variant → starves → `abandoned_start`. Don't
  treat a rebuild as equivalent to a cold start.
- **Deterministic rig startup:** `preferredPeakBitRate` (ceiling that
  removes high variants from initial selection — Apple's recommended
  control) and/or `startsOnFirstEligibleVariant=YES` would make startup
  reproducible instead of estimate-dependent. Candidate app/test-build
  change.

## Hypothesis
The 2160p-under-floor-cap starvation is the iOS 13+ startup optimizer
acting on a stale/high in-memory throughput estimate after a warm
manifest rebuild, NOT a wedge and NOT a recoverable "wait longer."
Tag: **confirmed** for the cause of selection (Apple-documented
behaviour + our wire evidence); **needs-test** for the specific
in-process cross-item estimate carry-over (the experiment issue).

## Action items
- [ ] Controlled experiment: in one app session, establish a high
      estimate, then start a new item under a low cap WITHOUT relaunch
      (settle so `tc` engages before first fetch) and read the startup
      variant — confirms/refutes in-process carry-over. (Filed as an issue.)
- [ ] Evaluate `startsOnFirstEligibleVariant=YES` (and/or
      `preferredPeakBitRate`) in the app / a test build for deterministic
      startup characterization.
- [ ] #segments cold-start design: pre-set segment then RELAUNCH
      (fresh process = no estimate) + cap-before-fetch, never warm-switch.

## Sources
- WWDC16 §503 Advances in AVFoundation Playback — https://asciiwwdc.com/2016/sessions/503
- Explore HLS variants in AVFoundation, WWDC21 §10143 (startsOnFirstEligibleVariant) — https://developer.apple.com/videos/play/wwdc2021/10143/
- HLS Authoring Specification for Apple devices — https://developer.apple.com/documentation/http-live-streaming/hls-authoring-specification-for-apple-devices
- AVPlayerItem.preferredPeakBitRate — https://developer.apple.com/documentation/avfoundation/avplayeritem/preferredpeakbitrate
- RFC 8216bis (HLS) — https://datatracker.ietf.org/doc/html/draft-pantos-hls-rfc8216bis

## See also
- Standards: propose adding the three-era startup-variant rule + "no DEFAULT for video" to `.claude/standards/abr-decision-model.md`.
- Related incident: the #segments segment-switch starvation (play `34bb8801`).
