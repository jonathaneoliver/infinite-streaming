# HLS taxonomy — what each tag/event means, what the proxy does

Concise reference for the m3u8 tags that show up in our manifests, plus the event-type taxonomy the forwarder emits at `/api/v2/timeseries?streams=events`.

## Master playlist tags

| Tag | Meaning | Notes / proxy behaviour |
|---|---|---|
| `#EXTM3U` | Mandatory first line. Identifies the file as an m3u8 playlist. |
| `#EXT-X-VERSION:N` | Min playlist version required by features in this file. AVPlayer may refuse playlists with `VERSION > 9`. |
| `#EXT-X-STREAM-INF:BANDWIDTH=X,RESOLUTION=WxH,CODECS="..."` | Declares one variant. Followed by the variant URL on the next line. |
| `AVERAGE-BANDWIDTH=X` | Average over the duration; preferred for ABR estimation. Proxy can strip via `content --strip-average-bandwidth`. |
| `CODECS="avc1.X,mp4a.X"` | Required for a player to pre-filter unsupported variants. Proxy can strip via `content --strip-codecs`. |
| `FRAME-RATE=N` | We parse this for the iOS `frames_displayed` fix (#147). |
| `#EXT-X-MEDIA:TYPE=AUDIO,...` | Audio rendition group. Each variant references one. |

## Media (variant) playlist tags

| Tag | Meaning |
|---|---|
| `#EXT-X-TARGETDURATION:N` | Max segment duration. Players use this to size their fetch ahead. |
| `#EXT-X-MEDIA-SEQUENCE:N` | First segment's sequence number in this playlist. Increments as the live edge advances. |
| `#EXT-X-MAP:URI="init.mp4"` | Init segment for fMP4 variants. Required before any media segment can be decoded. AVPlayer caches the init per variant. |
| `#EXTINF:duration,` | One segment header, with duration. Followed by segment URL on next line. |
| `#EXT-X-PART:DURATION=N,URI="..."` | LL-HLS partial segment. Lower latency at the cost of more requests. |
| `#EXT-X-PRELOAD-HINT:TYPE=PART,URI="..."` | Tells the player to start fetching the next partial before it's announced. |
| `#EXT-X-DISCONTINUITY` | Boundary between non-contiguous segments (e.g. across a loop boundary). Players reset the decoder. |
| `#EXT-X-ENDLIST` | This is a VOD playlist; no more segments coming. We never emit this — all our streams are live. |

## Request kinds (proxy classifier)

The proxy classifies each request into one `request_kind`:

| Kind | What it is | Common faults applied |
|---|---|---|
| `master_manifest` | The top-level m3u8 (master playlist). | `404` → player can't play anything. `corrupted` not valid (master-only). |
| `manifest` | A variant media playlist. | `404` → that variant unavailable; player falls back. |
| `audio_manifest` | Audio rendition playlist. | Same as manifest. |
| `init` | `init.mp4` for fMP4 variants. | `corrupted` is segment-only; init faults manifest as decoder errors. |
| `segment` | Regular video media segment. | All fault types valid. |
| `audio_segment` | Regular audio media segment. | |
| `partial` | LL-HLS partial. Higher rate than `segment`. | |

## Event taxonomy (forwarder-derived)

These are what `harness ts --streams events` emits. Priority 1=critical → 4=low.

| Type | Priority | Kind | Meaning |
|---|---|---|---|
| `error` | 1 | effect | Player emitted an error. `info` has the player_error string. |
| `master_manifest_failure` | 1 | cause | Counter incremented on a master_manifest fault. Breaks playback. |
| `all_failure` | 1 | cause | Counter incremented when the catch-all `--kind` filter fired. |
| `stall` (≥3s) | 1 | effect | Paired stall_start→stall_end with duration. |
| `stall` (<3s) | 2 | effect | Same but shorter. |
| `stall` (`info=(frozen)`) | 2 | effect | Inferred from `last_event=frozen` — no explicit end. |
| `stall` (`info=(segment)`) | 2 | effect | Inferred from `last_event=segment_stall`. |
| `restart` | 2 | effect | Player tore down + rebuilt. |
| `transport_failure` | 3 | cause | nftables drop/reject increment. |
| `manifest_failure` / `segment_failure` | 3 | cause | Per-kind counter increment. |
| `transfer_active_timeout` / `transfer_idle_timeout` | 3 | cause | Server-side transfer timeout fired. |
| `fault_on` / `fault_off` | 3 | cause | Transport_fault_active transitioned. |
| `downshift` / `timejump` / `buffering` | 3 | effect | Player behaviour. |
| `http_5xx` / `request_timeout` | 2 | cause | HAR-derived. Real failures that often precede stalls. |
| `http_4xx` / `request_incomplete` / `request_faulted` | 3 | cause | HAR-derived. |
| `slow_request` (>2s wait) / `slow_segment` (>6s transfer) | 3 | cause | HAR-derived. |
| `request_retry` (same URL refetched <4s) | 4 | cause | HAR-derived. Often noise. |
| `upshift` / `play_start` | 4 | effect | Routine player behaviour. (`playback_start` was the pre-#622 name of a redundant first-render label; historical rows still carry it.) |
| `loop_server` | 4 | cause | Source content rotated back to start. Routine in our platform. |
| `user_marked` | 1 | effect | Operator pressed the iOS "911" button (always P1). |

`kind=cause` means proxy/system action; `kind=effect` means user-visible / player-emitted.

## See also

- `.claude/standards/avplayer-quirks.md` — how AVPlayer reacts to specific manifest features
- `.claude/standards/codec-strings.md` — what codec strings each platform requires
- forwarder `events_query.go` — the SQL that derives this taxonomy
