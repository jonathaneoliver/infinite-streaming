# ABR ladder: peak vs average, and the limit ladder we shape with

Which bitrate scalar a player keys on per phase, and how the test rig's
limit ladder is built so a sweep characterizes that behaviour. Issue #551.

## Which scalar each player uses (read from source)

`BANDWIDTH` (peak per HLS spec) and `AVERAGE-BANDWIDTH` are two scalars
per variant. Rendition selection keys almost entirely on **peak**:

- **hls.js** (v1.6.16) — peak for startup / down-switch / abandon;
  average only in one narrow steady-state case (buffer ≥ 2 segments AND a
  zero-rebuffer-tolerance pass). `src/controller/abr-controller.ts:944`.
- **ExoPlayer / Media3** (1.10.1) — peak only. `Format.bitrate` resolves
  to `peakBitrate`; `averageBitrate` is parsed but never reaches the
  selector. `AdaptiveTrackSelection.java:563`, `Format.java:1205`.
- **Shaka** (`main`) — peak only. `variant.bandwidth` is `BANDWIDTH ||
  AVERAGE-BANDWIDTH`; the `||` fallback never fires when BANDWIDTH is
  present. `simple_abr_manager.js:309`.
- **AVPlayer** (our ship target) — **unknown**, closed-source. 3-of-3 OSS
  players key on peak, so peak-correctness is the safe assumption, but the
  only way to *know* is to instrument it with a discriminating ladder
  (the #551 stretch / follow-up).

Mental model for tests: **startup pick = peak. Switch-down = peak.
Switch-up-with-full-buffer = *may* be average, but only hls.js.** Down-
switching on average is something no audited player does.

## The limit ladder (go-proxy/pkg/ladder)

One shared builder feeds the characterization harness, the harness CLI
(`shape --pattern`), and the dashboard's pattern panel (JS mirror):

- **Both rungs per variant.** Each variant contributes a peak anchor and
  an average anchor (avg skipped when AVERAGE-BANDWIDTH is absent). We
  carry both because AVPlayer is unknown — a sweep then probes both
  regimes regardless of which scalar the player uses.
- **Flat +5% bump** (`--margin`, default 5) on each anchor — covers
  TCP/IP + TLS + HTTP framing. Replaced the old `margin × 1.07-TCP`
  two-factor. `0%` is a deliberate-stall footgun.
- **Geometric fills to ≤1.15×** (`--max-step` / `CHAR_LADDER_MAX_STEP`)
  between anchors, so a downward sweep can't skip the rung where a player
  switches. For the 6-variant tears-of-steel h264 ladder this is 12
  anchors + 22 fills = 34 rungs.
- **Discriminating band.** A variant's avg→peak gap is where peak- and
  avg-keyed players diverge: a cap parked inside it (e.g. 6.3 Mbps,
  between 1080p avg 5.4 and peak 7.2) makes a peak-keyed player drop to
  720p while an avg-keyed player holds 1080p. This is what makes the
  AVPlayer probe (throttle-into-the-gap) decisive.

## Manifest ladder hazards (ValidateLadder)

Real hazards to lint a published ladder for — overlap is NOT one:

- **Inversion** — the average column disagrees with the peak ordering
  (peak rises while avg falls across rungs).
- **Duplicate BANDWIDTH** — two variants share a peak value.
- **Tight spacing** — adjacent peak ratio < 1.5× (players can't tell the
  rungs apart and oscillate).
- **NOT a hazard: avg→peak band overlap across rungs.** Normal capped-VBR;
  RFC 8216 imposes no non-overlap rule; players reason over one scalar per
  variant, not the interval. Never flag it.

## Common mistakes

- Anchoring startup / down-switch test expectations to AVERAGE-BANDWIDTH.
  All three OSS players use peak; expectations must too.
- Treating an avg-based up-switch assertion as cross-player — it's
  hls.js-specific.
- Flagging avg→peak overlap as a ladder defect. It isn't.
- Assuming AVPlayer keys on peak. It probably does, but it's unverified —
  say "unknown" until the probe ladder settles it.

## See also

- `.claude/standards/abr-decision-model.md` — ABR decision mechanics, the `content` aggression knobs
- `.claude/standards/avplayer-quirks.md` — Apple-specific ABR behaviour
- `.claude/standards/characterization-principles.md` — how the sweeps are run
- `go-proxy/pkg/ladder/` — the shared builder + `ladder_test.go` golden vectors
