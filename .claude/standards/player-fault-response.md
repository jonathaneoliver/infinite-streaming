# Player response to injected faults: ExoPlayer vs AVPlayer

When you arm a fault on a variant and the player "keeps playing with no
error," the fault engine is usually working fine — the *player* is
absorbing it. iOS AVPlayer and Android ExoPlayer react to a faulted
segment **very differently**, and that asymmetry burns hours if you
assume the matching code is broken. It usually isn't (verify with the
live probe in "Proving the match fires" below before blaming it).

## The asymmetry (the thing that bit us — #919 era, 2026-07-09)

| | iOS AVPlayer | Android ExoPlayer |
|---|---|---|
| Single transient segment error (one 503) | Tends to **downshift** away from the rung | **Retries** the segment (default `LoadErrorHandlingPolicy`, ~3 tries) and, if the retry lands in a clean cadence slot, **recovers and keeps playing** — no surfaced error, no rate shift |
| Sustained failure on a rung | Downshifts | Downshifts / errors only once retries are exhausted |

So the *same* intermittent fault makes iOS visibly rate-shift while
Android looks unaffected. The 503s are still injected — they appear in
the **network log** either way (the proxy returns them). "Playback
continued" ≠ "no fault fired." Read the network log, not the player.

## Consequence for fault testing

To force an ExoPlayer/Android downshift you need failure the retries
**can't escape**:

- **Continuous** fault (see cadence below), or
- `mode: requests` with **`consecutive` above ExoPlayer's retry budget**
  (try `consecutive: 5`), so the segment + all its retries fail.

A single intermittent 503 will never move ExoPlayer.

The deterministic, platform-agnostic alternative is **Content
Manipulation**: deselect the top variant from `allowed_variants` so it's
removed from the manifest the player receives. A player can't request a
rung that isn't advertised → guaranteed downshift on iOS *and* Android,
no reliance on error handling.

## Fault-cadence semantics (go-proxy native evaluator, #919/#925)

The UI's frequency/consecutive sliders map to the native cadence engine
(`fault_evaluate.go` → `FailureHandler` in `main.go`). Two edges that
look like bugs but are (now) deliberate:

- **`consecutive=0 AND frequency=0` → CONTINUOUS**: fault every matching
  request until the rule is cleared. This is the UI default. Before the
  2026-07-09 fix it clamped to `consec=1/freq=0` = a **one-shot single
  failure** (fired once, then `resetFailureType` reverted it forever) —
  which is why default-armed faults seemed to "do nothing," especially
  on ExoPlayer. Guarded by `TestNativeContinuous`.
- **`consecutive>0, frequency=0` → one-shot burst of N** then revert
  (unchanged; `TestNativeOneShot`).
- **`frequency>0` → repeating cycle** (unchanged).

## Variant scope matching (what "select a variant" actually keys on)

The scope checkboxes write **`filter.variant.resolutions`** — a list of
**resolution strings** (e.g. `"3840x2160"`), NOT bitrate, NOT a URL id.
The classifier assigns each segment a resolution from the rendition map
(`_rendition_map`, dir→rendition), and both the scope list and the
classifier draw that string from the **same manifest parse** — HLS master
`RESOLUTION=` / DASH `<Representation>` width×height — so they can't
silently mismatch. Comparison is case-insensitive exact
(`containsStringFold`). See `fault_match.go:variantPredicateMatches`.

Empty vs absent matters:

- **`resolutions` absent** → no constraint → matches all video.
- **`resolutions: []` present-but-empty** → operator scoped to zero
  variants ("all unchecked") → **matches NOTHING**. Before the
  2026-07-09 fix the backend skipped the empty list and matched
  everything — the inverse of the UI's intent. Guarded by
  `TestMatchFaultRule_EmptyResolutionsMatchesNothing`.

A variant predicate only matches **video** requests (`rc.RungIndex >= 0`);
audio/init/master (`RungIndex < 0`) never match it — that's what keeps a
video-scoped rule off audio/init (#917). Audio is scoped via
`request_kind: [audio_segment]`, not the variant filter.

## Proving the match fires (no device needed)

Register a session, populate the rendition map, arm the exact UI rule,
and hit a real segment through the per-session proxy port:

```bash
H=jonathanoliver-ubuntu.local; C=insane_newer_p200_h264; PID=$(uuidgen)
# register + populate _rendition_map, capture per-session port from the redirect
FINAL=$(curl -skL "https://$H:21081/go-live/$C/manifest_6s.mpd?player_id=$PID" -o /tmp/m.mpd -w '%{url_effective}')
PORT=$(echo "$FINAL" | sed -E 's#.*:([0-9]+)/.*#\1#')
REV=$(curl -sk "https://$H:21000/api/v2/players/$PID" | python3 -c 'import sys,json;print(json.load(sys.stdin)["control_revision"])')
# arm: continuous 4K segment fault (freq=0,consec=0)
curl -sk -X PATCH "https://$H:21000/api/v2/players/$PID" -H 'Content-Type: application/merge-patch+json' -H "If-Match: $REV" \
  -d '{"fault_rules":[{"id":"t","type":"503","frequency":0,"consecutive":0,"mode":"failures_per_seconds","filter":{"request_kind":["segment"],"variant":{"resolutions":["3840x2160"]}}}]}'
# request a 2160p segment (dir must match the .mpd SegmentList dir) → expect 503
curl -sk -o /dev/null -w '%{http_code}\n' "https://$H:$PORT/go-live/$C/2160p/segment_00045.m4s?player_id=$PID"
```

Live matrix that should hold (2160p segments): `resolutions:[]`→no 503;
`["3840x2160"]`→all 503; `["1280x720"]`→no 503 on 2160p; no variant
filter→all 503.

Related: [`fault-injection-wire-contract.md`](fault-injection-wire-contract.md)
(socket-phase wire shapes), [`avplayer-quirks.md`](avplayer-quirks.md),
[`abr-decision-model.md`](abr-decision-model.md).
