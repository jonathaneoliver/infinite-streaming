# Ladder density changes over-downshift HUNTING, not bitrate overage — 2026-06-12

## Summary
On a **denser** variant ladder AVPlayer does **not** descend more gently — it
produces **more over-downshift→correct cycles** (more hunting), while the
**per-overshoot bitrate overage stays about the same**. The estimator jumps to
the bitrate-appropriate variant and overshoots either way; extra rungs just give
it more rungs to oscillate across. Tag: **needs-test** (n=1).

## How we learned this
First real result from the per-member A/B foundation (#768). A grouped A/B on
`insane_new_p200_h264` under **identical** 81-step broadcast caps (`CHAR_STEP_S=30`,
group `G1781288827`):

- **Leader** = full **11-rung** ladder (play `1730b477`)
- **Observer** = **6-rung** every-other ladder (play `c90ddf9e`, `allowed_variants` thinned)

Same content, same caps, same sim model/OS — only ladder density differs.

## Evidence
Fetched-variant paths (resolution, ascent clean on both):

```
DENSE  (11): … 2160 → 1080 → 1296 → 900 → 432 → 540
                    └─over─┘ └corr┘      └over─┘ └corr┘   2160→1080 skips 1800/1440/1296
SPARSE  (6): … 2160 → 1440 → 720 → 1080 → 540
                  (clean) └over┘ └corr┘                   2160→1440 clean 1-rung
```

| | Dense (11) | Sparse (6) |
|---|---|---|
| over-downshift→correct cycles | **2** | **1** |
| shift activity | up=12 / resCh=15 | up=6 / resCh=9 |
| top-of-descent | 2160→1080 (big over-downshift) | 2160→1440 (clean 1-rung) |
| biggest bitrate overage (dropped-below-recovery) | ~3.35 Mbps (1080=7.2M → recovered 1296=10.5M) | ~3.63 Mbps (720=3.6M → recovered 1080=7.2M) |

insane_new peak bitrates used: 360p 1.06 / 432p 1.42 / 540p 1.89 / 648p 2.57 /
720p 3.55 / 900p 5.03 / 1080p 7.18 / 1296p 10.53 / 1440p 15.48 / 1800p 21.53 /
2160p 30.33 Mbps.

## Hypothesis (needs-test)
Ladder density does **not** materially change the *bitrate magnitude* of an iOS
over-downshift — the player drops to roughly the same bitrate floor and corrects
back up by a similar amount regardless of rung count. What density changes is the
**hunting**: more rungs → more over-downshift/correct cycles (and the big drops
land *at the top* of the descent on a dense ladder, skipping several fine rungs;
a sparse ladder steps cleanly at the top and overshoots lower). n=1, single rep,
and the leader drove while the observer followed (identical caps, so fair, but a
2–3 rep repeat is needed before this is `confirmed`).

## Action items
- [ ] Re-run 2–3 reps (swap which device is leader) to confirm "dense = more
      hunting, same overage" and rule out a leader-vs-follower artifact.
- [ ] If confirmed, this is a characterization insight worth a line in an ABR /
      characterization standards doc: ladder density is a *hunting* lever, not an
      *overage* lever.

## See also
- `47d16786-over-downshift-2026-06-12.md` — the base over-downshift behaviour
  (drops ≥2 rungs on a descending cap with a full buffer, then corrects up). This
  finding extends it with the density A/B.
- `avplayer-startup-variant-selection-2026-06-07.md` — same in-memory throughput
  estimator on the startup side.
