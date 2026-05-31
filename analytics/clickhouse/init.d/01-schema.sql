-- Analytics schema for InfiniteStream session events + classifier markers.
--
-- Two coupled tables:
--   - session_events:   one row per player metrics POST (was: session_snapshots).
--   - session_markers:  one row per classifier-derived event (was: session_events).
-- The forwarder dedupes events by payload fingerprint; markers by event_fingerprint.
-- Grafana, ad-hoc SQL, the harness CLI, and the session viewer all read from here.
--
-- The rename (issue #472) was a vocabulary cleanup, not a data migration —
-- existing data was preserved via RENAME TABLE. On fresh hosts these CREATEs
-- use the new names from the start.

CREATE DATABASE IF NOT EXISTS infinite_streaming;

CREATE TABLE IF NOT EXISTS infinite_streaming.session_events
(
    ts                    DateTime64(3, 'UTC')        CODEC(DoubleDelta, ZSTD(1)),
    revision              UInt64                      CODEC(DoubleDelta, ZSTD(1)),
    session_id            String                      CODEC(ZSTD(1)),
    play_id               LowCardinality(String)      CODEC(ZSTD(1)),
    -- attempt_id: player-supplied monotonically-incrementing counter
    -- per playback attempt within a play. 1 on the initial play of
    -- any content, +1 on every `restart` event (user-restart OR
    -- auto-recovery). Resets to 1 at each new play boundary. 0 means
    -- "unknown" — pre-rename rows or non-iOS clients. Use with
    -- play_id to count recovery attempts per play: max(attempt_id)
    -- GROUP BY play_id. See bug #4 fix that removed the proxy
    -- synthesis which previously made play_id rotate on every
    -- control mutation.
    attempt_id            UInt32                      DEFAULT 0 CODEC(ZSTD(1)),
    player_id             LowCardinality(String)      CODEC(ZSTD(1)),
    group_id              LowCardinality(String)      CODEC(ZSTD(1)),
    user_agent            String                      CODEC(ZSTD(1)),

    -- Content / manifest
    manifest_url          String                      CODEC(ZSTD(1)),
    manifest_variants     String                      CODEC(ZSTD(3)),
    last_request_url      String                      CODEC(ZSTD(1)),
    content_id            LowCardinality(String)      CODEC(ZSTD(1)),

    -- Player state (hot path for charts)
    player_state          LowCardinality(String)      CODEC(ZSTD(1)),
    waiting_reason        LowCardinality(String)      CODEC(ZSTD(1)),
    buffer_depth_s        Float32                     CODEC(ZSTD(1)),
    network_bitrate_mbps  Float32                     CODEC(ZSTD(1)),
    video_bitrate_mbps    Float32                     CODEC(ZSTD(1)),
    measured_mbps         Float32                     CODEC(ZSTD(1)),
    mbps_shaper_rate      Float32                     CODEC(ZSTD(1)),
    mbps_shaper_avg       Float32                     CODEC(ZSTD(1)),
    -- Server-side TCP_INFO RTT (issue #401). 100 ms ticker in
    -- go-proxy reads getsockopt(TCP_INFO) on each session's most-
    -- recent connection, folds into a 1 s window, drained on each
    -- snapshot tick. Units: ms. min_lifetime is the kernel's sticky
    -- per-connection min RTT (Linux 4.6+); rto rises during wedges
    -- while smoothed rtt flatlines, and the gap between them is
    -- the canonical "kernel suspects this connection is stalling"
    -- signal.
    client_rtt_ms              Float32                CODEC(ZSTD(1)),
    client_rtt_max_ms          Float32                CODEC(ZSTD(1)),
    client_rtt_min_ms          Float32                CODEC(ZSTD(1)),
    client_rtt_min_lifetime_ms Float32                CODEC(ZSTD(1)),
    client_rtt_var_ms          Float32                CODEC(ZSTD(1)),
    client_rto_ms              Float32                CODEC(ZSTD(1)),
    -- Out-of-band ICMP echo from go-proxy → player_ip at 1 Hz
    -- (issue #404). Path latency that's independent of the
    -- streaming TCP connection's queue contribution — the line
    -- that stays put when shaping kicks in while client_rtt_ms
    -- climbs from queueing.
    client_path_ping_rtt_ms    Float32                CODEC(ZSTD(1)),
    display_resolution    LowCardinality(String)      CODEC(ZSTD(1)),
    fetching_resolution   LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    video_resolution      LowCardinality(String)      CODEC(ZSTD(1)),
    frames_displayed      UInt64                      DEFAULT 0,
    frames_dropped        UInt32                      DEFAULT 0,
    -- DEPRECATED (#550 soft cutover). Forwarder mirror-writes from
    -- stalling_count / stalling_time_ms below for the deprecation
    -- window. Consumers (Vue3 dashboard, harness, Grafana) migrate to
    -- stalling_* over time; a follow-up PR drops these two columns.
    stall_count           UInt32                      DEFAULT 0,
    stall_time_s          Float32                     CODEC(ZSTD(1)),

    -- ── State residency accumulators (#550 Phase 1) ────────────────────
    -- Cumulative-on-the-wire since play start; reset at every play
    -- boundary by the iOS client. Server derives per-snapshot deltas
    -- via lag() — and ALSO stores them as paired _delta columns below
    -- (forwarder-computed at insert time from per-play state cache).
    --
    -- Use _delta for "did this snapshot witness X" / "live tile";
    -- _time_ms for "total since play start"; _count for "entries";
    -- *_duration_ms (further down) for "most-recent event duration".
    --
    -- stalling_* replaces both the old AVPlayerItemPlaybackStalled-
    -- driven stall_count/stall_time_s AND the briefly-shipped
    -- stalled_state_* mirror — single canonical pair, driven by
    -- state transitions in PlaybackDiagnostics.
    --
    -- Trickplay = playback rate ∉ {~0, ~1}. Seeking = time between
    -- AVPlayerItemTimeJumped and next .playing transition; Conviva
    -- CIRR/CIRT pattern lets dashboard derive connection-induced
    -- buffer via `buffering_time_ms - seeking_time_ms`.
    playing_time_ms             UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    playing_time_ms_delta       UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    playing_count               UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    playing_count_delta         UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    pausing_time_ms             UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    pausing_time_ms_delta       UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    pausing_count               UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    pausing_count_delta         UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    buffering_time_ms           UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    buffering_time_ms_delta     UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    buffering_count             UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    buffering_count_delta       UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    stalling_time_ms            UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    stalling_time_ms_delta      UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    stalling_count              UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    stalling_count_delta        UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    idling_time_ms              UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    idling_time_ms_delta        UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    idling_count                UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    idling_count_delta          UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    seeking_time_ms             UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    seeking_time_ms_delta       UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    seeking_count               UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    seeking_count_delta         UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    trickplaying_time_ms        UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    trickplaying_time_ms_delta  UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    trickplaying_count          UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    trickplaying_count_delta    UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),

    -- ── Phase 1 per-event sticky durations (#550) ──────────────────
    -- Duration of the most-recent stall / buffer event. Set by iOS
    -- when the event completes (last_event='stall_end' /
    -- 'buffering_end'), sticky on subsequent heartbeats until next
    -- event. Use these for "longest single event in play" queries;
    -- the _delta columns above answer "this heartbeat saw X".
    stall_duration_ms           UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),
    buffering_duration_ms       UInt32 DEFAULT 0          CODEC(DoubleDelta, ZSTD(1)),

    -- JSON-string-encoded variant-label → cumulative seconds map.
    -- iOS emits as `{"2160p@29857kbps":65.28,"1080p@7060kbps":12.4}`.
    -- Preserved across retry()-style restarts so dwell totals remain
    -- continuous through auto-recovery. ZSTD compresses the
    -- repeating-shape strings to near-nothing in the column store.
    time_per_variant_s          String DEFAULT ''         CODEC(ZSTD(3)),

    -- Orthogonal "this stall won't auto-recover" flag. Set by iOS
    -- when AVPlayer transitions from .waitingToPlayAtSpecifiedRate
    -- to .paused mid-stall (give-up). The state lane stays "stalled"
    -- for residency continuity; this flag is the discriminator the
    -- dashboard renders for operator-actionable stalls. Cleared on
    -- next .playing transition.
    stall_stuck                 Bool   DEFAULT false      CODEC(ZSTD(1)),

    -- ── Phase 2 outcome status + error fields (#550) ───────────────
    -- playback_status: terminal outcome (completed / user_stopped /
    --   start_failure (VSF) / abandoned_start (EBVS) /
    --   mid_stream_failure (MSF) / in_progress).
    -- playback_reason: controlled vocab per status. During
    --   in_progress, mirrors player_state for free; on terminal rows,
    --   classifier-derived from iOS raw signals + qoe_thresholds.json.
    -- error_code/_domain/_details: per-snapshot error context;
    --   populated on `last_event='error'` rows AND on terminal failure
    --   session_end rows (same value as terminal_error_* on those).
    -- terminal_error_*: populated ONLY on terminal failure rows.
    --   Querying terminal_error_code != 0 never returns transient codes.
    -- error_count / _delta: cumulative observation counter + per-row
    --   delta. Match the Phase 1 _delta pattern.
    playback_status         LowCardinality(String) DEFAULT 'in_progress' CODEC(ZSTD(1)),
    playback_reason         LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    error_code              Int32                  DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),
    error_domain            LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    error_details           String                                       CODEC(ZSTD(3)),
    terminal_error_code     Int32                  DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),
    terminal_error_domain   LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    terminal_error_details  String                                       CODEC(ZSTD(3)),
    error_count             UInt32                 DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),
    error_count_delta       UInt32                 DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),

    -- ── Phase 4 device / platform / version taxonomy (#550) ────────
    -- Split the conflated `platform` field into the canonical Conviva
    -- / Bitmovin taxonomy. Stamped on every row (Mux/Conviva pattern);
    -- LowCardinality compresses the repeats. iOS app emits via
    -- DeviceInfo.swift; external HLS players (no metrics protocol)
    -- get device fields parsed best-effort from User-Agent by
    -- go-proxy.
    os_version_major        UInt16                 DEFAULT 0             CODEC(ZSTD(1)),
    os_version_minor        UInt16                 DEFAULT 0             CODEC(ZSTD(1)),
    app_version             LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    device_class            LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    device_model            String                 DEFAULT ''            CODEC(ZSTD(1)),
    player_tech             LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    -- Orientation-aware physical-pixel resolution, formatted "WxH" to
    -- match video_resolution / display_resolution. Supersedes
    -- screen_width_px / screen_height_px / screen_density (dropped
    -- 2026-05-30 — see ALTER block at end of file).
    device_resolution       LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),

    position_s            Float32                     CODEC(ZSTD(1)),
    live_edge_s           Float32                     CODEC(ZSTD(1)),
    true_offset_s         Float32                     CODEC(ZSTD(1)),
    playback_rate         Float32                     CODEC(ZSTD(1)),
    loop_count_player     UInt32                      DEFAULT 0,
    loop_count_delta  UInt32                      DEFAULT 0,
    state_from            LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    state_to              LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    content_name          LowCardinality(String) DEFAULT '' CODEC(ZSTD(1)),
    user_marked_at        String                 DEFAULT '' CODEC(ZSTD(1)),
    loop_count_server     UInt32                      DEFAULT 0,
    player_restarts       UInt32                      DEFAULT 0,
    profile_shift_count   UInt32                      DEFAULT 0,
    -- Effective rate cap the kernel is enforcing right now. Resolves
    -- in priority order:
    --   1. Pattern step runtime (nftables_pattern_rate_runtime_mbps)
    --      when a pattern is enabled and running.
    --   2. Operator slider (nftables_bandwidth_mbps) when set (>0).
    --   3. Deployment baseline (INFINITE_STREAM_DEFAULT_RATE_MBPS).
    -- 0 means truly uncapped (all three sources at 0). Distinct from
    -- nftables_bandwidth_mbps which stores operator slider intent only.
    -- Stamped by the proxy on every snapshot. Issue #480, pattern
    -- fold-in added in follow-up.
    effective_rate_limit_mbps Float32                 CODEC(ZSTD(1)),

    -- Player events (discrete signals embedded in the heartbeat snapshot;
    -- testing.html derives event-lane points from transitions in these).
    last_event            LowCardinality(String)      CODEC(ZSTD(1)),
    trigger_type          LowCardinality(String)      CODEC(ZSTD(1)),
    event_time            String                      CODEC(ZSTD(1)),
    player_error          String                      CODEC(ZSTD(1)),

    -- Player metrics (extended)
    avg_network_bitrate_mbps     Float32 CODEC(ZSTD(1)),
    buffer_end_s                 Float32 CODEC(ZSTD(1)),
    live_offset_s                Float32 CODEC(ZSTD(1)),
    playhead_wallclock_ms        Int64   CODEC(ZSTD(1)),
    seekable_end_s               Float32 CODEC(ZSTD(1)),
    metrics_source               LowCardinality(String) CODEC(ZSTD(1)),
    video_first_frame_time_s     Float32 CODEC(ZSTD(1)),
    video_quality_pct            Float32 CODEC(ZSTD(1)),
    video_quality_60s_pct        Float32 CODEC(ZSTD(1)),
    video_quality_avg_pct        Float32 CODEC(ZSTD(1)),
    video_start_time_s           Float32 CODEC(ZSTD(1)),

    -- Network / transfer
    mbps_transfer_complete       Float32 CODEC(ZSTD(1)),
    mbps_transfer_rate           Float32 CODEC(ZSTD(1)),
    player_ip                    LowCardinality(String) CODEC(ZSTD(1)),
    server_received_at_ms        Int64   CODEC(ZSTD(1)),
    x_forwarded_port             UInt16  DEFAULT 0,
    x_forwarded_port_external    UInt16  DEFAULT 0,

    -- Master manifest failure injection
    master_manifest_url               String                 CODEC(ZSTD(1)),
    master_manifest_failure_type      LowCardinality(String) CODEC(ZSTD(1)),
    master_manifest_failure_mode      LowCardinality(String) CODEC(ZSTD(1)),
    master_manifest_failure_frequency Float32                CODEC(ZSTD(1)),
    master_manifest_consecutive_failures UInt32              DEFAULT 0,
    master_manifest_requests_count    UInt32                 DEFAULT 0,

    -- Manifest failure injection (extended)
    manifest_failure_frequency      Float32 CODEC(ZSTD(1)),
    manifest_failure_urls           String  CODEC(ZSTD(3)),
    manifest_consecutive_failures   UInt32  DEFAULT 0,
    manifest_requests_count         UInt32  DEFAULT 0,

    -- Segment failure injection (extended)
    segment_failure_frequency       Float32 CODEC(ZSTD(1)),
    segment_failure_urls            String  CODEC(ZSTD(3)),
    segment_consecutive_failures    UInt32  DEFAULT 0,
    segments_count                  UInt32  DEFAULT 0,

    -- "All" (cross-cutting) failure injection
    all_failure_type                LowCardinality(String) CODEC(ZSTD(1)),
    all_failure_mode                LowCardinality(String) CODEC(ZSTD(1)),
    all_failure_frequency           Float32                CODEC(ZSTD(1)),
    all_failure_urls                String                 CODEC(ZSTD(3)),
    all_consecutive_failures        UInt32                 DEFAULT 0,

    -- Transport failure / fault details
    transport_failure_frequency     Float32                CODEC(ZSTD(1)),
    transport_failure_mode          LowCardinality(String) CODEC(ZSTD(1)),
    transport_failure_units         LowCardinality(String) CODEC(ZSTD(1)),
    transport_consecutive_failures  UInt32                 DEFAULT 0,
    transport_consecutive_seconds   Float32                CODEC(ZSTD(1)),
    transport_consecutive_units     UInt32                 DEFAULT 0,
    transport_frequency_seconds     Float32                CODEC(ZSTD(1)),
    transport_fault_drop_packets    UInt8                  DEFAULT 0,
    transport_fault_reject_packets  UInt8                  DEFAULT 0,
    transport_fault_off_seconds     Float32                CODEC(ZSTD(1)),
    transport_fault_on_seconds      Float32                CODEC(ZSTD(1)),
    transport_fault_type            LowCardinality(String) CODEC(ZSTD(1)),
    fault_count_transfer_active_timeout  UInt32            DEFAULT 0,
    fault_count_transfer_idle_timeout    UInt32            DEFAULT 0,

    -- Transfer timeouts
    transfer_active_timeout_seconds   Float32 CODEC(ZSTD(1)),
    transfer_idle_timeout_seconds     Float32 CODEC(ZSTD(1)),
    transfer_timeout_applies_manifests UInt8  DEFAULT 0,
    transfer_timeout_applies_master    UInt8  DEFAULT 0,
    transfer_timeout_applies_segments  UInt8  DEFAULT 0,

    -- nftables (extended)
    nftables_pattern_step               UInt32  DEFAULT 0,
    nftables_pattern_step_runtime       UInt32  DEFAULT 0,
    nftables_pattern_steps              String  CODEC(ZSTD(3)),
    nftables_pattern_rate_runtime_mbps  Float32 CODEC(ZSTD(1)),
    nftables_pattern_margin_pct         Float32 CODEC(ZSTD(1)),
    nftables_pattern_template_mode      LowCardinality(String) CODEC(ZSTD(1)),

    -- Content config
    content_allowed_variants    String                 CODEC(ZSTD(3)),
    content_live_offset         Float32                CODEC(ZSTD(1)),
    content_strip_codecs        String                 CODEC(ZSTD(1)),

    -- Misc
    abrchar_run_lock            UInt8                  DEFAULT 0,
    -- control_revision is go-proxy's RFC3339Nano "ETag" for
    -- optimistic concurrency on session mutations. Originally stored
    -- as UInt64 — silently truncated by getU64(s, "control_revision")
    -- via fmt.Sscanf("%d", ...) to just the leading year (e.g. "2026"
    -- from "2026-05-29T17:06:29Z"). Type-changed-in-place: dropped
    -- the broken UInt64 + renamed the working String column back to
    -- the canonical name. See migration in the same PR.
    control_revision            String                 DEFAULT ''            CODEC(ZSTD(1)),

    -- Server-side variant
    server_video_rendition       LowCardinality(String) CODEC(ZSTD(1)),
    server_video_rendition_mbps  Float32                CODEC(ZSTD(1)),

    -- Failure injection (categorical hot fields)
    manifest_failure_type    LowCardinality(String)   CODEC(ZSTD(1)),
    manifest_failure_mode    LowCardinality(String)   CODEC(ZSTD(1)),
    segment_failure_type     LowCardinality(String)   CODEC(ZSTD(1)),
    segment_failure_mode     LowCardinality(String)   CODEC(ZSTD(1)),
    transport_failure_type   LowCardinality(String)   CODEC(ZSTD(1)),
    transport_fault_active   UInt8                    DEFAULT 0,

    -- Network shaping
    nftables_bandwidth_mbps  Float32                  CODEC(ZSTD(1)),
    nftables_delay_ms        UInt32                   DEFAULT 0,
    nftables_packet_loss     Float32                  CODEC(ZSTD(1)),
    nftables_pattern_enabled UInt8                    DEFAULT 0,

    -- Lifecycle
    first_request_time   String                       CODEC(ZSTD(1)),
    last_request         String                       CODEC(ZSTD(1)),
    session_duration     Float32                      CODEC(ZSTD(1)),

    -- Long tail: full session map as JSON, for fields not promoted to columns.
    session_json         String                       CODEC(ZSTD(3)),

    -- Tiered retention classification (issue #342). One of:
    --   'other'        — default, evicted at 30 d (TTL clause below).
    --   'interesting'  — auto-classified at session-end when any bad-event
    --                    signal appears in the session. Evicted at 90 d.
    --   'favourite'    — explicitly starred by the user. Never evicted.
    -- Set by the forwarder via ALTER UPDATE on session-end + on star /
    -- unstar API calls. Must be declared inline (not via post-create
    -- ALTER) because the TTL clause below references it. The redundant
    -- ADD COLUMN IF NOT EXISTS further down is a no-op for fresh hosts
    -- but keeps the upgrade path alive for hosts created before #342.
    classification       LowCardinality(String) DEFAULT 'other' CODEC(ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
-- ORDER BY player_id first because the dashboard queries are
-- always `WHERE player_id = X AND ts BETWEEN ?` (one EventSource
-- per device per page); leading on player_id gives one primary-
-- key range scan per query instead of a bloom-filter granule
-- prune across N session_id chunks. session_id stays bloom-
-- indexed below for the rare replay-by-session case.
ORDER BY (player_id, ts)
-- Tiered retention by classification column (issue #342):
--   * 'other'       → 30 d (default)
--   * 'interesting' → 90 d
--   * 'favourite'   → no clause matches → kept forever
-- Per-row TTL is computed against the row's own ts. For long sessions
-- the front of the session may evict before the end (#347 tracks the
-- session_end_ts polish that would make eviction strictly all-or-
-- nothing). For typical hour-scale sessions the trim window is too
-- small to care about.
TTL toDateTime(ts) + INTERVAL 30 DAY DELETE WHERE classification = 'other',
    toDateTime(ts) + INTERVAL 90 DAY DELETE WHERE classification = 'interesting'
SETTINGS index_granularity = 8192;

-- Bloom filter on session_id for fast point lookups in replay mode.
ALTER TABLE infinite_streaming.session_events
    ADD INDEX IF NOT EXISTS idx_session_id session_id TYPE bloom_filter(0.01) GRANULARITY 4;

ALTER TABLE infinite_streaming.session_events
    ADD INDEX IF NOT EXISTS idx_player_id player_id TYPE bloom_filter(0.01) GRANULARITY 4;

ALTER TABLE infinite_streaming.session_events
    ADD INDEX IF NOT EXISTS idx_play_id play_id TYPE bloom_filter(0.01) GRANULARITY 4;

-- Bring older deployments up to date if the column predates this column.
ALTER TABLE infinite_streaming.session_events
    ADD COLUMN IF NOT EXISTS play_id LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS attempt_id UInt32 DEFAULT 0 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS content_id LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS last_event LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS trigger_type LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS event_time String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS player_error String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS avg_network_bitrate_mbps Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffer_end_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS live_offset_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS playhead_wallclock_ms Int64 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS player_restarts UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS profile_shift_count UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS effective_rate_limit_mbps Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS seekable_end_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS metrics_source LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_first_frame_time_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_quality_pct Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_quality_60s_pct Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_quality_avg_pct Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_start_time_s Float32 CODEC(ZSTD(1)),
    -- iOS per-variant watch time (issue #486). JSON-string keyed by
    -- `<resolution>@<kbps>kbps` with seconds-watched values. Stored
    -- verbatim; PlayLog expands it into one chip per variant via its
    -- generic JSON-field expander.
    ADD COLUMN IF NOT EXISTS time_per_variant_s String CODEC(ZSTD(3)),

    -- ── #550 Phase 1: residency accumulators + paired deltas ───────
    -- New canonical names (gerund, UInt32 ms, DoubleDelta+ZSTD).
    -- Supersedes the first-cut Float32 _s variants from the prior
    -- residency commit on this branch (those ALTERs are removed
    -- because no production deploy ever consumed them).
    ADD COLUMN IF NOT EXISTS playing_time_ms             UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS playing_time_ms_delta       UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS playing_count               UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS playing_count_delta         UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS pausing_time_ms             UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS pausing_time_ms_delta       UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS pausing_count               UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS pausing_count_delta         UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffering_time_ms           UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffering_time_ms_delta     UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffering_count             UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffering_count_delta       UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS stalling_time_ms            UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS stalling_time_ms_delta      UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS stalling_count              UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS stalling_count_delta        UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS idling_time_ms              UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS idling_time_ms_delta        UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS idling_count                UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS idling_count_delta          UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS seeking_time_ms             UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS seeking_time_ms_delta       UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS seeking_count               UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS seeking_count_delta         UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS trickplaying_time_ms        UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS trickplaying_time_ms_delta  UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS trickplaying_count          UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS trickplaying_count_delta    UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    -- Per-event sticky durations (Phase 1 ancillary).
    ADD COLUMN IF NOT EXISTS stall_duration_ms           UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffering_duration_ms       UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    -- "this stall won't auto-recover" discriminator (#550 Phase 1 ext.)
    ADD COLUMN IF NOT EXISTS stall_stuck                 Bool   DEFAULT false CODEC(ZSTD(1)),
    -- Per-variant dwell map (JSON string).
    ADD COLUMN IF NOT EXISTS time_per_variant_s          String DEFAULT ''    CODEC(ZSTD(3)),
    -- video_first_frame_time_ms / video_start_time_ms (Phase 1 ms migrations).
    -- Keep the legacy _s variants above as deprecated; forwarder
    -- mirror-writes both for the deprecation window.
    ADD COLUMN IF NOT EXISTS video_first_frame_time_ms   UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_start_time_ms         UInt32 DEFAULT 0 CODEC(DoubleDelta, ZSTD(1)),

    -- ── #550 Phase 2: outcome status + structured error fields ─────
    ADD COLUMN IF NOT EXISTS playback_status         LowCardinality(String) DEFAULT 'in_progress' CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS playback_reason         LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS error_code              Int32                  DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS error_domain            LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS error_details           String                                       CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS terminal_error_code     Int32                  DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS terminal_error_domain   LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS terminal_error_details  String                                       CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS error_count             UInt32                 DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),
    ADD COLUMN IF NOT EXISTS error_count_delta       UInt32                 DEFAULT 0             CODEC(DoubleDelta, ZSTD(1)),

    -- ── #550 Phase 4: device / platform / version taxonomy ─────────
    ADD COLUMN IF NOT EXISTS os_version_major        UInt16                 DEFAULT 0             CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS os_version_minor        UInt16                 DEFAULT 0             CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS app_version             LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS device_class            LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS device_model            String                 DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS player_tech             LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    -- device_resolution supersedes screen_width_px / screen_height_px /
    -- screen_density (2026-05-30 cleanup).
    ADD COLUMN IF NOT EXISTS device_resolution       LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS loop_count_delta    UInt32                 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS state_from              LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS state_to                LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS content_name            LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS user_marked_at          String                 DEFAULT ''            CODEC(ZSTD(1)),
    DROP COLUMN IF EXISTS screen_width_px,
    DROP COLUMN IF EXISTS screen_height_px,
    DROP COLUMN IF EXISTS screen_density,

    -- #550 dashboard-parity restoration: origination_ip was on the
    -- live PlayerRecord but never made it into CH (no schema column,
    -- no forwarder ingest, no bundle). Restored here so archived
    -- session viewer's "Origination IP" tile populates the same way
    -- the live Testing dashboard's does.
    ADD COLUMN IF NOT EXISTS origination_ip          LowCardinality(String) DEFAULT ''            CODEC(ZSTD(1)),
    -- session_number is the proxy's short numeric session id
    -- (port-derived). Surfaced as display_id in v2 + the dashboard's
    -- "Display ID" tile. Was missing from CH so archived rows showed
    -- "—". Same parity-restoration pattern as origination_ip above.
    ADD COLUMN IF NOT EXISTS session_number          UInt32                 DEFAULT 0             CODEC(ZSTD(1)),
    -- control_revision: the working String column. Type-changed in-
    -- place from UInt64 (truncated by Sscanf) via DROP + RENAME in
    -- the same migration block. See top-of-table CREATE definition
    -- for the canonical type. ALTER on existing deploys is the
    -- two-statement sequence at the bottom of this file.
    -- Manifest HOLD-BACK / PART-HOLD-BACK (issue #486 follow-up).
    -- recommended_offset_s = AVFoundation's parse of
    -- EXT-X-SERVER-CONTROL — what Apple says the player should sit
    -- back from the live edge. configured_offset_s = what the app
    -- currently has set. live_offset_s + recommended_offset_s gives
    -- the "true offset to end of playlist" stable across stalls.
    ADD COLUMN IF NOT EXISTS recommended_offset_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS configured_offset_s Float32 CODEC(ZSTD(1)),
    -- Active variant's nominal frame rate (issue #486 follow-up).
    -- Used by the dashboard to display the effective vs nominal FPS
    -- ratio. Sourced from AVAssetVariant.videoAttributes.
    ADD COLUMN IF NOT EXISTS frames_rate Float32 CODEC(ZSTD(1)),
    -- Median TTFB (responseStart - requestEnd) over the most recent
    -- AVMetricMediaResourceRequestEvent.networkTransactionMetrics
    -- samples. Rendered as "TTFB (client, ms)" on the RTT chart.
    -- NOT a wire-time RTT — on HTTP/2 keep-alive this is stream-
    -- level latency from URLSession's pipeline view, typically far
    -- below the server-side TCP_INFO `client_rtt_ms`. The gap
    -- between this and `client_rtt_ms` is itself a diagnostic
    -- signal (proxy buffering / stream queueing). Issue #486.
    ADD COLUMN IF NOT EXISTS client_rtt_avmetrics_ms Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS mbps_transfer_complete Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS mbps_transfer_rate Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS player_ip LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS server_received_at_ms Int64 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS x_forwarded_port UInt16 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS x_forwarded_port_external UInt16 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS master_manifest_url String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS master_manifest_failure_type LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS master_manifest_failure_mode LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS master_manifest_failure_frequency Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS master_manifest_consecutive_failures UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS master_manifest_requests_count UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS manifest_failure_frequency Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS manifest_failure_urls String CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS manifest_consecutive_failures UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS manifest_requests_count UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS segment_failure_frequency Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS segment_failure_urls String CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS segment_consecutive_failures UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS segments_count UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS all_failure_type LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS all_failure_mode LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS all_failure_frequency Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS all_failure_urls String CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS all_consecutive_failures UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transport_failure_frequency Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_failure_mode LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_failure_units LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_consecutive_failures UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transport_consecutive_seconds Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_consecutive_units UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transport_frequency_seconds Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_fault_drop_packets UInt8 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transport_fault_reject_packets UInt8 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transport_fault_off_seconds Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_fault_on_seconds Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transport_fault_type LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS fault_count_transfer_active_timeout UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS fault_count_transfer_idle_timeout UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transfer_active_timeout_seconds Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transfer_idle_timeout_seconds Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS transfer_timeout_applies_manifests UInt8 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transfer_timeout_applies_master UInt8 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transfer_timeout_applies_segments UInt8 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS nftables_pattern_step UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS nftables_pattern_step_runtime UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS nftables_pattern_steps String CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS nftables_pattern_rate_runtime_mbps Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS nftables_pattern_margin_pct Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS nftables_pattern_template_mode LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS content_allowed_variants String CODEC(ZSTD(3)),
    ADD COLUMN IF NOT EXISTS content_live_offset Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS content_strip_codecs String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS abrchar_run_lock UInt8 DEFAULT 0,
    -- control_revision was originally added here as UInt64 — wrong
    -- type (see CREATE TABLE comment). Type-fix migration below at
    -- the end of this file: DROP UInt64 + RENAME _str → canonical
    -- name. Line kept as a comment so legacy clusters with the
    -- UInt64 column still parse this file cleanly during init.d
    -- replay (ADD COLUMN IF NOT EXISTS is a no-op when present).
    -- Server-side TCP_INFO RTT (issue #401). See inline comment on
    -- the CREATE TABLE columns above.
    ADD COLUMN IF NOT EXISTS client_rtt_ms Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS client_rtt_max_ms Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS client_rtt_min_ms Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS client_rtt_min_lifetime_ms Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS client_rtt_var_ms Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS client_rto_ms Float32 CODEC(ZSTD(1)),
    -- Out-of-band ICMP path-ping (issue #404). See inline comment
    -- on the CREATE TABLE column above.
    ADD COLUMN IF NOT EXISTS client_path_ping_rtt_ms Float32 CODEC(ZSTD(1)),
    -- Tiered retention classification (issue #342). One of:
    --   'other'        — default, evicted at 30 d (TTL clause below).
    --   'interesting'  — auto-classified at session-end when any of
    --                    user_marked / frozen / segment_stall / restart /
    --                    error / non-empty player_error / fault counters
    --                    appear in the session. Evicted at 90 d.
    --   'favourite'    — explicitly starred by the user. Never evicted.
    -- Set by the forwarder via ALTER UPDATE on session-end + on star /
    -- unstar API calls. Lives on every row of (session_id, play_id) so
    -- ClickHouse TTL can evaluate it without joining a side table.
    ADD COLUMN IF NOT EXISTS classification LowCardinality(String) DEFAULT 'other' CODEC(ZSTD(1));

-- Convert legacy manifest_variants UInt16 (broken: always stored 0 because
-- the SSE field is an array, not a number) to String (JSON of the variant
-- ladder). Safe MODIFY since the only existing values are zero.
ALTER TABLE infinite_streaming.session_events
    MODIFY COLUMN IF EXISTS manifest_variants String CODEC(ZSTD(3));

-- control_revision: type-change-in-place from UInt64 → String.
-- The UInt64 was filled by getU64(s, "control_revision") which calls
-- fmt.Sscanf("%d", ...) — that reads leading digits and stops at the
-- first non-digit, truncating go-proxy's RFC3339Nano timestamps to
-- just the year (e.g. "2026" from "2026-05-29T17:06:29.776714594Z").
--
-- Migration sequence (atomic via two ALTERs run in immediate
-- succession; CH supports multi-action ALTER but the soft-cutover
-- column control_revision_str must exist before the DROP):
--   1. ADD COLUMN control_revision_str String   (already done above
--      in the additive ALTER block as a soft cutover)
--   2. DROP COLUMN control_revision (the broken UInt64)
--   3. RENAME COLUMN control_revision_str → control_revision
--
-- On fresh deploys the CREATE TABLE has the canonical String column,
-- so step 2's IF EXISTS is a no-op (nothing to drop) and step 3 is
-- a no-op (nothing to rename — control_revision_str was never added
-- because step 1 above is IF NOT EXISTS).
ALTER TABLE infinite_streaming.session_events
    DROP COLUMN IF EXISTS control_revision;

ALTER TABLE infinite_streaming.session_events
    RENAME COLUMN IF EXISTS control_revision_str TO control_revision;

-- Per-request HAR-style log so the session-viewer's network log fold
-- can replay archived sessions whose go-proxy buffer is gone. Forwarder
-- polls /api/session/<id>/network for live sessions and dedupes rows
-- via entry_fingerprint. Headers/query are stored as JSON strings —
-- almost never queried column-by-column and often empty.
CREATE TABLE IF NOT EXISTS infinite_streaming.network_requests
(
    ts                       DateTime64(3, 'UTC')   CODEC(Delta, ZSTD(1)),
    session_id               String                 CODEC(ZSTD(1)),
    -- player_id (canonical v2 UUID) on every HAR row so dashboard
    -- queries can filter by the same identifier as snapshots. Inline
    -- here (vs the historical post-hoc ALTER below) because it now
    -- participates in the primary ORDER BY tuple — ALTER-added
    -- columns can't be in the sort key on first init.
    player_id                LowCardinality(String) CODEC(ZSTD(1)),
    play_id                  LowCardinality(String) CODEC(ZSTD(1)),
    -- attempt_id mirrors session_events — see comment there.
    -- Stamped onto every HAR row from the session's sticky attempt_id.
    attempt_id               UInt32                 DEFAULT 0 CODEC(ZSTD(1)),
    method                   LowCardinality(String) CODEC(ZSTD(1)),
    url                      String                 CODEC(ZSTD(3)),
    upstream_url             String                 CODEC(ZSTD(3)),
    path                     String                 CODEC(ZSTD(3)),
    request_kind             LowCardinality(String) CODEC(ZSTD(1)),
    status                   UInt16                 DEFAULT 0,
    bytes_in                 Int64                  DEFAULT 0,
    bytes_out                Int64                  DEFAULT 0,
    content_type             LowCardinality(String) CODEC(ZSTD(1)),
    request_range            String                 CODEC(ZSTD(1)),
    response_content_range   String                 CODEC(ZSTD(1)),
    dns_ms                   Float32                CODEC(ZSTD(1)),
    connect_ms               Float32                CODEC(ZSTD(1)),
    tls_ms                   Float32                CODEC(ZSTD(1)),
    ttfb_ms                  Float32                CODEC(ZSTD(1)),
    transfer_ms              Float32                CODEC(ZSTD(1)),
    total_ms                 Float32                CODEC(ZSTD(1)),
    client_wait_ms           Float32                CODEC(ZSTD(1)),
    faulted                  UInt8                  DEFAULT 0,
    fault_type               LowCardinality(String) CODEC(ZSTD(1)),
    fault_action             LowCardinality(String) CODEC(ZSTD(1)),
    fault_category           LowCardinality(String) CODEC(ZSTD(1)),
    request_headers          String                 CODEC(ZSTD(3)),
    response_headers         String                 CODEC(ZSTD(3)),
    query_string             String                 CODEC(ZSTD(3)),
    -- Fingerprint over the immutable identity (ts ms, path, method,
    -- status, play_id) so re-polling go-proxy doesn't double-insert.
    entry_fingerprint        UInt64                 CODEC(ZSTD(1)),
    -- Tiered retention classification (issue #342) — must be inline
    -- because the TTL clause below references it. See full comment on
    -- session_events. The post-create ALTER ADD COLUMN IF NOT EXISTS
    -- below is a no-op on fresh hosts but covers the upgrade path.
    classification           LowCardinality(String) DEFAULT 'other' CODEC(ZSTD(1)),
    INDEX idx_play_id play_id TYPE bloom_filter GRANULARITY 4,
    INDEX idx_status status TYPE minmax GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
-- ORDER BY player_id first for the same reason as session_events:
-- the dashboard's network log filter is always WHERE player_id = X
-- AND ts BETWEEN ?. entry_fingerprint stays as the dedupe component
-- of the sort tuple so per-request UPSERT semantics still work.
ORDER BY (player_id, ts, entry_fingerprint)
-- Tiered retention by classification column — see comment on
-- session_events above. Both tables share the same retention
-- policy so paired (snapshots, network) data ages out together.
TTL toDateTime(ts) + INTERVAL 30 DAY DELETE WHERE classification = 'other',
    toDateTime(ts) + INTERVAL 90 DAY DELETE WHERE classification = 'interesting'
SETTINGS index_granularity = 8192;

-- Same classification column on the per-request table so HAR rows
-- track their session's retention tier. Defaults to 'other' on insert;
-- forwarder bumps it on session-end / star / unstar via ALTER UPDATE.
ALTER TABLE infinite_streaming.network_requests
    ADD COLUMN IF NOT EXISTS classification LowCardinality(String) DEFAULT 'other' CODEC(ZSTD(1));

-- player_id (canonical v2 UUID) on every HAR row so the v2
-- /api/v2/network_requests endpoint can filter by the same identifier
-- the dashboard uses everywhere else (live SSE, snapshots, groups).
-- Forwarder builds a sessionID→playerID map from incoming session
-- snapshots and stamps player_id at insert time. Old rows keep the
-- empty default — they remain queryable by session_id only.
ALTER TABLE infinite_streaming.network_requests
    ADD COLUMN IF NOT EXISTS player_id LowCardinality(String) CODEC(ZSTD(1));

-- attempt_id on HAR rows mirrors session_events — stamped from the
-- session's sticky attempt_id at insert. Old rows keep the 0 default.
ALTER TABLE infinite_streaming.network_requests
    ADD COLUMN IF NOT EXISTS attempt_id UInt32 DEFAULT 0 CODEC(ZSTD(1));

-- session_markers retired in issue #474 Milestone C — replaced by
-- per-row `labels[]` on session_events / network_requests and by
-- discrete rows on control_events. The CREATE TABLE block previously
-- here was removed; existing rows on running clusters age out via TTL
-- once forwarder writes stop. New deployments never create the table.

-- Issue #474 Milestone B — control_events.
--
-- Sibling of session_events / network_requests for server-side and
-- operator-driven actions: fault toggles, pattern step advances,
-- shaper changes, harness mutations (fault rule edits, label edits,
-- session lifecycle, content swap). One row per discrete action.
--
-- `source` distinguishes who caused it:
--   'harness' — operator-initiated via dashboard / harness CLI
--   'proxy'   — runtime auto-transition (fault loop, pattern step,
--               loop_server detection, proxy-detected session_end)
--   'auto'    — automated test runner (placeholder; no emit path yet)
--
-- `event` is the closed-set action vocabulary — see Milestone B body
-- in issue #474. `info` is an optional JSON blob with extras (the
-- changed field for control_change, step/rate/duration for
-- pattern_step, etc.).
--
-- `labels[]` follows the same `<severity>=<event>` / `<severity>=*<event>`
-- convention as the other two tables so the dashboard's severity
-- filter sweeps all three uniformly.
CREATE TABLE IF NOT EXISTS infinite_streaming.control_events
(
    ts                       DateTime64(3, 'UTC')   CODEC(Delta, ZSTD(1)),
    player_id                LowCardinality(String) CODEC(ZSTD(1)),
    play_id                  LowCardinality(String) CODEC(ZSTD(1)),
    attempt_id               UInt32                 DEFAULT 0 CODEC(ZSTD(1)),
    session_id               String                 CODEC(ZSTD(1)),
    source                   LowCardinality(String) CODEC(ZSTD(1)),
    event                    LowCardinality(String) CODEC(ZSTD(1)),
    info                     String                 CODEC(ZSTD(3)),
    labels                   Array(LowCardinality(String)) DEFAULT [] CODEC(ZSTD(1)),
    event_fingerprint        UInt64                 CODEC(ZSTD(1)),
    classification           LowCardinality(String) DEFAULT 'other' CODEC(ZSTD(1)),
    INDEX idx_play_id play_id TYPE bloom_filter GRANULARITY 4,
    INDEX idx_event   event   TYPE bloom_filter GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (player_id, ts, event_fingerprint)
TTL toDateTime(ts) + INTERVAL 30 DAY DELETE WHERE classification = 'other',
    toDateTime(ts) + INTERVAL 90 DAY DELETE WHERE classification = 'interesting'
SETTINGS index_granularity = 8192;

-- ── ios_avmetric_events ───────────────────────────────────────────────────
-- iOS 18 AVMetrics raw event stream (issue #486 spike).
--
-- One row per AVMetric event the iOS player publishes via
-- POST /api/session/{id}/avmetrics. Parallel to session_events; the spike
-- intentionally keeps this stream separate so we can compare AVMetrics vs
-- our heartbeat-derived metrics side-by-side without polluting either schema.
--
-- `event_type` is the AVMetric subclass name as published by AVFoundation —
-- e.g. AVMetricPlayerItemLikelyToKeepUpEvent, AVMetricPlayerItemStallEvent,
-- AVMetricPlayerItemVariantSwitchEvent, AVMetricPlayerItemPlaybackSummaryEvent.
-- `raw_json` is the unmodified event payload from the SDK so we can iterate
-- the projection without iOS code changes.
--
-- Same TTL policy as session_events / control_events: 30 d / 90 d / forever
-- by classification. classification is bumped by the forwarder on
-- session-end (interesting) and on user star (favourite).
CREATE TABLE IF NOT EXISTS infinite_streaming.ios_avmetric_events
(
    ts                DateTime64(3, 'UTC')          CODEC(Delta, ZSTD(1)),
    player_id         LowCardinality(String)        CODEC(ZSTD(1)),
    play_id           LowCardinality(String)        CODEC(ZSTD(1)),
    attempt_id        UInt32                        DEFAULT 0 CODEC(ZSTD(1)),
    session_id        String                        CODEC(ZSTD(1)),
    event_type        LowCardinality(String)        CODEC(ZSTD(1)),
    -- The AVMetric event's own timeline timestamp (CMTime → ms),
    -- preserved separately from `ts` (our wall-clock receive time) so
    -- the dashboard can plot causality without mixing clocks.
    event_ts_ms       Int64                         DEFAULT 0 CODEC(ZSTD(1)),
    raw_json          String                        CODEC(ZSTD(3)),
    labels            Array(LowCardinality(String)) DEFAULT [] CODEC(ZSTD(1)),
    -- Fingerprint over (session_id, event_ts_ms, event_type) so a retry
    -- POST of the same batch does not double-insert.
    event_fingerprint UInt64                        CODEC(ZSTD(1)),
    classification    LowCardinality(String)        DEFAULT 'other' CODEC(ZSTD(1)),
    INDEX idx_play_id    play_id    TYPE bloom_filter GRANULARITY 4,
    INDEX idx_event_type event_type TYPE bloom_filter GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (player_id, ts, event_fingerprint)
TTL toDateTime(ts) + INTERVAL 30 DAY DELETE WHERE classification = 'other',
    toDateTime(ts) + INTERVAL 90 DAY DELETE WHERE classification = 'interesting'
SETTINGS index_granularity = 8192;

-- ── characterization_runs ─────────────────────────────────────────────────
-- One row per (run_id, test_name) — the result of one
-- `tests/characterization/modes/<test>_test.go` invocation. Populated by
-- the test framework via `harness post characterization` at end-of-sweep.
-- Carries the report JSON in full so the dashboard can render per-step +
-- per-variant tables without needing access to the local artifact files.
-- See plan: ~/.claude/plans/characterization-run-report-server-ingest.md
CREATE TABLE IF NOT EXISTS infinite_streaming.characterization_runs
(
    run_id        String                            CODEC(ZSTD(1)),
    test_name     LowCardinality(String)            CODEC(ZSTD(1)),
    platform      LowCardinality(String)            CODEC(ZSTD(1)),
    started_at    DateTime64(3, 'UTC')              CODEC(Delta, ZSTD(1)),
    ended_at      DateTime64(3, 'UTC')              CODEC(Delta, ZSTD(1)),
    player_id     LowCardinality(String)            CODEC(ZSTD(1)),
    play_ids      Array(String)                     CODEC(ZSTD(1)),
    passed        UInt8                             DEFAULT 0,
    summary_json  String                            CODEC(ZSTD(3)),
    report_json   String                            CODEC(ZSTD(3)),
    classification LowCardinality(String)           DEFAULT 'other' CODEC(ZSTD(1)),
    INDEX idx_run_id run_id     TYPE bloom_filter GRANULARITY 4,
    INDEX idx_player player_id  TYPE bloom_filter GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(started_at)
ORDER BY (started_at, test_name, platform)
TTL toDateTime(started_at) + INTERVAL 90 DAY DELETE WHERE classification = 'other',
    toDateTime(started_at) + INTERVAL 180 DAY DELETE WHERE classification = 'interesting'
SETTINGS index_granularity = 8192;
