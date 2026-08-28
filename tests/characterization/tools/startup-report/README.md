# Startup characterization report

Regenerates the **video-startup** report — `is.peak_bitrate_mbps` (initial rate
cap) × `proxy.shape.rate_mbps` (network limit) × `is.segment` (segment length),
each cell run N times for run-to-run variation. Backs the finding
[`.claude/findings/startup-initial-cap-vs-network-limit-2026-07-12.md`](../../../../.claude/findings/startup-initial-cap-vs-network-limit-2026-07-12.md).

The pipeline is **three parts**; parts 2+3 are the single script here.

| Part | What | Where |
|---|---|---|
| 1 · collect | drive the grid on device(s), write `rep{N}_seg_{seg}.tsv` + `play_player_map.tsv` | sweep driver (per-device; not in this dir) |
| 2 · metrics | pull each play from the archive (`harness query events`), derive startup metrics | `startup_report.py` |
| 3 · aggregate + render | fold the N reps per cell, inject into the template → HTML | `startup_report.py` + `report.template.html` |

## Run (parts 2 + 3)

```bash
export PATH="$HOME/.local/bin:$PATH"          # the `harness` CLI
python3 startup_report.py --maps <dir> --reps 3
# → startup-report.html next to the script
```

`--maps <dir>` holds the part-1 output:

```
rep1_seg_s6.tsv  rep2_seg_s6.tsv  rep3_seg_s6.tsv   (limit<TAB>cap<TAB>play_id, one file per rep×segment)
rep1_seg_s2.tsv  …                                   segments: s6 / s2 / s1
rep1_seg_s1.tsv  …
play_player_map.tsv                                  (play_id<TAB>player_id — for session-viewer links)
```

n=1 fallback: with no `rep*_seg_*` files it reads `startup_seg_{seg}.tsv` as the single rep.

Queried events are cached to `<maps>/.metrics_cache.json` keyed by play_id, so
re-rendering after a template tweak is instant. `--no-cache` re-pulls everything.

### Flags

| Flag | Default | Notes |
|---|---|---|
| `--maps` | `$CLAUDE_JOB_DIR/tmp` else `.` | part-1 output dir |
| `--reps` | `3` | reps per cell |
| `--segs` | `s6,s2,s1` | segment lengths |
| `--limits` | `0,16,8,2` | network limits (Mbps; `0`=uncapped) |
| `--caps` | `0,1,2,8,16` | initial rate caps (Mbps; `0`=off) |
| `--host` | `https://dev.jeoliver.com:21000` | archive + session-viewer base |
| `--out` | `./startup-report.html` | rendered report |
| `--template` | `./report.template.html` | HTML shell (`__DATA__` placeholder) |

## Reading the report

Per cell (mean over the reps, min–max beneath): **video-start** (playhead moving),
**TTFF** (frame decoded), **Fetched/Shown/Var@60s** rungs (modal rung; ⚠ + rung
range when the reps disagree), **Settle**, **Startup-eff** (bitrate-utilization %),
**Stalls**, **Shifts↑**, a **±CoV** stability badge, and **Residency** (F=fetched,
S=shown) bars. `¹²³` open each rep in session-viewer; `⇄` overlays all reps.

Startup-eff is an engineering proxy, **not** a perceptual QoE score (ITU-T
P.1203/P.1204 and VMAF are the rigorous alternatives).
