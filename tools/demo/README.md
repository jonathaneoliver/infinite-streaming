# Demo video tooling

Drive `testing.html` through a real Valley shaping pattern against a real iPhone,
record the browser and the phone separately, composite them into one video whose
layout changes as the story moves between control and reaction, and narrate it —
without hand-editing a timeline.

**Nothing here is on the serving path.** It drives the dashboard over the DOM and
reads the same v2 API the dashboard reads, exactly as an operator would.

## Why it is in the repo

Everything it drives is repo behaviour — the session picker, the shaping pattern
editor, the bitrate chart's `Player Variant` / `Server Variant` series. When
those change the demo breaks, and there is otherwise nothing in-tree to notice.

## Layout

| file | does |
| --- | --- |
| `record_valley.js` | **app-specific.** Drives testing.html, applies Valley, polls the v2 API, writes `cues.json` (caption text + exact timings + layout marks + segment marks) |
| `render_layout.py` | **new.** Composites the browser webm and the phone mov following the `layout` track; span-render + xfade; remaps cue times onto the composite |
| `narrator_app.py` + `narrator.html` | browser editor: scrub, edit caption text, audition/select voices, drag the fast-forward range, export |
| `narrate_sentences.py` | text → speech, sentence-split, hash-cached |
| `pronounce.py` | written → spoken. Project vocabulary lives in `narrative/smashing.pronounce`, **not** in this file |
| `audition.py` | speaks the vocabulary aloud so you can judge it by ear |
| `make_ass.py` | cues → ASS subtitles positioned in the caption strip |
| `join.py` | concatenates parts with a fade, preserving every audio track |
| `edit_text.py` | narration in and out of a plain text file, and in and out of git |
| `narrative/` | **tracked**: the script itself, and the pronunciation extras |

Everything except `record_valley.js` and `render_layout.py` is vendored
byte-identical from `~/Projects/Encoder/tools/demo`. Keep it that way — the copy
being unmodified is what makes a later extraction into a shared package a clean
move rather than a merge.

## Where things live

Generated artifacts go to **`$DEMO_DIR`**, default `~/Desktop/smashing-demo`,
outside the repo. Only the tools and `narrative/` are tracked.

```bash
export DEMO_DIR=~/Desktop/smashing-demo
```

## Two recordings, not one

The iPhone's screen is **not** an AVFoundation device ffmpeg can open —
`ffmpeg -f avfoundation -list_devices true -i ""` lists only
`Jonathans iPhone Camera`, which is Continuity Camera, the wrong thing. The screen
is reachable only through QuickTime. So there are two independent recordings and
`render_layout.py` joins them.

Two consequences, both deliberate:

- **No caption strip is drawn in the page.** In the Encoder demo captions were
  injected into the DOM, which was right when the page *was* the video. Here the
  browser is a sub-rectangle of the final frame, so a caption burned into it
  would shrink and shift with the layout. Captions are data only; the compositor
  reserves the strip and `make_ass.py` burns into it at export.
- **The cursor and spotlight do stay in the page.** They point at browser
  elements, so they belong in the browser's own frame.

### Sync

Two independently started recordings have no shared clock. `record_valley.js`
starts the QuickTime capture itself, so the residual error is QuickTime's own
start latency rather than an unknown offset. Pin it with `phoneOffset` in
`cues.json` — one number, nudged once per take, stable across every re-export.
It is not frame-exact, and no arrangement of two separate recorders would be.

## The five steps

```bash
cd tools/demo
npm install                     # playwright, once

# 1. RECORD — drives the real pattern against the real phone and stamps every
#    caption's time. Those timings are the whole trick.
DEMO_DIR=~/Desktop/smashing-demo EXPECT_STEPS=47 node record_valley.js

# 2. COMPOSITE — two recordings -> one video, following the layout track.
python3 render_layout.py ~/Desktop/smashing-demo/valley1/cues.json

# 3-5. EDIT / VOICE / FAST-FORWARD / EXPORT — all in the browser editor.
CUES=~/Desktop/smashing-demo/valley1/cues-composite.json python3 narrator_app.py
```

## Preflight — check these, every time

Each maps to a way a take goes wrong.

1. **Is the iPhone actually playing?** A pattern applied to an idle session
   shapes nothing and records a flat chart. The recorder refuses to start and
   prints every session it can see, but check first.
2. **Is a pattern already applied?** The recorder refuses — a take that starts
   mid-descent has no settled "before" to cut back to.
3. **Has QuickTime got the iPhone selected?** QuickTime's device chooser is
   **not** scriptable: `new movie recording` reuses whatever source was last
   picked in the UI. Open QuickTime once, File ▸ New Movie Recording, pick
   *Jonathans iPhone* from the chevron next to the record button, then quit.
   After that the recorder can drive it. If you skip this you get a recording of
   the Mac's webcam.
4. **Which way up is the phone?** The compositor probes the recording and sizes
   the side-by-side split from the phone's own aspect, so either orientation
   works — but `phone-full` on a portrait capture leaves most of the frame black,
   because a portrait phone in a 16:9 frame simply is mostly black.

## The ordering rule inside the drive

**Configure the pattern completely, in silence, before anything narrates it.**

`onMaxStepChange` and `onStepSecondsChange` in `NetworkShapingPattern.vue` both
read `draft.template ?? 'ramp_up'`. Touching fill density or step duration
*before* picking a template silently builds a **ramp_up** step table. Picking
Valley corrects it — but any narration or spotlight in between would be
describing a pattern that is not the one about to run. This is the same class of
bug that made Encoder's first take narrate "adding HEVC doubles the jobs" over a
click that removed HEVC.

`EXPECT_STEPS` is the backstop: the recorder asserts the step count before
applying and aborts rather than record the wrong pattern.

## Sizing the pattern

Computed against the live 12-rung ladder (234p → 2160p):

| fill density | ladder caps | valley steps | @6s | @18s |
| --- | --- | --- | --- | --- |
| None | 24 | 47 | 4.7 min | 14.1 min |
| 1.375× | 29 | 55 | 5.5 min | 16.5 min |
| 1.25× (default) | 31 | 59 | 5.9 min | 17.7 min |
| 1.125× | 56 | 107 | 10.7 min | 32.1 min |

The default here is fill `none` at **6s steps** — a ~4.7 minute take.

Know what the step duration does. The device buffers ~22s, so a 6s step is well
under it: the cap moves roughly four times before any one change reaches the
screen, and the **displayed** variant trails the cap continuously instead of
settling between steps. The **fetched** variant still tracks each step promptly,
so the fetched-vs-displayed split is if anything more visible — it just reads as
a persistent offset rather than a sequence of discrete catch-ups. Raise
`STEP_SECONDS` above the buffer depth for the settled version.

## The narration is measured, not scripted

Every spoken claim resolves to a field read live during the take:

| claim | field |
| --- | --- |
| current cap | `shape.pattern_rate_runtime_mbps` |
| step *i* of *N* | `shape.pattern_step_runtime` |
| **fetched** variant | `player_metrics.fetching_resolution` |
| **displayed** variant | `player_metrics.video_resolution` |
| player's rung | `player_metrics.video_bitrate_mbps` |
| server-observed rung | `server_metrics.rendition_mbps` |
| shifts so far | `player_metrics.profile_shift_count` (baselined at Apply) |
| buffer | `player_metrics.buffer_depth_s` |
| did it stall | `stalling_count` / `buffering_count` / `stall_stuck` |

Four traps that would otherwise produce confident wrong narration:

- **The displayed-variant lag must be measured against the fetch change that
  introduced that rung**, not the most recent fetch change. At 6s steps under a
  22s buffer the cap moves ~4 times before one change lands, so "now minus the
  last fetch change" reports ~6s for a lag that is really ~22s. The recorder
  searches the shift history for the switch *to* that resolution.
- **`video_bitrate_mbps` is the rung's `BANDWIDTH` (peak), not throughput.** It
  is "the player is on the 2160p rung, whose peak is 32.7", never "the player is
  pulling 32.7 Mbps".
- **`avg_network_bitrate_mbps` over-reads** (90 Mbps under a 33 Mbps top rung is
  the known AVPlayer quirk). It stays out of the narration entirely.
- **`profile_shift_count` is cumulative for the play**, so it is baselined at
  Apply before anything counts shifts caused by the pattern.

## The layout track

`cues.json` carries a third array beside `cues` and `ffwd`:

```json
"layout": [
  {"at":  0.0, "preset": "web-full"},
  {"at": 41.2, "preset": "side-by-side"},
  {"at": 168.9, "preset": "phone-full"}
]
```

Presets: `web-full`, `phone-full`, `side-by-side`. The recorder emits marks at
its own segment boundaries; edit the numbers afterwards and re-run
`render_layout.py` — it is data, so the edit is reviewable in a diff.

Each span renders separately and the spans are chained with crossfades. A single
graph with `enable='between(t,a,b)'` gates was the alternative and is worse:
`scale` parameters cannot be animated, so every size change needs its own branch
regardless, and moving one cut would re-render everything.

**Crossfades shorten the output**, so cue times are remapped (`t - 0.4*span`) and
written to `cues-composite.json`. Point the narrator at that file, not the
original — the timings are the whole trick and the un-composited ones no longer
line up.

## Checking the pronunciations

```bash
export DEMO_PRONOUNCE=narrative/smashing.pronounce
python3 pronounce.py --list    # written -> spoken
python3 audition.py --both     # written form first, then spoken — the A/B
```

Project vocabulary belongs in `narrative/smashing.pronounce`, **not** in
`pronounce.py` — editing that file forks it from Encoder's copy. Note the extras
splice in *after* the base table, so by the time they run `\b(\d+)p\b` has
already turned `2160p` into `2160 p`; the rung rules match the spaced form.

## Requirements

`playwright` (`npm install` here); `ffmpeg`/`ffprobe` on PATH; QuickTime with the
iPhone selected once by hand; a local Voicebox-compatible TTS server on
`127.0.0.1:17493` for narration.
