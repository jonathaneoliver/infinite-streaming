-- Analytics schema for InfiniteStream session snapshots.
--
-- One row per session per SSE broadcast tick. The forwarder dedupes by
-- payload fingerprint, so an unchanging session does not generate rows.
-- Grafana, ad-hoc SQL, and testing.html replay mode all read from here.

CREATE DATABASE IF NOT EXISTS infinite_streaming;

CREATE TABLE IF NOT EXISTS infinite_streaming.session_snapshots
(
    ts                    DateTime64(3, 'UTC')        CODEC(DoubleDelta, ZSTD(1)),
    revision              UInt64                      CODEC(DoubleDelta, ZSTD(1)),
    session_id            String                      CODEC(ZSTD(1)),
    play_id               LowCardinality(String)      CODEC(ZSTD(1)),
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
    display_resolution    LowCardinality(String)      CODEC(ZSTD(1)),
    video_resolution      LowCardinality(String)      CODEC(ZSTD(1)),
    frames_displayed      UInt64                      DEFAULT 0,
    dropped_frames        UInt32                      DEFAULT 0,
    stall_count           UInt32                      DEFAULT 0,
    stall_time_s          Float32                     CODEC(ZSTD(1)),
    position_s            Float32                     CODEC(ZSTD(1)),
    live_edge_s           Float32                     CODEC(ZSTD(1)),
    true_offset_s         Float32                     CODEC(ZSTD(1)),
    playback_rate         Float32                     CODEC(ZSTD(1)),
    loop_count_player     UInt32                      DEFAULT 0,
    loop_count_server     UInt32                      DEFAULT 0,

    -- Player events (discrete signals embedded in the heartbeat snapshot;
    -- testing.html derives event-lane points from transitions in these).
    last_event            LowCardinality(String)      CODEC(ZSTD(1)),
    last_event_at         String                      CODEC(ZSTD(1)),
    trigger_type          LowCardinality(String)      CODEC(ZSTD(1)),
    event_time            String                      CODEC(ZSTD(1)),
    player_error          String                      CODEC(ZSTD(1)),

    -- Player metrics (extended)
    avg_network_bitrate_mbps     Float32 CODEC(ZSTD(1)),
    buffer_end_s                 Float32 CODEC(ZSTD(1)),
    last_stall_time_s            Float32 CODEC(ZSTD(1)),
    live_offset_s                Float32 CODEC(ZSTD(1)),
    playhead_wallclock_ms        Int64   CODEC(ZSTD(1)),
    seekable_end_s               Float32 CODEC(ZSTD(1)),
    metrics_source               LowCardinality(String) CODEC(ZSTD(1)),
    video_first_frame_time_s     Float32 CODEC(ZSTD(1)),
    video_quality_pct            Float32 CODEC(ZSTD(1)),
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
    control_revision            UInt64                 DEFAULT 0,

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
    session_json         String                       CODEC(ZSTD(3))
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (session_id, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;

-- Bloom filter on session_id for fast point lookups in replay mode.
ALTER TABLE infinite_streaming.session_snapshots
    ADD INDEX IF NOT EXISTS idx_session_id session_id TYPE bloom_filter(0.01) GRANULARITY 4;

ALTER TABLE infinite_streaming.session_snapshots
    ADD INDEX IF NOT EXISTS idx_player_id player_id TYPE bloom_filter(0.01) GRANULARITY 4;

ALTER TABLE infinite_streaming.session_snapshots
    ADD INDEX IF NOT EXISTS idx_play_id play_id TYPE bloom_filter(0.01) GRANULARITY 4;

-- Bring older deployments up to date if the column predates this column.
ALTER TABLE infinite_streaming.session_snapshots
    ADD COLUMN IF NOT EXISTS play_id LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS content_id LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS last_event LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS last_event_at String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS trigger_type LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS event_time String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS player_error String CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS avg_network_bitrate_mbps Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS buffer_end_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS last_stall_time_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS live_offset_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS playhead_wallclock_ms Int64 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS seekable_end_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS metrics_source LowCardinality(String) CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_first_frame_time_s Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_quality_pct Float32 CODEC(ZSTD(1)),
    ADD COLUMN IF NOT EXISTS video_start_time_s Float32 CODEC(ZSTD(1)),
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
    ADD COLUMN IF NOT EXISTS control_revision UInt64 DEFAULT 0;

-- Convert legacy manifest_variants UInt16 (broken: always stored 0 because
-- the SSE field is an array, not a number) to String (JSON of the variant
-- ladder). Safe MODIFY since the only existing values are zero.
ALTER TABLE infinite_streaming.session_snapshots
    MODIFY COLUMN IF EXISTS manifest_variants String CODEC(ZSTD(3));

-- Per-request HAR-style log so the session-viewer's network log fold
-- can replay archived sessions whose go-proxy buffer is gone. Forwarder
-- polls /api/session/<id>/network for live sessions and dedupes rows
-- via entry_fingerprint. Headers/query are stored as JSON strings —
-- almost never queried column-by-column and often empty.
CREATE TABLE IF NOT EXISTS infinite_streaming.network_requests
(
    ts                       DateTime64(3, 'UTC')   CODEC(Delta, ZSTD(1)),
    session_id               String                 CODEC(ZSTD(1)),
    play_id                  LowCardinality(String) CODEC(ZSTD(1)),
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
    INDEX idx_play_id play_id TYPE bloom_filter GRANULARITY 4,
    INDEX idx_status status TYPE minmax GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (session_id, ts, entry_fingerprint)
TTL toDateTime(ts) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;
