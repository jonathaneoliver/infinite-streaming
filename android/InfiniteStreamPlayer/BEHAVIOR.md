# Android TV — Behaviour Spec (addendum to issue #251 HANDOFF)

The original HANDOFF.md described visuals (typography, motion, layout). This
file captures behavioural decisions that came out of the build — rules that
aren't obvious from the code or visuals alone, framed so a future contributor
knows what's intentional vs. accidental.

## Recovery (Playback HUD)

Two buttons. Escalating cost.

| Button | Action | Use when |
|---|---|---|
| **Retry** | `stop` + `clearMediaItems` + re-prepare the *same* URL on the *same* ExoPlayer. | Player stalled or surfaced an error. Manual trigger of the same call auto-recovery makes internally. |
| **Reload** | Release the ExoPlayer, build a new one (new BandwidthMeter + listeners + track-selection params), bump `playerEpoch` so the PlayerView remounts, then re-issue the *original* go-proxy URL — the one before the per-session 302 redirect — so the proxy hands out a fresh redirect target. | Server restart, or anything Retry can't unwedge. |

There is no Restart button. Every URL-affecting setting (`Protocol`, `Codec`,
`Segment`, `Local Proxy`) auto-rebuilds + re-prepares on toggle, so a manual
"rebuild URL" button has no unique job.

Reload **does not** rotate `player_id` and **does not** re-fetch the
`/api/content` catalogue. Both were tried and rejected — proxy session
continuity matters, and the catalogue is mostly static during a recovery
flow. The dashboard's reload remains the way to pick up new server-side
content.

### Auto-recovery

`onPlayerError` walks the cause chain through `classifyCodecError`:

- **Codec errors** (`MediaCodec.CodecException`, `DecoderInitializationException`,
  `MediaCodecDecoderException`) → up to 3 retries, backoff `150ms × attempt`.
  `onRenderedFirstFrame` resets the counter.
- **Non-codec errors** → if Settings → Auto-Recovery is on, single retry after
  500ms. No looping.

NO_MEMORY (errno -12, MTK pool exhausted) gets the user-facing tag "Decoder
busy" in `statusText`; everything else codec-related is "Codec fault".

## Settings drawer

Slide-from-right, 46 % panel width, 240 ms enter/exit (per HANDOFF).
Behavioural rules below are additions.

- **One Back semantics.** Remote-Back inside a picker pops to the main
  settings list; on the main list it closes the drawer. Implemented as
  nested `BackHandler`s — drawer-level handler in MainActivity, picker-level
  handler in `SettingsPanel` enabled only when `picker != null`.
- **Single Back hint.** Panel footer shows "◀ Press Back to return". The
  picker does *not* show its own "◀ Back" — having both was confusing.
- **Sticky picker focus.** When you return from a picker, focus lands on the
  row that opened it (Stream / Protocol / Segment / Codec / Advanced),
  not always on Server. Tracked via `lastPicker`.
- **Initial focus delay.** Drawer animates in 240 ms; the focus
  `requestFocus` waits 280 ms so the row is laid out before being focused.
- **No Right-arrow shortcut.** Was tried and dropped — accidental opens.
- **HUD opens on D-pad Down**, not Up. Dev HUD stacks below the shortcut
  hint so the hint stays visible when developer mode is on.
- **Body fills the panel.** Both `MainList` and the picker `LazyColumn` use
  `weight(1f)` (fill = true) wrapped in a body Box, so settings rows occupy
  full available height and scroll when overflowing. Earlier `fill = false` +
  trailing `Spacer(weight 1f)` halved the usable area.

### Settings → Advanced (toggle list)

Six toggles, all persisted alongside developer mode:

- **4K** — allow renditions above 1080p (off → cap at 1080p, default off).
- **Local Proxy** — route through per-session go-proxy port (default on).
- **Auto-Recovery** — single retry on non-codec player errors (default off).
- **Go Live** — `seekToDefaultPosition` on every load (default off).
- **Skip Home on launch** — auto-resume to Playback when a saved server and
  `lastPlayed` both exist; Home only mounts if the user presses Back from
  Playback. Default off.
- **Developer mode** — AVG/PEAK + decoder-lease overlay. Default off.

## Home screen

- **No top navigation row.** The "Home / Streams / Library / Search /
  Server / Settings" pill row was removed; settings are reachable from the
  drawer alone. Server picker is reachable via Back from Home.
- **Live preview tiles.** 3 visible tiles (not 4 or 6).
  - Hardwired to 360p HLS — `http://{host}:{apiPort}/go-live/{name}/playlist_6s_360p.m3u8`.
  - Hits the **API port directly**, not go-proxy — failure injection is
    irrelevant here and saves a hop.
  - **H.264 only** (`MimeTypes.VIDEO_H264` preference + `setMaxVideoSize(640, 360)`).
  - **No audio renderer.** Custom `DefaultRenderersFactory` overrides
    `buildAudioRenderers` to no-op, so the sibling Opus playlist
    (`playlist_6s_audio.m3u8`) has nowhere to decode to. Just disabling
    the audio track wasn't enough — it still spun up a decoder.
  - Cap is 3 simultaneous H.264 decoders on the MTK c2.mtk.avc decoder
    used by the Google TV Streamer. Active-decoder window is gated to
    keep within budget.
- **Preview pool ordering.** Slot 0 is Continue Watching (`lastPlayed`),
  remaining slots ordered by `viewCounts` DESC, then catalogue order.
  Distinct `clip_id`s only — same-clip-different-codec is deduped client-side
  in the preview row (the catalogue itself is not deduped).
- **Tile carousel rotates.** Scrolling the preview row pops the off-screen
  tile and replaces it with the next pool entry — only the 3 around focus
  hold a decoder.
- **Decoder leasing.** Tiles publish acquire/release calls around their
  player's actual ownership window. The main player's `prepare()` call on
  Home → Playback navigation waits (up to 1 s) for tile decoders to drop
  before starting, to avoid racing the chip's pool.
- **NO_MEMORY safety net.** If the main player still gets `NO_MEMORY` after
  the lease wait, retry is queued via the codec-error path — same up-to-3
  attempts.

## App lifecycle (codec budget)

`MainActivity.onStop` calls `vm.onActivityStopped()` which:

- `_appStopped = true` (broadcast through StateFlow).
- Stops + clears the main player (`vm.player.stop(); clearMediaItems()`).

Every `LivePreviewTile` reads `appStopped` and unmounts its inner
`ActivePlayerSurface` when true; the `DisposableEffect.onDispose` fires
`player.release()`, freeing the decoder slot.

`onStart` flips `appStopped = false` and tiles re-prepare. Net effect:
homing out of the app releases every video decoder InfiniteStream holds,
so YouTube (or whatever else takes foreground) gets a clean codec budget.

## URL shapes

**Main playback** (built by `buildUrlAndLoad`):

```
http://{server.host}:{port}/go-live/{selectedContent}/{manifest}?player_id={playerId}
```

- `port` = `server.port` when Local Proxy on, `server.apiPort` when off.
- `manifest` = `master{segment.suffix}.m3u8` (HLS) or `manifest{segment.suffix}.mpd` (DASH).

**Live preview tile** (always direct to API port, no player_id):

```
http://{server.host}:{server.apiPort}/go-live/{name}/playlist_6s_360p.m3u8
```

## Server-side contracts (relevant to the client)

- `/api/content` returns `ContentInfo` with `clip_id`, `codec`, `has_thumbnail`.
- `clip_id` = lowercased name with `_p200_<codec>[_TIMESTAMP]` stripped.
  Codec-agnostic — different encodes of the same logical clip share an id.
- `/api/content` newest-wins dedupes by `(clip_id, codec)` using the encode
  timestamp suffix or directory mtime tiebreak. The client trusts this.
- Thumbnails: 320 / 640 / 1280 widths, generated from the pre-burnin source
  with `ffmpeg -ss 10 ... thumbnail=300 ... split=3` (avoids black frames).
- Static thumbnail files (`.jpg/.jpeg/.png`) under `/go-live/` are served by
  nginx directly, not proxied to go-live.

## Persistence

`SharedPreferences`-backed:

- Saved servers list + active index.
- All Settings → Advanced flags + codec/segment/protocol selection.
- `lastPlayed` (last content with a successful first frame).
- `viewCounts` (per-clip-id, JSON-encoded).
- `/api/content` cache (stale-while-revalidate — Streamer Wi-Fi cold reads
  are ~2s, cache eliminates the wait on subsequent launches).

## Default codec

H.264. Every TV chip hardware-decodes it, so first-launch playback is
maximally likely to work. AUTO is selectable in Settings → Codec.

## Open / parked

- Slow `/api/content` cold-read (~2.1 s) on Streamer Wi-Fi — instrumentation
  retained, mitigated by cache. Not yet root-caused; not a Docker/server bug.
- Issue #266 (live-offset override in Settings) tracked separately, not in
  this rework.
