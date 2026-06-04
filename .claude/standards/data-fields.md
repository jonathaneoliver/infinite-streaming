# Data fields — canonical reference

What each field in the analytics tables (and the nested player-side blobs they carry) actually means, what units it's in, who populates it, and what gotchas to watch for when reasoning from it. Canonical for **both** the dashboard chat bot (via `read_standard("data-fields")`) and Claude Code.

**Scope of this version (Phase 1, issue #512):** the two most-queried tables — `session_events` and `network_requests` — plus the nested `player_metrics` / `server_metrics` blobs you'll see in their JSON responses. Other sources are covered in Phase 2 (`control_events`, plays summary, label vocabulary) and Phase 3 (characterization report shapes); until those land, this doc has a stub pointer at the bottom for each so you know where the gap is.

**How to use this doc:**
- The bot should `read_standard(name="data-fields")` **once per chat** when a field's meaning is non-obvious — the doc lands in context for the rest of the conversation.
- Claude Code should grep this file directly when interpreting raw tool/CH output.
- Field entries link to related standards / findings / skills where they exist; follow the links when you need deeper context on a specific behaviour.
- When the schema changes, **update this doc in the same PR** (the schema's row comments are not authoritative for semantics — this file is).

---

## How to read each entry

```
field_name                                                                [usage tags]
  type:        <CH type or JSON shape>
  units:       <ms, KB/s, ratio 0-1, ISO timestamp, …>  (absent = dimensionless)
  populated:   <player-SDK | forwarder | proxy | harness | operator>
  meaning:     <one-line semantic description>
  gotchas:     <surprising behaviour worth knowing>  (absent = none)
  see:         <related standards/findings>  (absent = none)
```

If a field is **0 by default** and 0 is also a meaningful value, the entry calls out "0 vs null" explicitly. Several iOS-supplied fields use 0 to mean "not measured yet" — don't confuse with "measured zero".

### Usage tags

Every entry carries one or more of these tags so the bot (and you) can prioritise reading. A doc entry can carry multiple tags.

```
[hot]        Used in almost every investigation. Cite when you mention
             the field. (player_state, buffer_depth_s, stall_count,
             video_resolution, bytes_out, ttfb_ms, request_kind, status.)

[forensics]  Needed specifically when chasing a failure or correlating
             with fault injection. (player_error, waiting_reason,
             fault_*, transport_*, client_rto_ms, response_headers.)

[char]       Relevant inside characterization-test investigations.
             (variant_activity, first_variant_picked, settled_variant,
             boundary_type, cap_mbps — most live in Phase 3.)

[ops]        Operator-set proxy state. Reflects current rules, not
             history. (nftables_pattern_*, all_failure_*, transfer_timeout_*.)

[debug]      Last-resort fields. Avoid quoting unless specifically
             needed. (request_headers, session_json, raw query_string.)
```

Future work: a `[hot]`-only extract baked into chat.md as a "fast path" reference. For now the bot loads the whole doc and the tags help it decide what to cite.

---

## 1. `session_events` (CH table)

**One row per player metrics POST.** The player SDK heartbeats (typically every 1s during playback, +event-driven on state transitions) plus the proxy-side `server_metrics` get folded into a single row at ingest. The hot columns below are promoted out of the underlying JSON for query speed; the full payload is also kept verbatim in `session_json` for any field that didn't get promoted.

Read via:
- Tier-1 typed tool: usually you'll see this data inside `find_plays` / `get_play_summary` aggregates rather than raw rows.
- Tier-3 raw: `query(sql="SELECT … FROM session_events WHERE player_id={…}…")` or `harness query events <play_id>` (which hits `GET /api/v2/events`).

The raw `/api/v2/events` response wraps each row's `current_play` map (containing nested `player_metrics`, `server_metrics`, etc.) — those nested blobs are documented in §1.b and §1.c below.

### Tag rollup for §1

```
[hot]        ts, player_id, play_id, attempt_id, classification,
             player_state, buffer_depth_s, position_s, playback_rate,
             video_resolution, video_bitrate_mbps, network_bitrate_mbps,
             video_quality_pct, video_first_frame_time_s, stall_count,
             stall_time_s, last_event, player_restarts, mbps_shaper_rate,
             manifest_variants, content_id, server_video_rendition,
             player_error

[forensics]  waiting_reason, last_stall_time_s, last_buffering_time_s,
             dropped_frames, player_error, client_rtt_ms,
             client_rtt_max_ms, client_rtt_min_ms, client_rtt_var_ms,
             client_rtt_min_lifetime_ms, client_rto_ms,
             client_path_ping_rtt_ms, mbps_shaper_avg, mbps_transfer_rate,
             *_failure_type, *_failure_mode, *_failure_frequency,
             transport_fault_*

[ops]        nftables_*, transfer_*_timeout_*, transfer_timeout_applies_*,
             content_allowed_variants, content_live_offset,
             content_strip_codecs, abrchar_run_lock

[debug]      session_json (raw blob — only when a column doesn't exist
             for what you need), control_revision, x_forwarded_port,
             x_forwarded_port_external
```

### 1.a Identity & lifecycle

```
ts
  type:        DateTime64(3, 'UTC')
  units:       UTC instant, millisecond resolution
  populated:   forwarder (at ingest, from the player's POST arrival)
  meaning:     when the row was recorded.
  gotchas:     this is the SERVER-side receive time. For the player's
               event time (when the metric was actually sampled), use
               player_metrics.event_time inside the nested blob.

player_id
  type:        LowCardinality(String) — UUID
  populated:   player-SDK
  meaning:     stable per device; persists across kill+relaunch on iOS
               (UserDefaults-backed; AX-readable from home screen).
  gotchas:     iOS / iPadOS / tvOS emit UPPERCASE UUIDs; CH is
               case-sensitive. All ingest paths canonicalise via
               canonicalV2ID() to lowercase. Don't compare raw uppercase
               from a chat handoff against CH rows without normalising.
  see:         [[case_sensitivity_ids]]

play_id
  type:        LowCardinality(String) — UUID
  populated:   player-SDK
  meaning:     one play of one content item from one player. Rotates on
               (a) user-initiated content change, (b) channel-change, or
               (c) explicit player.next() / restart that picks NEW content.
  gotchas:     does NOT rotate on control-mutations (fault toggles, shaper
               changes). Was a bug pre-#474; the proxy used to synth a
               new play_id on every operator mutation and that broke
               aggregation. Now stable across mutations.

attempt_id
  type:        UInt32
  populated:   player-SDK
  meaning:     monotonic counter per playback attempt within a play. 1
               on the initial play; +1 on every `restart` event (user OR
               auto-recovery). Resets to 1 at each new play_id.
               max(attempt_id) GROUP BY play_id = recovery attempts.
  gotchas:     0 = unknown (pre-rename rows or non-iOS clients). Treat
               0 specially.

session_id
  type:        String
  populated:   proxy
  meaning:     the proxy-port handle (1-8) for the player's testing
               session. Used as a routing key inside the proxy; not a
               unique row identifier.
  gotchas:     for archived sessions, session_id is constant ("1" or
               similar) and not useful. Use (player_id, play_id) instead.

group_id
  type:        LowCardinality(String)
  populated:   proxy (when the player is part of a coordinated group)
  meaning:     for multi-player grouped tests, the group handle. Empty
               for solo plays.

user_agent
  type:        String
  populated:   forwarder (from HTTP request header)
  meaning:     parsed device/OS/app identifier of the POSTing client.
  gotchas:     iOS sim reports the same UA as device-iOS — distinguish
               via player_id or labels[platform].

first_request_time / last_request
  type:        String (ISO timestamp as text)
  populated:   forwarder (lifecycle tracking)
  meaning:     first / last server-receive ts the forwarder has seen
               for this row's session. Updated on every POST.

session_duration
  type:        Float32
  units:       seconds
  populated:   forwarder
  meaning:     last_request - first_request, in seconds.

session_json
  type:        String (JSON blob)
  populated:   forwarder
  meaning:     the COMPLETE original POST payload — everything that
               wasn't promoted to a top-level CH column.
  gotchas:     parse on demand; very large for some clients. Most fields
               you'd want are already promoted; reach for session_json
               only when you can't find a column.

classification
  type:        LowCardinality(String) — 'other' | 'interesting' | 'favourite'
  populated:   forwarder (auto, on session-end) or operator (star button)
  meaning:     retention tier. 'other' = 30d TTL, 'interesting' = 90d,
               'favourite' = no TTL.
  see:         [[abr-decision-model]] for the auto-promotion rules.
```

### 1.b Player state (hot path — what charts show)

```
player_state
  type:        LowCardinality(String)
  populated:   player-SDK
  meaning:     current AVPlayer state. Common values: 'playing',
               'paused', 'buffering', 'idle', 'failed'.

waiting_reason
  type:        LowCardinality(String)
  populated:   player-SDK (iOS)
  meaning:     when player_state='buffering', AVPlayer's own
               reasonForWaitingToPlay enum. Common: 
               'AVPlayerWaitingWhileEvaluatingBufferingRateReason',
               'AVPlayerWaitingToMinimizeStallsReason'.
  gotchas:     empty when not buffering. Reading this for a state=playing
               row is meaningless.

buffer_depth_s
  type:        Float32
  units:       seconds (forward buffer ahead of playhead)
  populated:   player-SDK (computed from buffer ranges)
  meaning:     how many seconds of playable video are buffered ahead of
               the current playhead position. 0 = exhausted (=stall
               imminent or in progress).
  gotchas:     iOS reports this every heartbeat; 0 transient during
               normal transitions doesn't mean a stall — look for
               sustained 0 across multiple consecutive rows.

buffer_end_s
  type:        Float32
  units:       seconds (absolute position)
  populated:   player-SDK
  meaning:     the absolute media-timeline position of the END of the
               currently-buffered range. Differs from buffer_depth_s
               which is relative to the playhead.

position_s
  type:        Float32
  units:       seconds (absolute position)
  populated:   player-SDK
  meaning:     current playhead position in the media timeline.

playback_rate
  type:        Float32
  units:       ratio (1.0 = normal, 0 = paused, 0.5 = half-speed, …)
  populated:   player-SDK
  meaning:     AVPlayer's `rate` property. 0 during buffering AND during
               operator pause; you cannot distinguish those two from
               playback_rate alone — combine with player_state.

video_resolution
  type:        LowCardinality(String) — e.g. "1920x1080"
  populated:   player-SDK (from AVPlayerItem.presentationSize)
  meaning:     the resolution of the CURRENTLY-DISPLAYED video. Updates
               when the player switches variants.
  gotchas:     **carries forward from previous play** into the first
               heartbeats of a new play. Don't trust the first 1-2
               samples of a fresh play for what video is actually
               playing — they may reflect the previous session's idle
               state. Confirmed in 36-cycle startup characterization
               (issue #512 / 2026-05-25).

display_resolution
  type:        LowCardinality(String)
  populated:   player-SDK
  meaning:     the resolution of the DISPLAY (not the video). Hardware
               capability info.

video_bitrate_mbps
  type:        Float32
  units:       Mbps
  populated:   player-SDK (from AVPlayerItem.observedBitrate)
  meaning:     the EWMA-smoothed bitrate of recent video segment fetches,
               as reported by AVPlayer. Roughly tracks the variant the
               player is actually playing.

network_bitrate_mbps
  type:        Float32
  units:       Mbps
  populated:   player-SDK
  meaning:     iOS's instantaneous network-throughput estimate (NOT the
               cap, NOT the video bitrate — actual measured network).
  gotchas:     known UNRELIABLE at startup. Can be null or wildly
               over-cap (see [[avplayer-quirks]] and the 2026-05-25
               startup-overshoot finding). Don't use as a primary
               bandwidth signal without cross-checking against
               server-side `mbps_shaper_rate` or `mbps_transfer_rate`.

avg_network_bitrate_mbps
  type:        Float32
  units:       Mbps
  populated:   player-SDK
  meaning:     moving average of network_bitrate_mbps.

measured_mbps
  type:        Float32
  units:       Mbps
  populated:   player-SDK
  meaning:     a separate iOS network-bandwidth estimate. In practice
               very similar to network_bitrate_mbps; safe to treat as
               near-equivalent.

video_quality_pct
  type:        Float32
  units:       percent 0–100 (nominal; see gotchas)
  populated:   player-SDK (player_metrics_video_quality_pct — NOT
               forwarder-derived; verified against the main.go getF32
               mapping during #607 Phase 1)
  meaning:     position in the variant ladder. 100% = top variant,
               smaller values = lower variants.
  gotchas:     "3.35%" doesn't mean playback is 3% of full quality
               visually — it means the player is on the lowest of N
               variants where the bottom is mapped to ~floor pct. Read
               as a relative position, not perceptual quality.
               Top-variant rows routinely read 100.01 (player-side
               float overshoot; ~70% of archive rows) — treat <=101 as
               in-range. Values far above 100 (rare; observed up to
               457) occur on early-play rows before the ladder is
               known; video_resolution/content_id are empty on them.

video_first_frame_time_s
  type:        Float32
  units:       seconds (since playback intent)
  populated:   player-SDK
  meaning:     time-to-first-frame (TTFF). UX-critical: below 2s =
               instant; 2–5s = fine; >5s = user-noticed slow start.
  gotchas:     measured from playback-intent, not from URL change.

video_start_time_s
  type:        Float32
  units:       seconds (since playback intent)
  populated:   player-SDK
  meaning:     time until AVPlayer's timeControlStatus first flipped to
               .playing — "playing smoothly", the Conviva-style VST
               signal (PlayerViewModel.markPlayingStarted).
  gotchas:     typically >= video_first_frame_time_s — first frame
               renders (isReadyForDisplay), THEN playback rate ramps.
               The two are independent KVO observables, so the order
               is typical, not guaranteed. (This entry previously
               claimed the opposite direction; corrected during #607
               Phase 1 after 45% of archive rows "violated" it.)
```

### 1.c Stalls, errors, lifecycle counters

#### Legacy stall fields (DEPRECATED — replaced in #550)

`stall_count` / `stall_time_s` / `last_stall_time_s` / `last_buffering_time_s`
are kept as deprecated mirrors during the soft-cutover window. The
forwarder mirror-writes from the new canonical Phase 1 fields
(`stalling_count` / `stalling_time_ms` / `stall_duration_ms` /
`buffering_duration_ms`) so legacy consumers still get fresh data.
Prefer the new names in any new query. Old fields will be dropped in
a follow-up PR once consumers migrate.

#### #550 Phase 1 — state residency (per-state cumulative ms)

```
playing_time_ms / pausing_time_ms / buffering_time_ms /
stalling_time_ms / idling_time_ms / seeking_time_ms /
trickplaying_time_ms
  type:        UInt32 (DoubleDelta + ZSTD)
  units:       milliseconds (cumulative since play start)
  populated:   player-SDK (iOS state-machine residency tick)
  meaning:     total time the player has spent in each player_state.
               Sum across all states ≈ wall-clock since play started.
  gotchas:     resets to 0 at every play_id boundary. For per-snapshot
               deltas the forwarder also writes paired *_delta columns
               (already-computed, no lag() needed).

playing_count / pausing_count / buffering_count / stalling_count /
idling_count / seeking_count / trickplaying_count
  type:        UInt32
  populated:   player-SDK (state-machine entry counter)
  meaning:     count of distinct entries into each state. Goes up
               every time player_state transitions INTO the named state.

playing_time_ms_delta / *_delta
  type:        UInt32 (DoubleDelta)
  populated:   forwarder (computed from per-play state cache)
  meaning:     per-row delta of the paired cumulative *_time_ms.
               Use this instead of `x - lag(x) OVER (...)` — it's
               populated server-side already, bloom-filter-indexable,
               and the "who's buffering RIGHT NOW" tile is a one-liner:
               `WHERE buffering_time_ms_delta > 0 AND ts > now() - 5s`.

stall_duration_ms / buffering_duration_ms
  type:        UInt32
  populated:   player-SDK (set on the last_event='stall_end' /
               'buffering_end' row; sticky on subsequent heartbeats)
  meaning:     duration of the MOST RECENT stall / buffer event.
               Use for "longest single event in play" queries:
               `WHERE buffering_duration_ms > 5000 AND last_event =
               'buffering_end'`.
```

#### #550 Phase 2 — outcome status + structured errors

```
playback_status
  type:        LowCardinality(String)
  populated:   player-SDK (iOS) or forwarder default
  meaning:     terminal outcome enum. One of:
               - 'in_progress' (default, mid-session)
               - 'completed'   (natural EOF / clean exit)
               - 'user_stopped'
               - 'start_failure'      (VSF — fatal before first frame)
               - 'abandoned_start'    (EBVS — > threshold without start)
               - 'mid_stream_failure' (MSF — fatal after first frame)
  gotchas:     Sessions that never get a session_end POST stay
               'in_progress' until TTL ageout. Triage queries should
               filter `WHERE playback_status != 'in_progress'` for
               clean success-rate metrics.

playback_reason
  type:        LowCardinality(String)
  populated:   mirrors player_state during in_progress; classifier-
               derived on terminal rows
  meaning:     controlled-vocab reason per status. Examples per status:
               completed → natural_eof / loop_complete
               user_stopped → user_quit / app_backgrounded
               *_failure → drm_license_failed / segment_404 /
                           manifest_5xx / network_timeout / unknown / ...

error_code, error_domain, error_details
  type:        Int32 / LowCardinality(String) / String (JSON)
  populated:   player-SDK on every error-bearing row
  meaning:     per-row error context. Populated on `last_event='error'`
               events AND on terminal-failure session_end rows.

terminal_error_code, terminal_error_domain, terminal_error_details
  type:        Int32 / LowCardinality(String) / String (JSON)
  populated:   ONLY on terminal failure rows
  meaning:     SQL-safe terminal cause. Querying
               `WHERE terminal_error_code != 0` returns only plays that
               actually failed — never transient codes from recovered
               errors. Use this for failure-rate dashboards.

error_count, error_count_delta
  type:        UInt32 / UInt32 (DoubleDelta)
  populated:   player-SDK / forwarder (delta)
  meaning:     cumulative error observations across the play + per-row
               delta. High error_count even with playback_status =
               'completed' = the play recovered through hiccups.
```

#### #550 Phase 4 — device / platform / version taxonomy

```
device_class, device_model, player_tech, app_version,
os_version_major, os_version_minor, screen_width_px,
screen_height_px, screen_density
  type:        LowCardinality(String) / String / LowCardinality(String)
               / UInt16 / UInt16 / UInt16 / UInt16 / Float32
  populated:   player-SDK (DeviceInfo.swift on iOS) or go-proxy UA
               parser for external HLS players (VLC / ffplay / hls.js)
  meaning:     splits the conflated v1 `platform` field into the
               Conviva / Bitmovin canonical taxonomy. Stamped on every
               row (LowCardinality compresses the repeats); session-
               stable in practice, so `argMax(field, ts)` in plays-
               aggregation queries gives a clean per-play value.
  gotchas:     external HLS players get partial coverage from
               User-Agent parsing; fields default to empty when the
               UA doesn't match a known pattern. iOS app is the
               canonical source — when present, its values override
               go-proxy's UA-parsed defaults via setIfEmpty merge.

frames_displayed
  type:        UInt64
  populated:   player-SDK (monotonic)
  meaning:     cumulative frames rendered to screen.

dropped_frames
  type:        UInt32
  populated:   player-SDK (monotonic)
  meaning:     cumulative frames the decoder produced but the renderer
               dropped (typically decode pressure or display-rate mismatch).
               Don't confuse with frames the network failed to deliver —
               those just don't get counted here at all.

loop_count_player / loop_count_server
  type:        UInt32
  populated:   player-SDK / proxy
  meaning:     how many times the test loop has restarted on the
               player-reported view vs the server-reported view.
               Usually equal; divergence implies a missed restart event.

player_error
  type:        String
  populated:   player-SDK
  meaning:     the most recent AVPlayer error string. Empty when no
               error. Example: "The operation couldn't be completed.
               (CoreMediaErrorDomain error -15628.)".
  gotchas:     CoreMediaErrorDomain -12174 is documented sim-only noise
               (issue #145) — not a real error. Other -15xxx codes are
               typically real network/decode errors.
  see:         [[reference_repo_memory]] for the -12174 doc

last_event
  type:        LowCardinality(String)
  populated:   player-SDK
  meaning:     the most recent event-type the player reported. Common:
               'heartbeat', 'state_change', 'buffering_start',
               'buffering_end', 'playing', 'paused', 'restart', 'error'.

trigger_type
  type:        LowCardinality(String)
  populated:   player-SDK
  meaning:     why THIS POST happened. Mirrors last_event for event-
               driven POSTs; 'heartbeat' for the 1s timer.

event_time
  type:        String (ISO timestamp)
  populated:   player-SDK
  meaning:     when the player sampled the metric (client-side wall clock).
               Differs from row's `ts` (server receive time) by ~RTT.

player_restarts
  type:        UInt32
  populated:   player-SDK (monotonic; lives inside player_metrics)
  meaning:     count of player.restart() / auto-recovery events. Closely
               related to attempt_id (which is the per-play index).
```

### 1.d Server-side timing & RTT (proxy-derived)

```
client_rtt_ms
  type:        Float32
  units:       milliseconds
  populated:   proxy (TCP_INFO via getsockopt, sampled at 100Hz, folded
               to 1s windows, drained on each snapshot POST). Issue #401.
  meaning:     smoothed RTT from the kernel's TCP stack for the player's
               most-recent active connection.
  gotchas:     this is QUEUE-influenced. When the proxy is shaping the
               link, RTT climbs because of bufferbloat. To get a
               queue-independent path latency, use client_path_ping_rtt_ms.

client_rtt_max_ms / client_rtt_min_ms / client_rtt_var_ms
  type:        Float32
  units:       ms
  populated:   proxy
  meaning:     window max / min / variance of TCP RTT during the 1s
               sample window.

client_rtt_min_lifetime_ms
  type:        Float32
  units:       ms
  populated:   proxy (Linux 4.6+ kernel sticky min)
  meaning:     the kernel's per-connection sticky min-RTT — the best
               RTT ever seen on this TCP connection. Doesn't decay.
               Useful baseline against which to compare current rtt.

client_rto_ms
  type:        Float32
  units:       ms
  populated:   proxy
  meaning:     the kernel's current Retransmission Timeout. Rises during
               wedges while smoothed rtt flatlines — the GAP between
               rto and rtt is the "kernel suspects this connection is
               stalling" signal.

client_path_ping_rtt_ms
  type:        Float32
  units:       ms
  populated:   proxy (out-of-band ICMP echo at 1 Hz). Issue #404.
  meaning:     path latency to the player, INDEPENDENT of the streaming
               TCP connection's queue. Stays put when shaping kicks in
               while client_rtt_ms climbs from queueing.
  gotchas:     the right signal when you want to ask "what's the
               underlying network latency, ignoring buffer fill?"

mbps_shaper_rate
  type:        Float32
  units:       Mbps
  populated:   proxy
  meaning:     the CURRENT applied shaper cap (instantaneous). Reflects
               what the proxy is configured to limit egress to.

mbps_shaper_avg
  type:        Float32
  units:       Mbps
  populated:   proxy
  meaning:     1s-window average of the shaper rate (smooths over
               pattern step changes).

mbps_transfer_rate
  type:        Float32
  units:       Mbps
  populated:   proxy
  meaning:     observed instantaneous throughput the proxy actually
               served on this connection (different from the cap —
               might be lower if the player wasn't requesting).

mbps_transfer_complete
  type:        Float32
  units:       Mbps
  populated:   proxy
  meaning:     throughput at the moment of the last completed response.
```

### 1.e Content / manifest

```
manifest_url
  type:        String
  meaning:     current manifest the player is fetching.

manifest_variants
  type:        String (JSON array)
  populated:   forwarder (parsed once from the master manifest)
  meaning:     the variant ladder advertised in the master manifest.
               Each entry: { average_bandwidth, bandwidth, resolution, url }.
  gotchas:     this is the ADVERTISED variant ladder (avg + peak from
               the m3u8). Actual sustainable bitrate may be lower — see
               the pyramid stall finding for an example where 360p peak
               (0.998 Mbps) was insufficient for sustained playback
               (player needed ~1.3 Mbps).

last_request_url
  type:        String
  meaning:     last URL the player requested (manifest, segment, init).

content_id
  type:        LowCardinality(String)
  populated:   forwarder (parsed from URL)
  meaning:     short identifier for the content asset (e.g.
               "INSANE_FPV_SHOTS_Hydrofoil_Windsurfing_p200_h264_…").

server_video_rendition
  type:        LowCardinality(String)
  populated:   proxy
  meaning:     server-side knowledge of which variant the player is
               on (from the last segment URL it fetched).

server_video_rendition_mbps
  type:        Float32
  units:       Mbps
  populated:   proxy
  meaning:     the variant's advertised bitrate.
```

### 1.f Fault-injection & shaping fields (operator-set)

These are populated by the harness via `harness fault add` / `harness shape` / `harness pattern` mutations. They reflect the CURRENT state of the proxy's injection rules — not the historical state. To see fault-toggle history use `control_events` (see §3, Phase 2).

```
nftables_bandwidth_mbps
  units:       Mbps
  meaning:     OPERATOR INTENT — the rate the slider / harness set. 0
               means "no operator override." For the rate the KERNEL
               is actually enforcing, read effective_rate_limit_mbps
               below.

effective_rate_limit_mbps
  units:       Mbps
  meaning:     KERNEL-ENFORCED cap at this instant: max of the operator
               override (nftables_bandwidth_mbps) and the deployment
               baseline (INFINITE_STREAM_DEFAULT_RATE_MBPS env, see
               GET /api/v2/info → default_rate_mbps). 0 means truly
               uncapped (prod-style deployments with operator slider
               at 0). Distinct from nftables_bandwidth_mbps because
               on test-dev (baseline=100) the kernel caps at 100
               even when the operator hasn't set anything — the
               difference between intent and enforcement explains
               "why is throughput capped at X with no visible
               operator limit?" Issue #480.

nftables_delay_ms / nftables_packet_loss
  units:       ms / ratio 0-1
  meaning:     additional delay / packet-loss injection on the link.

nftables_pattern_enabled
  type:        UInt8 (0 | 1)
  meaning:     1 = a stepped/scripted pattern is driving the shaper;
               nftables_bandwidth_mbps is then ignored.

nftables_pattern_step / nftables_pattern_step_runtime
  meaning:     pattern progress — current step index, and runtime within
               that step.

nftables_pattern_rate_runtime_mbps
  units:       Mbps
  meaning:     the cap currently applied by the running pattern step.

nftables_pattern_margin_pct
  units:       percent
  meaning:     safety margin baked into the pattern step calculation.

nftables_pattern_template_mode
  type:        LowCardinality(String)
  meaning:     which template generated the steps (e.g. 'pyramid',
               'rampup', 'rampdown').
```

Failure-injection categorical columns (`manifest_failure_type`, `segment_failure_type`, `transport_failure_type`, `all_failure_type`, `master_manifest_failure_type`) plus their `_mode` / `_frequency` / `_consecutive_failures` / `_urls` siblings record the proxy's CURRENT injection rules. Document in detail in Phase 2; for now: presence + non-empty `*_failure_type` means the proxy will misbehave on matching requests; the matching request count is in the `_count` column. See [[fault-injection-wire-contract]] for the wire shape and semantics.

### 1.g Nested blobs: `player_metrics`, `server_metrics`, `current_play`

When you query session_events via `/api/v2/events` (harness query events <play_id>), the response wraps each row's full original POST payload — many fields you'd expect as columns live inside nested objects:

- **`player_metrics`** — duplicates the hot player-side columns (buffer_depth_s, video_bitrate_mbps, etc.) AND carries some that are NOT promoted to columns: `first_frame_time_s`, `playhead_wallclock_ms`, `error`, `live_offset_s`, `seekable_end_s`, `source` ('ios' | 'roku' | 'web'), `true_offset_s`. Read these from inside the nested object — the top-level row mirrors only the promoted subset.

- **`server_metrics`** — proxy-side derived: `bytes_in_last`, `bytes_in_total`, `bytes_out_last`, `bytes_out_total`, `bytes_last_ts`, `mbps_in`, `mbps_out`, `mbps_shaper_rate`, `mbps_shaper_avg`, `mbps_transfer_rate`, `measurement_window_io`, `path_ping_rtt_ms`, `rendition_mbps`, `server_rendition`. Useful for cross-validating player-reported throughput against server-side ground truth.

- **`current_play`** — wraps player_metrics + server_metrics + manifest + started_at, scoped to the play active at the moment of the POST. Effectively the snapshot's "this is what was playing."

For most analysis, prefer the promoted columns over the nested fields — they're cheaper to query and the values are identical. Reach into the nested blob only when the field you need isn't promoted.

---

## 2. `network_requests` (CH table)

**One row per HTTP request the proxy handled.** Covers everything: master/variant manifest fetches, segment fetches, init segments. Logged at request completion (not at request start), so `ts` = the response's completion instant.

Read via:
- Tier-1 typed tool: indirectly — `find_plays` rolls up counters (`net_errors`, `net_faults`) but doesn't return per-row data.
- Tier-3 raw: `harness query network <play_id>` or `GET /api/v2/network_requests?play_id=...`.

### Tag rollup for §2

```
[hot]        ts, player_id, play_id, method, url, request_kind, status,
             content_type, bytes_out, ttfb_ms, transfer_ms, total_ms,
             client_wait_ms, faulted, fault_type, classification

[forensics]  fault_action, fault_category, status (when >=400),
             request_range, response_content_range

[debug]      request_headers, response_headers, query_string,
             entry_fingerprint, upstream_url (when investigating proxy
             routing only)
```

**Note on `transfer_ms`:** it IS the downstream write+flush time to the player (client-perceived receive) — but `dns/connect/tls/ttfb` are upstream-scoped, and `total_ms` is unreliably maintained. See §2.d; this family is the most-misread in the table (including by an earlier revision of this doc).

### 2.a Identity

```
ts
  type:        DateTime64(3, 'UTC')
  units:       UTC instant, ms resolution
  meaning:     response completion time (when proxy finished sending).
  gotchas:     for request START time, subtract total_ms. There is no
               separate "request_at" column.

player_id / play_id / attempt_id / session_id
  meaning:     same semantics as session_events. Used to scope rows
               to one player/play.

method
  type:        LowCardinality(String)
  meaning:     HTTP method. Almost always 'GET' for HLS.

url
  type:        String
  meaning:     the URL the player REQUESTED (proxy-relative — what the
               player saw).

upstream_url
  type:        String
  meaning:     the URL the proxy fetched UPSTREAM to fulfil the request
               (typically http://127.0.0.1:30005/go-live/...).

path
  type:        String
  meaning:     just the URL path portion (no scheme/host/query).

query_string
  type:        String
  meaning:     query-string portion of the URL.
```

### 2.b Classification

```
request_kind
  type:        LowCardinality(String) — 'manifest' | 'segment' | 'init' | 'other'
  populated:   proxy (heuristic from URL path)
  meaning:     what KIND of HTTP request this is, semantically. 'manifest'
               = .m3u8 / .mpd; 'segment' = .m4s / .ts / .mp4; 'init' =
               init.mp4 / .init.m4s; 'other' = everything else.
  gotchas:     'manifest' covers BOTH master and variant playlists — use
               URL pattern matching to distinguish (master usually has
               `master_` prefix; variant has `playlist_6s_<variant>`).

status
  type:        UInt16
  meaning:     HTTP response status code. 200 = success; 4xx/5xx = error.
               0 = upstream connection failed before any response was
               received (rare — usually upstream is local).

content_type
  type:        LowCardinality(String)
  meaning:     response content-type. m3u8 = 'application/vnd.apple.mpegurl',
               segments = 'application/octet-stream' or 'video/mp4'.
```

### 2.c Bytes

```
bytes_in
  type:        Int64
  units:       bytes
  populated:   proxy (request body size)
  meaning:     bytes the proxy received FROM the player on this request.
               Always 0 for GET (no request body); non-zero only for PUT/POST.

bytes_out
  type:        Int64
  units:       bytes
  populated:   proxy (response body size)
  meaning:     bytes the proxy WROTE to the socket toward the player as
               the response body.
  gotchas:     this counts bytes SUBMITTED to the kernel's TCP send
               buffer, NOT necessarily bytes the player RECEIVED. If
               the proxy is shaping (tc on eth0), the actual wire
               delivery rate is limited by the cap, but bytes_out can
               still reflect what was queued. For ABANDONED downloads
               (player aborts mid-segment), bytes_out is bounded by
               what the proxy wrote before the abort — usually a partial
               segment. Confirmed in the 0.931 Mbps pyramid analysis:
               a 19 MB 2160p segment showed bytes_out=942 KB because
               the player abandoned mid-transfer.

request_range
  type:        String
  meaning:     the `Range:` header value if the player sent one (byte
               range requests). Empty for normal full-resource fetches.

response_content_range
  type:        String
  meaning:     the proxy's `Content-Range:` response header value.
```

### 2.d Timing

These are the most-misinterpreted fields in the table. **Read this carefully.**

```
dns_ms / connect_ms / tls_ms
  type:        Float32
  units:       milliseconds
  populated:   proxy (when fetching upstream)
  meaning:     DNS resolution time, TCP connect time, TLS handshake time
               for the UPSTREAM connection (proxy → go-live), NOT for
               the player → proxy connection.
  gotchas:     in test-dev / local deploys, upstream is 127.0.0.1, so
               these are typically 0 (no DNS, no real connect, no TLS).
               In production they'd be meaningful. For PLAYER-side
               connection timing, this table has nothing — use
               session_events.client_rtt_ms instead.

ttfb_ms
  type:        Float32
  units:       ms
  populated:   proxy
  meaning:     time to first byte from UPSTREAM (proxy → go-live's first
               byte of response).

transfer_ms
  type:        Float32
  units:       ms
  populated:   proxy
  meaning:     DOWNSTREAM write+flush time to the player — the
               client-perceived `receive` phase, where traffic-shaping
               backpressure appears (streamToClientMeasured /
               NetworkLogEntry doc comment in go-proxy main.go).
  gotchas:     this entry previously claimed transfer_ms was UPSTREAM
               transfer time and "NOT wire-delivery to the player" —
               that described pre-2026-02-13 semantics (the field was
               re-pointed downstream in 9fb686d) and was corrected
               during #607 Phase 1 source-verification. Under shaping,
               transfer_ms DOES grow with the cap (e.g. ~103ms segment
               writes under a 100 Mbps cap on test-dev). dns/connect/
               tls/ttfb remain UPSTREAM-scoped — the table mixes the
               two views; read each field's scope individually.

total_ms
  type:        Float32
  units:       ms
  populated:   proxy
  meaning:     nominally max(time-to-upstream-headers, ttfb + transfer)
               — initially set when upstream headers complete, then
               lifted by mergeTotalTiming() to ttfb_ms + transfer_ms.
  gotchas:     the lift is NOT applied on every code path: ~26% of
               archive rows have total_ms ≈ ttfb_ms while transfer_ms
               is 100x larger (#607 Phase 1 census). Until that's
               fixed, prefer ttfb_ms + transfer_ms over total_ms for
               request duration. The old formula documented here
               (dns+connect+tls+ttfb+transfer) never matched the code.

client_wait_ms
  type:        Float32
  units:       ms
  populated:   proxy
  meaning:     time the player waited for the proxy's response (from
               the player's perspective). This IS the wall-clock-ish
               number — closest thing to "how long did the player feel
               this took". When the cap is throttling, this rises while
               total_ms stays sub-ms.
```

**Rule of thumb for "how long did the player wait":** `client_wait_ms` (time to first response byte) + `transfer_ms` (downstream write+flush) covers the request end-to-end from the player's side; the final kernel-buffer flush can still trail the last write, so for exact wire time under heavy shaping use (ts of THIS request - ts of NEXT same-variant segment request) under steady fetch cadence.

### 2.e Fault injection

```
faulted
  type:        UInt8 (0 | 1)
  populated:   proxy
  meaning:     1 = the proxy's fault-injection rules matched this request
               and applied a misbehaviour (returned an error, hung,
               corrupted, etc.). 0 = passed through normally.

fault_type
  type:        LowCardinality(String)
  populated:   proxy (when faulted=1)
  meaning:     which fault-injection rule fired. Examples:
               'manifest_failure', 'segment_failure', 'transport_failure',
               'all_failure', 'master_manifest_failure'.

fault_action
  type:        LowCardinality(String)
  populated:   proxy (when faulted=1)
  meaning:     what action was taken. Examples: 'error' (return 4xx/5xx),
               'hang' (keep connection open without sending), 'corrupt'
               (return garbage), 'reset' (TCP reset), 'drop' (silently
               drop the packets via nftables).
  see:         [[fault-injection-wire-contract]]

fault_category
  type:        LowCardinality(String)
  populated:   proxy
  meaning:     coarser grouping above fault_type. 'http' | 'transport' |
               'transfer' | 'manifest'.
```

### 2.f Headers (low-frequency, for debugging only)

```
request_headers / response_headers
  type:        String (JSON of header map)
  meaning:     captured headers for the request and response. Useful
               when an issue may relate to header content (Range, Auth,
               cookies, conditional GETs).
  gotchas:     adds query weight — don't SELECT these in fleet-scale
               aggregations.

entry_fingerprint
  type:        UInt64
  populated:   forwarder (for dedup)
  meaning:     dedup key. Multiple writes of the same logical request
               (e.g. proxy retries) get folded.
```

### 2.g Classification & retention

```
classification
  type:        LowCardinality(String) — 'other' | 'interesting' | 'favourite'
  populated:   forwarder (inherits the parent play's classification)
  meaning:     same retention tiering as session_events. 'other' = 30d,
               'interesting' = 90d, 'favourite' = no TTL.
```

---

## 3. `control_events` (CH table)

**One row per discrete operator / proxy / harness action.** Sibling of session_events / network_requests. The forensic glue between "the player did X" and "the rules of the world changed at time T." Without this table you cannot answer "was a fault injected when the stall happened" reliably.

Read via:
- Tier-1 typed tool: `get_control_events(player_id, play_id?, from?, to?, labels_has?, labels_not?)`.
- Tier-3 raw: `harness query control <player_id>` (positional is player_id, NOT play_id — see [[reference_harness_cli_gotchas]]) or `/api/v2/control_events?player_id=...`.

### 3.a Identity & lifecycle

```
ts
  type:        DateTime64(3, 'UTC')
  units:       UTC instant, ms resolution
  populated:   forwarder (at ingest from /api/control/stream)
  meaning:     when the action happened.

player_id / play_id / attempt_id / session_id
  meaning:     same semantics as session_events. play_id is the play
               *active when the action happened*; this is non-trivial
               for operator-initiated actions that fire between plays
               (e.g. content_changed during channel-change).

source
  type:        LowCardinality(String) — 'harness' | 'proxy' | 'auto'
  meaning:     who caused the action.
                 'harness' = operator-initiated via dashboard / harness CLI
                 'proxy'   = runtime auto-transition (fault loop, pattern
                             step advance, loop_server detection,
                             proxy-detected session_end)
                 'auto'    = automated test runner (placeholder; no emit
                             path yet)
  gotchas:     'harness' events are the ones to subpoena when investigating
               "why did the network change at 15:42:30" — they're the
               operator-or-script-driven mutations. 'proxy' events are
               consequences of earlier 'harness' actions.

event_fingerprint
  type:        UInt64
  populated:   forwarder (FNV over player_id + play_id + ts_ms + source +
               event + info)
  meaning:     dedup key — replay of the proxy's SSE on reconnect
               doesn't double-insert.

classification
  type:        LowCardinality(String) — 'other' | 'interesting' | 'favourite'
  meaning:     retention tier (same as session_events; 30d / 90d / forever).
```

### 3.b The `event` vocabulary (closed set)

**Read this carefully — it's the closed set of action types control_events can carry. Every event maps to one synthesized label (see §5). Listed by source.**

```
fault_on                     [proxy] [forensics] [hot]
  meaning:     a fault rule started firing at runtime (not when it was
               configured — when it actually engaged on a request).
  info JSON:   { rule, action } — which fault rule, what action it took.
  label:       warning=*fault_on

fault_off                    [proxy] [forensics] [hot]
  meaning:     a fault rule stopped firing. Pair with the preceding
               fault_on to compute fault duration.
  info JSON:   { rule }
  label:       info=*fault_off

pattern_step                 [proxy] [forensics] [hot]
  meaning:     proxy auto-advanced to the next step of a running shaper
               pattern (rampup / rampdown / pyramid / etc.).
  info JSON:   { step, rate_mbps, duration_s } — step index, the cap this
               step applies, and how long it'll hold.
  label:       info=*pattern_step + info=*pattern_step_<mode>
               (e.g. info=*pattern_step_rampUp)
  gotchas:     between consecutive pattern_step events the shaper is
               held at the previous step's rate. To know "what was the
               cap at time T," find the LAST pattern_step with ts <= T.

shaper_changed               [proxy] [forensics]
  meaning:     proxy applied a pattern-driven shaper config update.
               Mirrors the per-step rate. Distinct from shaper_config_change
               (which is an operator action).
  info JSON:   { rate_mbps, delay_ms, packet_loss }
  label:       info=*shaper_changed

loop_server                  [proxy]
  meaning:     proxy detected end-of-content and restarted the loop on
               the server-side counter. Server-equivalent of the player's
               restart event.
  info JSON:   { loop_count }
  label:       info=*loop_server

fault_rule_enabled           [harness] [forensics] [hot]
  meaning:     operator turned ON a fault rule. The rule won't fire yet
               until a request matches its scope (and that's when fault_on
               fires). This is the CONFIG event, not the fire event.
  info JSON:   { rule } — rule name and details
  label:       warning=*fault_rule_enabled
  gotchas:     warning-level (not info) because by-design this is going
               to degrade playback. If you see this label on a row, the
               operator-or-script intentionally introduced a fault.

fault_rule_disabled          [harness] [forensics]
  meaning:     operator turned OFF a fault rule. Pair with fault_rule_enabled
               for the rule's lifetime.
  label:       info=*fault_rule_disabled

fault_rule_config_change     [harness] [forensics]
  meaning:     operator edited a fault rule's parameters while it was
               active. Treat as semantically equivalent to disable+enable.
  label:       warning=*fault_rule_config_change

pattern_enabled              [harness] [forensics] [hot]
  meaning:     operator started a shaper pattern (the harness `shape <id>
               --rate=ramp(...)` family).
  info JSON:   { mode, steps, base_rate_mbps, margin_pct, … }
  label:       info=*pattern_enabled + info=*pattern_enabled_<mode>
               (e.g. info=*pattern_enabled_pyramid)

pattern_disabled             [harness] [forensics] [hot]
  meaning:     operator stopped a pattern. After this, the shaper holds
               whatever cap was last set (or unshapes if no fallback cap).
  label:       info=*pattern_disabled

pattern_config_change        [harness] [forensics]
  meaning:     operator edited a running pattern's parameters.
  label:       info=*pattern_config_change

shaper_config_change         [harness] [forensics] [hot]
  meaning:     operator set the shaper directly (not via a pattern).
               `harness shape <id> --rate 5` produces this.
  info JSON:   { rate_mbps, delay_ms, packet_loss }
  label:       info=*shaper_config_change
  gotchas:     this is the "the human/script changed the network cap at
               time T" record — biggest forensic signal when you're
               trying to correlate a player event with a network change.

timeouts_changed             [harness] [ops]
  meaning:     operator changed transfer timeouts. Rare.
  label:       info=*timeouts_changed

label_changed                [harness] [hot]
  meaning:     operator set / cleared KV labels on the session. The
               INFO payload's keys end up as additional labels[] entries
               on this row (kvLabelsFromInfo() — see labels.go:539).
  info JSON:   { <key>: <value>, ... }
  label:       info=*label_changed + one info=<key>_<value> per KV pair
  gotchas:     this is how test runners stamp run_id, test_name, platform,
               cap_mbps, cycle_idx, boundary, etc. onto a session.
               Search for label_changed events to reconstruct what tags
               a test framework applied.

content_changed              [harness] [hot]
  meaning:     operator pointed the session at a different content asset
               (channel-change OR start-of-test content selection).
  info JSON:   { content_id, master_url, ... }
  label:       info=*content_changed
  gotchas:     a content_changed often immediately precedes the start
               of a new play_id. Use it to find the boundary between
               two plays in a multi-content session.

session_start                [harness | proxy]
  meaning:     session lifecycle began. First row for this session.
  label:       info=*session_start

session_end                  [harness | proxy]
  meaning:     session lifecycle ended. Last row for this session.
  label:       info=*session_end

control_change               [harness | proxy] [debug]
  meaning:     generic fallback when the changed field hasn't been
               enumerated yet (forward-compat). Read info to see what.
  label:       info=*control_change
```

### 3.c info — the JSON payload

```
info
  type:        String (JSON; see per-event shapes above)
  meaning:     event-specific payload. NOT pre-parsed at ingest — you
               see the raw JSON string from CH and have to parse it.
  gotchas:     CH stores info as a String column, not a JSON column.
               When querying, use `JSONExtractString(info, 'rate_mbps')`
               etc. — DON'T try to parse the whole string in SQL.
```

### 3.d labels[] — computed at ingest

```
labels
  type:        Array(LowCardinality(String))
  populated:   forwarder (computeControlLabels() — labels.go:478)
  meaning:     synthesized `<severity>=*<event>` labels per the §3.b
               table above. Every control_events row gets AT LEAST ONE
               label (matching its event); some get more (e.g.
               label_changed expands KV pairs into per-pair labels).
  gotchas:     control_events labels ALL have the `*` synthMark prefix.
               There are no "direct" labels on this table — the table
               itself is a synthetic surface. Filter by `info=*X` not
               `info=X` when targeting control event types.
```

---

## 4. Plays summary (find_plays return shape)

**One aggregated row per play.** Built at query time by `internal/plays/find.go` from a JOIN across session_events + network_requests + the labels histogram. This is what `find_plays` returns (default `mode='summary'` aggregates further; `mode='rows'` returns these per-play rows directly).

Read via:
- Tier-1 typed tool: `find_plays(..., mode='rows', top_k=N)`.
- Tier-3 raw: the JOIN SQL is in `internal/plays/find.go:140-250`.

### 4.a Identity

```
play_id                                                                   [hot]
  meaning:     UUID — one play of one content item from one player.

player_id                                                                 [hot]
  meaning:     UUID — same player can have many plays.

attempt_id                                                                [hot]
  meaning:     the max(attempt_id) seen for this play. 0 = unknown
               (legacy data); >1 = the play had restart/recovery cycles.

attempt_count                                                             [hot]
  meaning:     alias of attempt_id (the count is currently `argmax`).

session_id / group_id / content_id
  meaning:     same semantics as session_events. content_id is the
               short asset identifier; useful for grouping by clip.
```

### 4.b Time bounds

```
started_at                                                                [hot]
  type:        ISO timestamp string
  meaning:     earliest session_events.ts for this play. Effectively
               "when the play first reported."
  gotchas:     if the player's first heartbeat takes a few seconds to
               arrive, this can lag the true play start by ~1s.

last_seen_at                                                              [hot]
  type:        ISO timestamp string
  meaning:     latest session_events.ts for this play. Effectively
               "when the play last reported." Compare to current time
               to assess "is this play still live."
```

### 4.c Event / lifecycle counters

```
metric_events                                                             [hot]
  meaning:     count of session_events rows for this play. Rough proxy
               for play duration (heartbeats are ~1 Hz).

stalls                                                                    [hot]
  meaning:     max(stall_count) seen across the play. Cumulative count
               of distinct stall events.
  gotchas:     0 = no stalls. >0 = at least one. To find which stalls,
               drill into events.

dropped_frames                                                            [hot]
  meaning:     max(dropped_frames) — cumulative dropped during the play.

last_state                                                                [hot]
  meaning:     argMax(player_state, ts) — the player's most recently
               reported state. Common values: playing, paused, buffering,
               idle, failed.
  gotchas:     'paused' could be operator pause OR end-of-content idle.

last_player_error                                                         [hot]
  meaning:     argMax(player_error, ts) — most recent error string.
               Empty = no error. Look at this BEFORE assuming a play
               succeeded.

restart_count                                                             [hot]
  meaning:     countIf(last_event = 'restart') — how many times the
               player.restart() / auto-recovery event fired.

error_event_count                                                         [hot]
  meaning:     countIf(last_event = 'error') — how many error events
               were reported on this play (distinct from `last_player_error`
               which is just the most recent error string).

user_marked_count                                                         [hot]
  meaning:     countIf(last_event = 'user_marked') — how many times the
               operator pressed the "mark this" button during the play.

frozen_count                                                              [hot]
  meaning:     countIf(last_event = 'frozen') — how many distinct "video
               is frozen" events. Different from stalls — frozen means
               the player reported the frame stuck without entering a
               buffering state.

segment_stall_count                                                       [hot]
  meaning:     countIf(last_event = 'segment_stall') — how many times a
               segment fetch stalled (took too long to deliver bytes).
```

### 4.d ABR signals

```
bitrate_shifts                                                            [hot]
  meaning:     count of distinct video_bitrate_mbps transitions across
               consecutive samples (with both values > 0).
  gotchas:     high values (>20 on a short play) = ABR thrash; the
               player kept changing its mind about which variant to play.

downshifts / upshifts                                                     [hot]
  meaning:     subsets of bitrate_shifts split by direction.
               downshifts = went to lower variant; upshifts = went to higher.

resolution_changes                                                        [hot]
  meaning:     count of distinct video_resolution transitions across
               consecutive samples.
  gotchas:     correlates with bitrate_shifts but counts the RESOLUTION
               step changes specifically — different from minor bitrate
               oscillation within the same resolution.

avg_quality_pct
  meaning:     avgIf(video_quality_pct, video_quality_pct > 0) — average
               position in the variant ladder during this play.
  gotchas:     skips 0-valued samples (those are "unknown"). A play that
               spent half its time at unknown won't have those counted.

min_quality_pct                                                           [hot]
  meaning:     minIf(video_quality_pct, video_quality_pct > 0) — lowest
               variant the player visited. 100 = stayed at top variant
               the whole play. Sub-10 values indicate the player hit
               the floor.

frames_displayed
  meaning:     max(frames_displayed) — cumulative frames rendered.

first_frame_s                                                             [hot]
  meaning:     round(max(video_first_frame_time_s), 2) — UX time-to-first-frame.
               <2s = instant. >5s = noticed slow start.
```

### 4.e Network / fault counters

```
net_events
  meaning:     count of network_requests rows for this play.

net_errors                                                                [hot]
  meaning:     countIf(status >= 400) — HTTP 4xx/5xx responses.
  gotchas:     this counts the response status the proxy SAW from upstream,
               not necessarily what the player saw (the proxy might have
               retried, or the fault rule might have synthesized an error).
               For "what error the player saw" combine with faulted=1
               rows.

net_faults                                                                [hot]
  meaning:     countIf(faulted = 1) — requests where the proxy's
               fault-injection rules fired.
```

### 4.f Failure-injection persistence counters

```
master_manifest_failures / manifest_failures / segment_failures /        [forensics]
all_failures / transport_failures
  meaning:     max(*_consecutive_failures) — the highest streak of
               consecutive failures observed for each category. Useful
               for "did the player give up vs recover" questions.

active_timeouts / idle_timeouts                                          [forensics]
  meaning:     max(fault_count_transfer_*_timeout) — count of transfer
               timeout fires of each kind.
```

### 4.g Labels

```
labels_total                                                              [hot]
  meaning:     sum(n) — total count of all labels[] entries across all
               events / network_requests in this play. Rough activity
               measure.

labels_distinct_count                                                     [hot]
  meaning:     count of distinct label strings seen on this play.

label_histogram                                                           [hot]
  type:        Array of [label, count] tuples
  populated:   forwarder (JOIN'd via labels_agg CTE)
  meaning:     per-label occurrence count across this play. e.g.
               [["critical=frozen", 3], ["warning=segment_stall", 5], ...].
  gotchas:     this is the THIRD aggregation surface — the `labels[]`
               column on session_events / network_requests / control_events
               is per-row; this is per-play. Use to summarise "what
               happened in this play" without scanning all the rows.

classification                                                            [hot]
  meaning:     same as session_events.classification — 'other' (30d TTL)
               | 'interesting' (90d TTL) | 'favourite' (no TTL).
```

---

## 5. Label vocabulary

Labels are `<severity>=<event>` strings attached to **every row** of `session_events`, `network_requests`, and `control_events`. The grammar:

```
<severity>=<event>          direct  — written when the event actually happened
<severity>=*<event>         synth   — derived by the forwarder from cross-row aggregation
```

The `*` (synthMark) before `<event>` is the only difference between the two flavours.

**Severities** (in escalating order, used for the dashboard's severity filter):
- `testing`  — operator/test-harness KV metadata, **not playback signal** (e.g. `testing=run_id_20260530T141942Z`, `testing=test_rampup`, `testing=platform_iphone`). Set by the automated test code via `LabelPlay`; emitted by `kvLabelsFromInfo`. **Unranked** — excluded from `worstSeverity`, so it never tints a row or bumps classification. Groups under its own dashboard tier (#571).
- `info`     — informational; not a failure (e.g. `info=*pattern_step`)
- `warning`  — concerning; possibly a failure (e.g. `warning=segment_stall`, `warning=*fault_on`)
- `error`    — failure (e.g. `error=stall_recovery_timeout`)
- `critical` — severe failure (e.g. `critical=frozen`, `critical=*qoe_stall_severe_startup`)

**Find what labels actually exist** by calling `list_labels(from=..., to=..., like='%...%')`. **DO NOT GUESS** label strings when constructing `find_plays(labels_has=[...])` filters — see [[reference_labelplay_value_encoding]] for why guessed labels silently match zero rows.

### 5.a Direct labels (written when the event happened)

Each `last_event` value on session_events maps to a direct label. The most common (from `analytics/go-forwarder/labels.go`):

```
rate_shift_up / rate_shift_down                                           [hot]
  produced:    when video_bitrate_mbps changed direction across consecutive
               samples (both values > 0).
  example:     info=rate_shift_up, info=rate_shift_down

video_first_frame                                                         [hot]
  produced:    when the player reports the first decoded frame.
  example:     info=video_first_frame

video_start_time                                                          [hot]
  produced:    when the player commits to a variant.
  example:     info=video_start_time

segment_stall                                                             [hot] [forensics]
  produced:    a segment fetch took too long (player-reported).
  example:     warning=segment_stall

frozen                                                                    [hot] [forensics]
  produced:    the player reported the video frame is stuck without
               entering a buffering state.
  example:     critical=frozen

user_marked                                                               [hot]
  produced:    operator pressed the "mark this" button.
  example:     info=user_marked

restart                                                                   [hot]
  produced:    player.restart() OR auto-recovery fired.
  example:     warning=restart

error                                                                     [hot] [forensics]
  produced:    player reported an error.
  example:     error=error  (yes, "error=error" is real and means the
               player's last_event was 'error'; check player_error
               for the actual message string)

stall_start / stall_end                                                   [hot] [forensics]
  produced:    boundaries of a stall pair on the player.
  example:     warning=stall_start, info=stall_end

buffering_start / buffering_end                                           [hot] [forensics]
  produced:    boundaries of a buffering pair on the player.
  example:     warning=buffering_start, info=buffering_end

timejump
  produced:    playhead position made an unexpected jump (seek-like
               transition without a seek event).
  example:     warning=timejump
```

For network_requests:

```
master_manifest / manifest / audio_manifest                              [hot]
  produced:    request_kind classification.
  example:     info=master_manifest, info=manifest, info=audio_manifest

segment / audio_segment / init / partial                                 [hot]
  produced:    request_kind classification (per-row, never "synthesized").
  example:     info=segment, info=audio_segment, info=init, info=partial

socket / client_disconnect / transfer_timeout                            [forensics]
  produced:    transport-level fault categories.
  example:     warning=socket, warning=client_disconnect, error=transfer_timeout
```

For control_events: see §3.b above — every event maps to one (or more) synth label.

### 5.b Synthesized labels (computed by the forwarder)

The `*` prefix indicates the forwarder created the label by **aggregating across rows** (not just looking at a single row's columns). Examples:

```
critical=*stall_severe_startup                                            [hot] [forensics]
  produced:    a stall during the startup phase that lasted longer than
               the "severe" threshold.

critical=*stall_severe_midplay                                            [hot] [forensics]
  produced:    a severe stall during normal playback (after startup
               completed).

warning=*stall_short_scrub                                                [forensics]
  produced:    a short stall during scrub. Likely user-induced.

info=*stall_short_midplay
  produced:    minor mid-play stall, below severity threshold.

error=*video_startup_severe                                               [hot] [forensics]
  produced:    startup took severely long (TTFF cliff).

warning=*segment_failure                                                  [forensics]
  produced:    a segment request failed AND the player was negatively
               affected (vs a transient retry).

warning=*transport_disconnect                                             [forensics]
  produced:    transport-level disconnect detected.

warning=*fault_incomplete                                                 [forensics]
  produced:    a fault rule was active but the player either didn't
               retry or the fault didn't reach the player.
```

Pattern for synth labels: they fold a `position bucket` (startup / midplay / scrub) with a `severity bucket` (short / long / severe). See `labels.go:295-340` for the precise thresholds.

### 5.c Filtering: `find_plays(labels_has=[...], labels_not=[...])`

- `labels_has=['critical=frozen']` — every named label must be on the play (AND).
- `labels_has=['critical=%']` — SQL LIKE: every critical-severity label (direct AND synth).
- `labels_has=['%=*%']` — every synth label.
- `labels_has=['%stall%']` — substring match — finds segment_stall, stall_severe_startup, etc.
- `labels_has=['stall']` — matches NOTHING. No bare `stall` label; needs `%`.
- `labels_not=['info=*pattern_step']` — every named label must NOT be present.

**The bot's standard flow:** call `list_labels(from='…', like='%X%')` first to see the actual strings, pick exact ones, then `find_plays(labels_has=[...])` with the verified strings. Don't guess.

**Forbidden characters in label values:** `,` and `=` (the grammar uses `=` as the severity separator). See [[reference_labelplay_value_encoding]] — silently dropped if used, no error.

### 5.d Severity precedence

When the dashboard rolls up multiple labels on one play, severity precedence is `critical > error > warning > info`. The dashboard's "worst signal" chip shows the highest-severity label present. (`testing` is **not** in this precedence — it's test-harness metadata, never the worst chip and never tints a row.)

---

## 6. Characterization report shapes

**One row per characterization test run** (`run_id, test_name`) is stored in `characterization_runs` (CH table; see §1.a's brief mention) and exposed via the `/api/v2/characterization-runs/{run_id}/{test_name}` endpoint plus the Tier-1 tool `get_characterization_step`. The row carries `report_json` — a serialised Go `runner.Report` struct.

This section documents the structure of `report_json` and its nested types so the bot can interpret raw report blobs without inferring from samples.

**Per-test test specifics live in the per-test standards docs:**
- [[startup-characterization-test]] — `app_cold` / `channel_change` boundaries, `StartupCycleResult` rows
- [[abort-characterization-test]] — server-driven segment-abort recovery, `AbortCycleResult` rows
- [[retry-backoff-characterization-test]] — persistent-fault retry, `RetryCycleResult` rows
- [[characterization-principles]] — cross-cutting design rules

Read those for **how to interpret a row's findings**. This section documents **what each field IS** for the cross-cutting shapes (Report, Step, variant_activity, Summary, Sample).

### Tag rollup for §6

```
[hot]    Report.mode, Report.player_id, Report.play_ids, Report.steps,
         Step.rate_mbps, Step.max_bitrate_mbps, Step.exit_reason,
         Step.buffer_at_start_s, Step.buffer_at_end_s,
         Summary.lowest_sustainable_cap_mbps,
         Summary.bottom_variant_floor_mbps,
         Summary.total_stalls, Summary.profile_shifts,
         Sample.ts, Sample.state, Sample.video_resolution,
         Sample.buffer_depth_s, Sample.video_bitrate_mbps,
         VariantActivity.resolution, VariantActivity.segment_fetches,
         VariantActivity.peeked_but_never_used

[char]   Almost everything in this section (the whole report shape
         only appears in characterization-test investigations).

[forensics]
         Step.stalls_delta, Step.profile_shifts_delta,
         Step.unexpected_upshift, Step.unexpected_downshift,
         StartupCycleResult.time_to_first_frame_s,
         StartupCycleResult.network_bitrate_at_start_mbps,
         AbortCycleResult.abort_detected, AbortCycleResult.recovery_s,
         RetryCycleResult.gave_up_url, RetryCycleResult.recovery_s

[debug]  Step.variant_idxes_seen (long arrays of integers — readable
         summaries are MajorVariantIdx + UnexpectedUpshift/Downshift),
         Sample.err (last-resort sample-level error string)
```

### 6.a `Report` — top-level

```
mode                                                                       [hot]
  type:        string
  meaning:     test mode that produced the report. One of:
               smooth | steps | rampup | rampdown | pyramid |
               transient-shock | startup-caps | downshift-severity |
               hysteresis-gap | emergency-downshift | startup | abort |
               retry-backoff.
  gotchas:     mode determines which result arrays are populated:
                 - linear (smooth/steps/ramp*/pyramid/transient-shock/
                   startup-caps/downshift-severity/hysteresis-gap/
                   emergency-downshift): populates steps[]
                 - startup: populates startup_cycles[]
                 - abort:   populates abort_cycles[]
                 - retry-backoff: populates retry_cycles[]
               Treat the unpopulated arrays as "not applicable to this
               mode," not as "empty result."

platform
  type:        Platform (string enum: iphone | ipad | ipad-sim | appletv |
               androidtv | web)
  populated:   harness (from device.go's Platform constants)
  meaning:     player runtime the test ran against.
  gotchas:     ipad-sim is the simulator (xcrun simctl); ipad is the
               real device — different cold-start latency profiles
               but same player binary.

device
  type:        Device { Platform, UDID, Label, BundleID }
  populated:   harness
  meaning:     human-readable device handle. UDID is the platform-native
               identifier (simctl UDID, devicectl UUID, adb serial,
               empty for web).

player_id                                                                  [hot]
  type:        string (UUID)
  meaning:     UUID of the player session under test. Identifies all
               session_events / network_requests for this test run.

play_ids                                                                   [hot]
  type:        []string (UUID array)
  meaning:     every play_id observed during the sweep. Usually one entry
               for smooth/steps modes; multiple for modes that relaunch
               the app (startup-caps, startup).
  gotchas:     first entry is the play active at sweep start; subsequent
               entries are post-relaunch / post-channel-change plays.

started_at
  type:        time.Time (ISO timestamp)
  meaning:     when the test framework started executing the sweep.

ended_at
  type:        time.Time (ISO timestamp)
  meaning:     when the test framework finished. ended_at - started_at
               = total wall-clock run time including all step holds.

variants                                                                   [char]
  type:        []VariantRate (see §6.b)
  populated:   harness (from VariantRatesDesc at sweep start)
  meaning:     the variant ladder + computed caps the test ROUNDED to.
               Used to translate Sample.variant_idx into a resolution
               or Mbps value at analysis time.
  gotchas:     ORDER MATTERS — variants are sorted DESCENDING by cap
               rate (index 0 = highest-quality variant). Sample.variant_idx
               and Step.expected_variant_idx are indexes INTO this array.

steps                                                                      [hot]
  type:        []Step (see §6.c)
  meaning:     one entry per applied-rate hold in the sweep. Populated
               for linear modes only — empty for cycle-based modes
               (startup, abort, retry-backoff).

samples                                                                    [debug]
  type:        []Sample (see §6.e)
  meaning:     all per-second player metric samples collected during
               the run. Large array — typically ~1Hz across the entire
               sweep duration.
  gotchas:     prefer the aggregated step results (Step.* fields) over
               iterating samples — they're computed once at sweep end
               and cheaper to read. Only iterate samples when you need
               sub-step-granularity detail (e.g. "when exactly did
               buffer hit 0?").

summary                                                                    [hot]
  type:        Summary (see §6.d)
  meaning:     run-wide aggregates (total stalls, lowest sustainable
               cap, bottom variant floor). The dashboard's per-run
               summary chips read from this.

abort_cycles / startup_cycles / retry_cycles                              [char]
  type:        []AbortCycleResult / []StartupCycleResult / []RetryCycleResult
  meaning:     per-cycle observations for cycle-based test modes.
               Exactly one of these will be populated; the others are
               empty for that mode. See the per-test standards for
               field interpretation.
```

### 6.b `VariantRate` — one entry in `Report.variants[]`

```
resolution                                                                 [hot]
  type:        string — "3840x2160" etc.
  meaning:     the variant's display resolution from the master playlist.

url                                                                        [debug]
  type:        string
  meaning:     the variant playlist URL the master pointed at.

avg_bps / peak_bps
  type:        int (bits per second)
  populated:   harness (from HLS master playlist tags)
  meaning:     AVERAGE-BANDWIDTH and BANDWIDTH attributes per the HLS
               spec. avg_bps = mean segment rate; peak_bps = peak.
               peak_bps is always > avg_bps; avg can be 0 if the
               manifest omitted the tag.

raw_bps
  type:        int
  meaning:     the value used for cap calc — avg_bps if present, else
               peak_bps. The test caps to (raw_bps × (1 + margin/100)).

source
  type:        string — "average" | "peak"
  meaning:     which attribute fed raw_bps.

margin_pct
  type:        int (percent)
  meaning:     headroom the test added when computing cap_mbps. 5 =
               cap is 5% above raw rate.

cap_mbps                                                                   [hot]
  type:        float64 (Mbps)
  meaning:     final applied cap for this variant = raw_bps × (1 + margin/100) / 1e6.
               This is the SHAPER value the proxy uses, not the variant's
               own bitrate.
```

### 6.c `Step` — one entry in `Report.steps[]` (linear modes only)

```
started_at / ended_at                                                      [hot]
  type:        time.Time (ISO timestamps)
  meaning:     wall-clock boundaries of the step. Use to compute
               actual hold time (vs intended Hold duration).

rate_mbps                                                                  [hot]
  type:        float64 (Mbps)
  meaning:     the applied shaper cap during this step.

hold_ns                                                                    [debug]
  type:        time.Duration (ns)
  meaning:     intended duration to hold the step. May exit early —
               see exit_reason.

variant                                                                    [char]
  type:        *VariantRate (pointer)
  meaning:     which rung + margin produced this cap. NIL for plain
               rate-ramp modes (rampup, rampdown, etc.); populated for
               variant-aware modes (smooth, steps).

exit_reason                                                                [hot]
  type:        string — "full" | "early-stable" | "cancelled"
  meaning:     why the test left this step.
                 - "full"         held the full Hold duration
                 - "early-stable" buffer was stable over early-exit window
                 - "cancelled"    ctx fired (timeout / test stop)
  gotchas:     exit_reason="cancelled" on a non-last step means the
               sweep was interrupted — subsequent steps may not have run.

min_buffer_s / max_buffer_s                                                [hot]
buffer_at_start_s / buffer_at_end_s                                        [hot]
  type:        float64 (seconds)
  meaning:     buffer envelope during the step. Together they paint
               the full trajectory: start → trough (min) → recovery
               (max) → end. Don't need to open the session viewer
               for this story.
  gotchas:     start can equal end if the step was very short; min/max
               span the whole step including both endpoints.

stalls_delta                                                               [forensics]
  type:        int
  meaning:     count of stall events triggered DURING this step
               (delta of stall_count from start to end). Any value >0
               on a low-rate step usually indicates buffer depletion.

profile_shifts_delta                                                       [forensics]
  type:        int
  meaning:     count of ABR transitions the player reported during the
               step (delta of profile_shift_count).
  gotchas:     >1 means the player thrashed (multiple variant changes);
               1 means one clean down/upshift; 0 means it stayed put.

mean_bitrate_mbps / max_bitrate_mbps                                       [hot]
  type:        float64 (Mbps)
  meaning:     player's reported video bitrate, averaged / max over
               the step. max_bitrate distinguishes "settled at variant X"
               from "briefly spiked to richer variant then settled lower."

mean_network_bitrate_mbps / max_network_bitrate_mbps                       [forensics]
  type:        float64 (Mbps)
  meaning:     player's measured network throughput. Should be close
               to (but slightly below) rate_mbps if tc is enforcing
               properly. Over-cap excursions in max_network often
               point at the proxy's tc bursting.

sample_count
  type:        int
  meaning:     number of Samples that fell within [started_at, ended_at].
               Short steps may have <5 samples; treat aggregates as
               low-confidence under that threshold.

expected_variant_idx                                                       [char]
  type:        int (index into Report.variants)
  meaning:     the rung the cap was built for. -1 = no variant binding
               (non-variant-aware mode).

variant_idxes_seen                                                         [debug]
  type:        []int (length = len(Report.variants))
  meaning:     count of samples observed at each variant rung during
               the step.
  gotchas:     verbose. Prefer major_variant_idx + the unexpected_*
               booleans for summarisation.

major_variant_idx                                                          [char]
  type:        int (index into Report.variants)
  meaning:     the most-observed rung during the step. -1 = no samples
               classified.

unexpected_upshift / unexpected_downshift                                  [forensics]
  type:        bool
  meaning:     true when major observed rung differs from expected
               in the named direction. upshift = player picked richer
               variant than target (cap was loose); downshift = player
               picked lower variant than target (cap was tighter than
               the player could sustain at the expected variant).
  gotchas:     these are the easiest "did the player do what we
               expected" signals — far cheaper than scanning
               variant_idxes_seen.
```

### 6.d `Summary` — run-wide aggregates

```
total_stalls                                                               [hot]
total_stall_seconds
  type:        int / float64 (seconds)
  meaning:     run-wide stall counters across all samples.

max_buffer_depth_s / min_buffer_depth_s
  type:        float64 (seconds)
  meaning:     buffer envelope across the whole run.

mean_bitrate_mbps / min_bitrate_mbps / max_bitrate_mbps                    [hot]
  type:        float64 (Mbps)
  meaning:     video bitrate envelope across the whole run.

profile_shifts                                                             [hot]
  type:        int
  meaning:     total ABR transitions across the whole run (sum of
               steps' profile_shifts_delta).

dropped_frames
  type:        int
  meaning:     run-wide cumulative dropped frames.

sample_count                                                               [hot]
  type:        int
  meaning:     total Samples in Report.samples.

variant_sample_counts                                                      [char]
  type:        []int (length = len(Report.variants))
  meaning:     per-variant sample count across the entire run.
               Index aligns with Report.variants. A 0 means the player
               never visited that rung. smooth_test.go asserts every
               variant has >0 samples; deviations are test-design or
               player-behaviour signals.

lowest_sustainable_cap_mbps                                                [hot]
  type:        float64 (Mbps)
  meaning:     smallest applied cap that kept buffer above
               SustainableBufferS (1.0s) for the entire step AND
               produced no stalls. The next-lower cap is the first
               that broke either rule.
  gotchas:     0 = the sweep never found a sustainable step (every
               cap stalled or depleted). The headline number for
               "how low can this player go on this content."

highest_stalling_cap_mbps
  type:        float64 (Mbps)
  meaning:     largest applied cap that depleted buffer OR stalled.
               The boundary between safe and unsafe.

bottom_variant_floor_mbps                                                  [hot]
  type:        float64 (Mbps)
  meaning:     largest applied cap that caused a stall or buffer
               depletion while the cap's target was the BOTTOM rung
               (lowest variant in the ladder). Qualitatively distinct
               from highest_stalling_cap_mbps — at the bottom rung
               there's nowhere lower to go, so this is a definitive
               "cap cannot deliver this content" threshold.
```

### 6.e `Sample` — one entry in `Report.samples[]`

Samples are the per-second telemetry rows the test sampler collected.

```
ts                                                                         [hot]
  type:        time.Time
  meaning:     when the sample was taken.

applied_rate_mbps
  type:        float64 (Mbps)
  populated:   harness (sampler annotates with what rate the caller
               said was applied at sample time)
  meaning:     NOT the shaper-reported value — what the test was
               trying to apply. Useful for "did the proxy actually
               apply what the test told it to."

state                                                                      [hot]
last_event                                                                 [hot]
  type:        string
  meaning:     player state + last reported event (mirrors
               session_events.player_state + .last_event).

buffer_depth_s / buffer_end_s                                              [hot]
  type:        float64 (seconds)
  meaning:     forward buffer from playhead / absolute buffer-end position.
  gotchas:     on AVPlayer buffer_depth_s often reports 0 even with
               buffered content; buffer_end_s is more reliable on iOS.

stalls                                                                     [hot]
stall_time_s
  type:        int / float64
  meaning:     cumulative stall count + cumulative stall seconds
               (NOT per-sample — monotonic across the run).

profile_shift_count
  type:        int
  meaning:     cumulative ABR transitions (monotonic). Used to
               compute Step.profile_shifts_delta.

video_bitrate_mbps                                                         [hot]
  type:        float64 (Mbps)
  meaning:     player-reported observed video bitrate (EWMA-smoothed).
               Variant the player is actually playing.

video_first_frame_time_s                                                   [forensics]
  type:        float64 (seconds)
  meaning:     TTFF from play-start. Per-play (resets on new play_id).
               Authoritative for startup-cycle TTFF measurement.

play_id                                                                    [char]
  type:        string (UUID)
  meaning:     current play's UUID. Changes when the player starts a
               new play (channel change, app relaunch). Used by
               startup_test to find the new-play transition.

video_quality_pct                                                          [hot]
  type:        float64 (percent 0-100)
  meaning:     position in the variant ladder. 100 = top variant.
               Same semantics as session_events.video_quality_pct.

video_resolution                                                           [hot]
  type:        string — "960x540" etc.
  meaning:     DISPLAYED video resolution.
  gotchas:     LAGS the actually-being-fetched variant by a few seconds —
               the player switches by REQUESTING new segments, but the
               screen keeps showing the old variant until those segments
               decode and render. For "what the user sees" this is right;
               for "what the player is downloading" use variant_idx
               (bitrate-based, leading indicator).

network_bitrate_mbps                                                       [forensics]
  type:        float64 (Mbps)
  meaning:     player's instantaneous measured network throughput.
               Useful for verifying the proxy's tc cap is biting.
  gotchas:     known UNRELIABLE at startup (see [[avplayer-quirks]]).

avg_network_bitrate_mbps
  type:        float64 (Mbps)
  meaning:     long-term average — seeded by the warm-up step before
               the sweep begins.

dropped_frames
  type:        int
  meaning:     cumulative dropped frames (monotonic).

position_s
  type:        float64 (seconds)
  meaning:     current playhead position.

variant_idx                                                                [char]
  type:        int (index into Report.variants)
  meaning:     what the player is currently FETCHING, derived from
               video_bitrate_mbps (closest variant by raw rate).
               LEADING indicator of ABR decisions. -1 = unclassified.

displayed_variant_idx                                                      [char]
  type:        int (index into Report.variants)
  meaning:     what the player is currently RENDERING, derived from
               video_resolution. LAGGING indicator (catches up to
               variant_idx a few seconds after a switch). -1 = none.

err                                                                        [debug]
  type:        string
  meaning:     sample-level error string. Empty when sampler succeeded.
```

### 6.f `StartupCycleResult` — one entry in `Report.startup_cycles[]`

Used by the `startup` test mode. **Field interpretation belongs in [[startup-characterization-test]]** — this is just the data dictionary.

```
cycle_idx                                                                  [hot]
boundary_type                                                              [hot]
  type:        int / string ("app_cold" | "channel_change")
  meaning:     the independent variable of the cycle.

content_clip_id
  type:        string
  meaning:     target clip for THIS cycle. Constant across cycles in a
               run (the test's design rule — see startup standard).

cap_mbps                                                                   [hot]
  type:        float64
  meaning:     network cap applied before the boundary fired.

started_at
  type:        time.Time
  meaning:     t=0 reference for the cycle. All *_at_s timings are
               relative to this.

player_id
  type:        string (UUID)
  meaning:     read from the home AX node before resume.

first_master_at_s / first_variant_at_s / first_segment_at_s                [hot]
  type:        float64 (seconds since started_at)
  meaning:     when each request kind first hit the proxy. The deltas
               between them reveal master-parse + ABR-decision time.

first_variant_picked                                                       [hot]
  type:        string (resolution, e.g. "3840x2160")
  meaning:     the variant the player chose FIRST (read from the first
               variant-playlist URL it fetched). Audio playlists skipped.
  gotchas:     same gotcha as Sample.variant_idx — this is what the
               player REQUESTED first, not necessarily what it played.
               Combined with variant_at_5s shows the startup trajectory.

first_variant_avg_mbps / first_variant_peak_mbps                           [char]
  type:        float64 (Mbps)
  meaning:     manifest avg+peak bandwidth for first_variant_picked.

settled_variant_avg_mbps / settled_variant_peak_mbps                       [char]
  type:        float64 (Mbps)
  meaning:     same for settled_variant.

pre_play_id / play_id                                                      [char]
  type:        string (UUID)
  meaning:     pre_play_id = play active BEFORE boundary; play_id = play
               the boundary started (cycle MEASURES this play). Required
               for the session-viewer link from the dashboard.

time_to_first_frame_s                                                      [hot] [forensics]
  type:        float64 (seconds)
  meaning:     iOS-reported video first-frame time. THE UX number.
               <2s = instant; 2-5s = fine; >5s = noticed slow start.

first_req_dns_ms / first_req_connect_ms / first_req_tls_ms
  type:        float64 (milliseconds)
  meaning:     median across the first ~5 requests. Reveals TLS
               resumption, TCP keepalive, DNS cache hits.
  gotchas:     currently 0-stubbed in some deployments — NetworkRow
               schema may not yet carry the timing columns.

reached_5s_buffer_at_s / reached_15s_buffer_at_s                           [forensics]
  type:        float64 (seconds since started_at)
  meaning:     when buffer first crossed N seconds. 0 = never reached.

variant_at_5s / variant_at_15s / variant_at_30s                            [char]
  type:        string (resolution)
  meaning:     sampled video_resolution at marks — startup trajectory.

upshifts_in_30s / downshifts_in_30s                                        [char]
  type:        int
  meaning:     profile_shift_count delta over the observation window.
               (Note: current implementation may lump both into upshifts.
               See per-test standard.)

stalls_in_30s                                                              [hot] [forensics]
dropped_frames_in_30s
  type:        int
  meaning:     deltas over the observation window.

settled_variant                                                            [hot]
  type:        string (resolution)
  meaning:     majority-sample resolution in the LAST 10s of the window.
               Empty = never stabilised.

network_bitrate_at_start_mbps                                              [forensics]
network_bitrate_at_30s_mbps
  type:        float64 (Mbps)
  meaning:     player's bandwidth estimate at start / end of the window.
               THE TELL for channel_change: non-zero at start = bandwidth
               estimate carried from previous play.
  gotchas:     unreliable in practice (see [[startup-characterization-test]]
               post-2026-05-25 finding). Often null on app_cold and
               wildly over-cap on channel_change. Don't trust as a
               primary signal.

variant_activity                                                           [hot]
  type:        []VariantActivity (see §6.g)
  meaning:     per-variant fetch breakdown over the 30s window. Lets you
               see "the player fetched 1440p's playlist but never any
               segments" (peeked_but_never_used).
```

### 6.g `VariantActivity` — one entry in `StartupCycleResult.variant_activity[]`

```
resolution                                                                 [hot]
  type:        string — "3840x2160" or empty
  meaning:     variant resolution. Empty when we couldn't map the
               segment-directory back to a manifest entry.

variant_dir
  type:        string — "2160p" etc.
  meaning:     the canonical identifier — segment-directory name.

playlist_fetches                                                           [hot]
  type:        int
  meaning:     count of variant-playlist GETs in the observation window.

segment_fetches                                                            [hot]
  type:        int
  meaning:     count of segment GETs from this variant in the window.

first_segment_at_s / last_segment_at_s
  type:        float64 (seconds since cycle started_at)
  meaning:     time of first/last segment fetch from this variant.
               Zero when no segments fetched.

active_duration_s
  type:        float64 (seconds)
  meaning:     last_segment_at_s - first_segment_at_s. Rough "time the
               player was actively using this variant."
  gotchas:     accurate only when fetches were consecutive (no gaps).
               For abandoned-then-resumed variants this overstates.

peeked_but_never_used                                                      [hot]
  type:        bool
  meaning:     playlist_fetches > 0 AND segment_fetches == 0. Player
               evaluated the variant (fetched its playlist) but never
               committed to fetching any of its segments.
  gotchas:     this is the cheapest "did the player consider but reject
               this variant" signal. Bitrate ladders with many peeked
               variants suggest aggressive ABR exploration.

avg_mbps / peak_mbps
  type:        float64 (Mbps)
  meaning:     manifest avg+peak bandwidth for this variant (optional).
```

### 6.h `AbortCycleResult` — one entry in `Report.abort_cycles[]`

Used by the `abort` test mode. **Interpretation belongs in [[abort-characterization-test]].**

```
cycle_idx
fault_shape
  type:        int / string
  meaning:     cycle index and the abort fault shape applied
               (e.g. "server_timeout", "request_first_byte_hang").

pre_variant / pre_buffer_s / pre_bw_est_mbps                              [forensics]
  type:        string / float64 / float64
  meaning:     player state captured BEFORE the abort fired.

armed_at
  type:        time.Time
  meaning:     t=0 for the cycle — when the abort fault was armed.

abort_detected                                                            [hot] [forensics]
abort_kind / abort_at_s / abort_url                                       [forensics]
  type:        bool / string / float64 / string
  meaning:     whether the abort actually fired, what kind (fault_type/
               fault_action), when (seconds since armed_at), and which URL.

retry_found / retry_had_range / retry_range_start                         [forensics]
  type:        bool / bool / int64
  meaning:     whether the player retried, whether it used a Range
               header, and the Range start byte if so. retry_range_start
               > 0 = player resumed mid-segment vs restarting it.

player_stalled                                                            [hot] [forensics]
  type:        bool
  meaning:     position frozen for >5s post-abort.

downshifted_to / downshift_after_s                                        [forensics]
  type:        string / float64
  meaning:     if the player switched variants, what to and after how long.

recovery_s                                                                [hot] [forensics]
  type:        float64
  meaning:     seconds for the player to return to its pre-cycle
               variant + healthy buffer after the fault was released.
               0 = never recovered in the recovery window.

post_bw_est_mbps
  type:        float64
  meaning:     player's bandwidth estimate at end of cycle.
```

### 6.i `RetryCycleResult` — one entry in `Report.retry_cycles[]`

Used by the `retry-backoff` test mode. **Interpretation belongs in [[retry-backoff-characterization-test]].**

```
cycle_idx
fault_shape
pre_variant / pre_variant_dir / pre_buffer_s
armed_at / observe_window_s
  meaning:     cycle setup — same shape as AbortCycleResult.

per_url_retries                                                           [forensics]
  type:        []URLRetryInfo (see §6.j)
  meaning:     one entry per faulted URL observed in the window. Ordered
               temporally by first attempt.

faulted_urls / total_failed_fetches                                       [forensics]
mean_retry_interval_ms / median_retry_interval_ms
  type:        int / int / float64 / float64
  meaning:     aggregate counters + retry-interval distribution across
               all faulted URLs.

downshift_decided_at_s / downshift_decided_to                             [forensics]
downshift_committed_at_s / downshift_committed_to
  type:        float64 / string
  meaning:     two independent downshift signals.
                 - "decided" = sample's video_resolution flipped post-arm
                   (the player ANNOUNCED its new choice)
                 - "committed" = a new manifest URL appeared in
                   network_requests post-arm (player ACTUALLY started
                   fetching from the new variant)
  gotchas:     `decided` precedes `committed` by a few seconds typically.
               Gap > 10s = the player decided but couldn't execute on
               the decision.

gave_up_url / gave_up_at_s                                                [hot] [forensics]
  type:        string / float64
  meaning:     gave-up URL = one that stopped getting retries with a
               gap >30s before the end of the observation window.
               First URL the player abandoned, if any. Empty = no
               give-up observed.

recovery_s                                                                [hot] [forensics]
  type:        float64
  meaning:     seconds for player to return to pre-cycle variant +
               healthy buffer after fault release. 0 = never recovered
               in the recovery window.

player_stalled
  type:        bool
  meaning:     position frozen for >5s post-arm.
```

### 6.j `URLRetryInfo` — one entry in `RetryCycleResult.per_url_retries[]`

```
url
  type:        string
  meaning:     the faulted URL.

attempt_count                                                             [forensics]
  type:        int
  meaning:     total number of times the player tried this URL during
               the window.

intervals_ms
  type:        []int64
  meaning:     gaps between consecutive attempts in ms.
               len(intervals_ms) = attempt_count - 1.
  gotchas:     reveals the player's retry-backoff curve. Doubling
               intervals = exponential backoff; flat = constant; growing
               slower than doubling = jittered backoff.

first_attempt_at_s / last_attempt_at_s
  type:        float64 (seconds since cycle armed_at)
  meaning:     wall-clock window of attempts.

all_faulted
  type:        bool
  meaning:     true when EVERY attempt got faulted (i.e. fault applied
               consistently — not a flaky fault).

fault_kinds
  type:        []string
  meaning:     fault_type/fault_action values the proxy stamped on this
               URL's rows. Usually singleton.
```
