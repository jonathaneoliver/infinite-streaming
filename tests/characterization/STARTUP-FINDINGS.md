# AVPlayer low-bitrate startup — findings

How to make AVPlayer (iOS/tvOS) start up well when the network is throttled to
~1–1.5 Mbps. This is the consolidated learning from the #811 characterization
work; the runnable test model is in `TEST-PLAN.md`, the analysis tools are
`analytics/tools/startup_view.py` and `startup_sweep.py`.

## Overall conclusion

**The on-device LocalProxy breaks AVPlayer's initial bitrate estimate** — it
serves AVPlayer over localhost, so the player measures the localhost hop (fast),
not the shaped network, and its startup `observedBitrate` reads **above the rate
cap** for a while before settling. Disabling the LocalProxy gives an accurate
estimate from the start.

**It is still worth keeping the in-app "startup bitrate cap."** The cap is a soft
hint (see below) and doesn't bound a runaway on its own, but combined with
proxy-off it materially improves cold-start variant selection at low bitrate.

Caveat (keep this honest): the over-selection wedge appeared **once with the
LocalProxy already off** (`4d9889d8` → 1440p), so proxy-off + cap is *much
better*, not a guaranteed cure — the single-packet init sample (below) is a
residual, proxy-independent trigger.

## The failure mode: cold-start over-selection "wedge"

On a fresh play_id, AVPlayer's startup bandwidth estimate over-reads → it climbs
the variant ladder fetching **init segments only** (v360→v432→…→2160 init) → lands
on 4K → can't deliver a 4K segment under the cap → never reaches stable playback
(TTFF 60–85s, `played=0`, 10+ shifts). Intermittent: ~1–2 of 3–5 reps in the bad
configs, but it shows up across every config we tried.

## Why the obvious fixes DON'T work (the expensive lessons)

- **`preferredPeakBitRate` is a soft hint.** With it held, AVPlayer still climbed
  to 2160p during the over-read. It does not bound the wedge.
- **`startsOnFirstEligibleVariant` is also soft.** It pins the *join* rung and cut
  the wedge to ~1/10, but AVPlayer still overrode it (`16b822a3`: fetched v360
  init then climbed init-by-init without ever fetching v360 data).
- **iOS measures throughput first-byte-to-last-byte** (`AVMetric`
  responseStart→responseEnd; `observedBitrate`/`transferDuration` = "active
  transfer"), NOT request-to-last-byte. So *delaying* a response (adding TTFB)
  does nothing to the estimate.
- **The ~1 KB init segment is the garbage sample.** A single packet → transfer
  time ≈ 0 → astronomically high apparent bitrate (6–17 Mbps observed). It also
  sits *below* the HTB rate-limiter's 1-MTU burst floor (`htbClassParams`,
  go-proxy/cmd/server/main.go), so it's sent at line rate. **TC cannot pace a
  single packet** — and since iOS excludes TTFB, you can't fix it by delaying;
  you'd have to pace the *bytes*.

## What actually moves the needle

- **s6 (6s) segments** — the ~6s live-edge abandon budget lets deliverable
  segments finish before the climb spirals. Durable mitigation.
- **Ladder-trim** — remove the high rungs from the manifest so 4K/1440 simply
  aren't selectable. The only *hard* cap (everything else is a soft hint).
- **start-on-first** — reduces the wedge rate (~1/10), doesn't eliminate.
- **Startup forward-buffer cap** (`is.flag.startup_forward_buffer_s`) — smaller
  forward buffer → first frame sooner.
- **bufsize-1× encode** (`BUFSIZE_MULT=1` in `create_abr_ladder.sh`) — tightens
  per-segment peaks so actual ≈ advertised, but does NOT fix the cold-start
  over-read.

## LocalProxy vs SFQ — two separate effects

- **LocalProxy** inflates the startup `observedBitrate` (localhost TTFB sub-ms vs
  ~100 ms real RTT). Real, worth disabling for measurement accuracy.
- **SFQ on** (`PROXY_DISABLE_SFQ=0`) incidentally *suppresses* the wedge by
  splitting the link ~50/50 audio/video → video only ever measures ~0.72 Mbps →
  no over-read. But it **starves video / audio over-banks** (equal share audio
  doesn't need): slow start (TTFF ~7.7s vs ~3s), quality capped at 360p.
  SFQ off → full rate, faster, 432p, but the wedge can return. If we keep a fair
  leaf it should be **weighted** (video ≫ audio), not equal SFQ.

## Cold-start vs recovery (why "reload" wedges but "restart" doesn't)

`applyStartupCaps` (PlayerViewModel.swift) branches on `isRecovery`:
- **Reload / new play_id** → `isRecovery=false` → cold-start cap, released after
  first frame, ABR re-rolls from a fresh estimate → variable max → this is where
  the wedge happens.
- **Restart / recovery** → `isRecovery=true` → cap pinned to the **pre-retry
  throughput** (`recoveryVariantCapBps`, floored; #819) and
  `startsOnFirstEligibleVariant` forced off → resumes at the prior rate → stable
  max, never re-cold-starts.

So the wedge only reproduces via **reload (new play_id)**.

## Methodology scars

- **Confounds bit us twice:** a go-server restart appeared to "fix" the wedge
  (0/3) then it returned; SFQ on/off swung the numbers. Always control server
  state + SFQ before attributing.
- **n=3 is noise.** The wedge is intermittent — need ~8–10 reps/config for a real
  rate. Don't conclude from 1–3.

## Open / needs-test

- **Proxy byte-pacing of small/init responses** — trickle the init bytes so its
  first-byte-to-last-byte reads a realistic rate. The candidate real fix for the
  residual init-garbage trigger; untested.
- Whether AVPlayer folds the init (map) segment into its estimate, or the early
  playlists/connection are the source.

## Tooling

- `analytics/tools/startup_view.py --vseg N <play>` — per-segment startup timeline
  with TTFF + videoWillKeepUp markers, segment-complete status, audio/video
  interleave; `--chunks --log` for on-device `[NETCHUNK]` partial view.
- `analytics/tools/startup_sweep.py` — runs `harness char matrix`, reports
  TTFF/keepup/shifts/stalls/quality across reps with wedge rejection.
- Matrices in `matrix/` (pyramid-1-s2 / -s6 / -firstvar / -firstvar-cap, etc.).
