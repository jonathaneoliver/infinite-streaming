/**
 * chRowAdapter — adapter from a CH session_snapshots row (wire shape
 * from /api/v2/timeseries) to the v2 PlayerRecord shape the rest of
 * the dashboard expects (nested player_metrics / server_metrics, with
 * a couple of fields renamed).
 *
 * Lives outside the renderers because both MetricsLineChart and
 * SessionDisplay read CH rows now (TS5+TS6) and shouldn't each carry
 * their own copy of the mapping. Mirrors the inverse direction in
 * go-forwarder/v2translate.go.
 */
import type { PlayerRecord } from '@/repo/v2-repo';

export function tsOfRow(row: Record<string, unknown>): number {
  const v = row.ts;
  if (typeof v === 'number') return v;
  if (typeof v !== 'string' || !v) return NaN;
  if (v.length > 10 && v.charAt(10) === ' ') {
    return Date.parse(v.replace(' ', 'T') + 'Z');
  }
  return Date.parse(v);
}

function num(v: unknown): number | null {
  if (v == null) return null;
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v === 'string') { const n = Number(v); return Number.isFinite(n) ? n : null; }
  return null;
}

// Normalise a CH JSONEachRow timestamp ("YYYY-MM-DD HH:MM:SS.SSS",
// no T, no Z) to ISO-with-Z so `new Date(...)` reads it as UTC across
// all browsers — matching the live PlayerRecord shape from go-proxy
// (Go RFC3339 always has Z). Empty / already-ISO inputs pass through.
function toISOWithZ(s: string): string {
  if (!s) return s;
  if (s.endsWith('Z') || /[+-]\d{2}:?\d{2}$/.test(s)) return s;
  const isoBody = s.includes('T') ? s : s.replace(' ', 'T');
  return isoBody + 'Z';
}

// #550 Phase 1: convert UInt32-ms to Float seconds for the existing
// dashboard adapters (panel + charts expect seconds). 0 ms → null so
// "no data" rows render as "—" instead of "0.000s".
function msToSeconds(v: unknown): number | null {
  const n = num(v);
  if (n == null || n === 0) return null;
  return n / 1000;
}

// #550 Phase 1 soft cutover: prefer the new _ms column; fall back to
// the deprecated _s column on rows that pre-date the migration.
function msOrSeconds(msV: unknown, sV: unknown): number | null {
  const fromMs = msToSeconds(msV);
  if (fromMs != null) return fromMs;
  return num(sV);
}

/** Adapter — synthesize a PlayerRecord-shaped object from one CH row.
 *  CH stores flat columns; the v2 wire shape nests them under
 *  player_metrics.* and server_metrics.*. Map here so per-series
 *  accessors and panel templates don't need to know the storage
 *  shape. */
export interface ChRowContext {
  /** ISO/CH timestamp of the play's first session_events row. The
   * brush-end row alone gives us "last seen" but not "first seen";
   * callers with access to the stream range bounds should pass this
   * so SessionDetails' "First Request" + "Session Duration" tiles
   * render the play's real start, not the current cursor's row. */
  firstSeenAt?: string;
  /** Maximum control_revision observed across the play. Without this,
   * SessionDetails' "Control Rev" tile shows the brush-cursor row's
   * value (mid-play, may be older than the play's final mutation
   * count) — confusing when comparing to the live Testing dashboard
   * which always reports the latest. SessionDisplay walks the stream
   * once via inRange() and passes the max. */
  maxControlRevision?: string;
  /** Maximum attempt_id observed across the play. attempt_id =
   * recovery-attempt counter (1 on initial play, +1 on every
   * restart/auto-recovery). The play's max is "how many tries did
   * this play take" — meaningful at the play level even though
   * attempt_id is per-snapshot in the schema. */
  maxAttemptId?: number;
}

export function chRowToPlayerRecord(
  row: Record<string, unknown>,
  ctx?: ChRowContext,
): PlayerRecord {
  const playerMetrics = {
    event_time: typeof row.event_time === 'string' ? row.event_time
      : (typeof row.ts === 'string' ? row.ts : ''),
    state: typeof row.player_state === 'string' ? row.player_state : '',
    waiting_reason: typeof row.waiting_reason === 'string' ? row.waiting_reason : '',
    error: typeof row.player_error === 'string' ? row.player_error : '',
    video_resolution: typeof row.video_resolution === 'string' ? row.video_resolution : '',
    display_resolution: typeof row.display_resolution === 'string' ? row.display_resolution : '',
    video_bitrate_mbps: num(row.video_bitrate_mbps),
    network_bitrate_mbps: num(row.network_bitrate_mbps),
    avg_network_bitrate_mbps: num(row.avg_network_bitrate_mbps),
    buffer_depth_s: num(row.buffer_depth_s),
    buffer_end_s: num(row.buffer_end_s),
    live_offset_s: num(row.live_offset_s),
    true_offset_s: num(row.true_offset_s),
    frames_displayed: num(row.frames_displayed),
    dropped_frames: num(row.dropped_frames),
    // #550 Phase 1: prefer the new gerund-named ms columns; fall
    // back to the deprecated _s variants during the soft cutover so
    // pre-migration historical rows still render. The forwarder
    // mirror-writes both during the deprecation window.
    stalls: num(row.stalling_count) || num(row.stall_count),
    stall_time_s: msOrSeconds(row.stalling_time_ms, row.stall_time_s),
    first_frame_time_s: msOrSeconds(row.video_first_frame_time_ms, row.video_first_frame_time_s),
    video_start_time_s: msOrSeconds(row.video_start_time_ms, row.video_start_time_s),
    // #550 Phase 1: new residency + delta columns + sticky durations.
    playing_time_s: msToSeconds(row.playing_time_ms),
    pausing_time_s: msToSeconds(row.pausing_time_ms),
    buffering_time_s: msToSeconds(row.buffering_time_ms),
    stalling_time_s: msToSeconds(row.stalling_time_ms),
    idling_time_s: msToSeconds(row.idling_time_ms),
    seeking_time_s: msToSeconds(row.seeking_time_ms),
    trickplaying_time_s: msToSeconds(row.trickplaying_time_ms),
    playing_count: num(row.playing_count),
    pausing_count: num(row.pausing_count),
    buffering_count: num(row.buffering_count),
    stalling_count: num(row.stalling_count),
    idling_count: num(row.idling_count),
    seeking_count: num(row.seeking_count),
    trickplaying_count: num(row.trickplaying_count),
    stall_duration_s: msToSeconds(row.stall_duration_ms),
    buffering_duration_s: msToSeconds(row.buffering_duration_ms),
    // Orthogonal "this stall won't auto-recover" discriminator. True
    // when AVPlayer transitioned stalled → .paused (give-up) and the
    // app must call play() to resume. PLAYERSTATE lane stays on
    // "stalled"; this flag drives the chip the operator sees.
    stall_stuck: row.stall_stuck === true || row.stall_stuck === 'true' || row.stall_stuck === 1,
    // Per-variant dwell map. CH stores as JSON string; pass through
    // as-is and let PlayerMetrics.vue's parser handle decode. Empty
    // string ⇒ "no variant data" path in the consumer.
    time_per_variant_s: typeof row.time_per_variant_s === 'string' ? row.time_per_variant_s : '',
    // #550 Phase 2: outcome + structured error fields.
    playback_status: typeof row.playback_status === 'string' ? row.playback_status : '',
    playback_reason: typeof row.playback_reason === 'string' ? row.playback_reason : '',
    error_code: num(row.error_code),
    error_domain: typeof row.error_domain === 'string' ? row.error_domain : '',
    terminal_error_code: num(row.terminal_error_code),
    terminal_error_domain: typeof row.terminal_error_domain === 'string' ? row.terminal_error_domain : '',
    error_count: num(row.error_count),
    // #550 Phase 4: device / platform taxonomy.
    os_version_major: num(row.os_version_major),
    os_version_minor: num(row.os_version_minor),
    app_version: typeof row.app_version === 'string' ? row.app_version : '',
    device_class: typeof row.device_class === 'string' ? row.device_class : '',
    device_model: typeof row.device_model === 'string' ? row.device_model : '',
    player_tech: typeof row.player_tech === 'string' ? row.player_tech : '',
    // Orientation-aware "WxH"; supersedes screen_width_px / _height_px / _density.
    device_resolution: typeof row.device_resolution === 'string' ? row.device_resolution : '',
    // panel_v1 bundle additions — populated only when the request
    // included the panel_v1 bundle (testing.html + session-viewer).
    // Without this bundle these columns aren't projected and the
    // PlayerMetrics panel shows "—" for fields the data actually has.
    // `source` is the dashboard label for CH's `metrics_source`.
    last_event: typeof row.last_event === 'string' ? row.last_event : null,
    trigger_type: typeof row.trigger_type === 'string' ? row.trigger_type : null,
    position_s: num(row.position_s),
    playback_rate: num(row.playback_rate),
    seekable_end_s: num(row.seekable_end_s),
    live_edge_s: num(row.live_edge_s),
    source: typeof row.metrics_source === 'string' ? row.metrics_source : null,
    loop_count_player: num(row.loop_count_player),
    // #550 Phase 1: stall_duration_ms supersedes last_stall_time_s.
    last_stall_time_s: msOrSeconds(row.stall_duration_ms, row.last_stall_time_s),
    video_quality_pct: num(row.video_quality_pct),
    video_quality_60s_pct: num(row.video_quality_60s_pct),
    video_quality_avg_pct: num(row.video_quality_avg_pct),
    playhead_wallclock_ms: num(row.playhead_wallclock_ms),
    player_restarts: num(row.player_restarts),
    profile_shift_count: num(row.profile_shift_count),
  } as PlayerRecord['player_metrics'];

  const serverMetrics = {
    measured_mbps: num(row.measured_mbps),
    mbps_shaper_rate: num(row.mbps_shaper_rate),
    mbps_shaper_avg: num(row.mbps_shaper_avg),
    mbps_transfer_rate: num(row.mbps_transfer_rate),
    mbps_transfer_complete: num(row.mbps_transfer_complete),
    // CH `client_*` columns map to v2 `server_metrics.*` (TCP_INFO at
    // the proxy's socket, server-side from the player's POV).
    rtt_ms: num(row.client_rtt_ms),
    rtt_min_ms: num(row.client_rtt_min_ms),
    rtt_max_ms: num(row.client_rtt_max_ms),
    rto_ms: num(row.client_rto_ms),
    path_ping_rtt_ms: num(row.client_path_ping_rtt_ms),
    // Issue #486: client-side TTFB-derived RTT from iOS AVMetrics.
    // Surfaced on server_metrics for chart parity with the rest of
    // the RTT family even though it originates client-side; the
    // PlayerRecord shape doesn't distinguish strictly enough to
    // warrant a third bucket.
    rtt_avmetrics_ms: num(row.client_rtt_avmetrics_ms),
    // Server's view of the player's active variant — pairs with
    // player_metrics.video_bitrate_mbps so BandwidthChart can draw
    // both "Player Variant" and "Server Variant" for archived plays.
    rendition_mbps: num(row.server_video_rendition_mbps),
  } as PlayerRecord['server_metrics'];

  // Reconstruct the per-snapshot shaper config the BandwidthChart's
  // "Limit (rate_mbps)" accessor reads via `p.shape.*`. The proxy's
  // pattern_rate_runtime_mbps is the effective enforced rate when a
  // pattern is active; static `nftables_bandwidth_mbps` is the
  // baseline when no pattern is running. pattern_steps is stored as
  // a JSON string in CH — parsed lazily here so the accessor can
  // walk `steps[stepIdx-1].rate_mbps` for the legacy "between
  // pattern_rate_runtime updates" fallback.
  let patternSteps: { rate_mbps?: number }[] | null = null;
  const stepsRaw = row.nftables_pattern_steps;
  if (typeof stepsRaw === 'string' && stepsRaw.length > 0 && stepsRaw !== 'null') {
    try { const parsed = JSON.parse(stepsRaw); if (Array.isArray(parsed)) patternSteps = parsed; }
    catch { /* malformed pattern JSON — leave null */ }
  }
  const patternEnabled = Number(row.nftables_pattern_enabled ?? 0) === 1;
  const shape = {
    rate_mbps: num(row.nftables_bandwidth_mbps),
    pattern: patternEnabled && patternSteps && patternSteps.length
      ? { steps: patternSteps }
      : null,
    pattern_rate_runtime_mbps: num(row.nftables_pattern_rate_runtime_mbps),
    pattern_step: num(row.nftables_pattern_step),
    pattern_step_runtime: num(row.nftables_pattern_step),
  };

  // Synthesise a minimal raw_session so accessors that read v1-shape
  // fields (e.g. BandwidthChart's "Effective Limit" reading
  // `raw_session.effective_rate_limit_mbps`) work uniformly across
  // the live and archive code paths. Add only the fields actually
  // consumed — full passthrough would inflate every CH row's footprint
  // in the chart memory.
  const rawSession = {
    effective_rate_limit_mbps: num(row.effective_rate_limit_mbps),
    // Identity fields SessionDetails.vue reads from raw_session (port)
    // and group_id (top-level). Mirror the keys the live PlayerRecord
    // shape uses so the same component renders both paths.
    group_id: typeof row.group_id === 'string' ? row.group_id : undefined,
    x_forwarded_port: row.x_forwarded_port != null ? Number(row.x_forwarded_port) : undefined,
    x_forwarded_port_external: row.x_forwarded_port_external != null
      ? Number(row.x_forwarded_port_external)
      : undefined,
  };

  // Identity / lifecycle fields SessionDetails.vue reads at the top
  // level (matching the live PlayerRecord shape from /api/v2/players).
  //
  // Timestamp normalisation: ClickHouse JSONEachRow emits DateTime64
  // as "YYYY-MM-DD HH:MM:SS.SSS" — no T separator, no Z suffix. Some
  // browsers interpret that as LOCAL time when fed to `new Date(...)`,
  // while the live PlayerRecord (Go RFC3339) always has the Z, parsed
  // as UTC. The mismatch made archive timestamps render hours off in
  // SessionDetails' fmtDate. Normalise CH-format → ISO-with-Z so both
  // paths display identical wall-clock times.
  const lastSeenAt = toISOWithZ(typeof row.event_time === 'string' ? row.event_time
    : (typeof row.ts === 'string' ? row.ts : ''));
  const userAgent = typeof row.user_agent === 'string' ? row.user_agent : undefined;
  const playerIp = typeof row.player_ip === 'string' ? row.player_ip : undefined;
  const originationIp = typeof row.origination_ip === 'string' ? row.origination_ip : undefined;
  // master_manifest_url is the master playlist the player loaded
  // (rendered as "Master Manifest URL" in SessionDetails). Fall back
  // to manifest_url (the variant playlist) only if the master column
  // wasn't projected.
  const masterUrl = typeof row.master_manifest_url === 'string' && row.master_manifest_url
    ? row.master_manifest_url
    : (typeof row.manifest_url === 'string' ? row.manifest_url : undefined);

  return {
    id: typeof row.player_id === 'string' ? row.player_id : '',
    // SessionDetails reads p.display_id; matches the live PlayerRecord
    // shape from v2translate which projects session_number → DisplayId.
    display_id: num(row.session_number) ?? undefined,
    last_seen_at: lastSeenAt,
    first_seen_at: ctx?.firstSeenAt ?? lastSeenAt,
    user_agent: userAgent,
    player_ip: playerIp,
    origination_ip: originationIp,
    server_received_at_ms: num(row.server_received_at_ms) ?? undefined,
    player_metrics: playerMetrics,
    server_metrics: serverMetrics,
    current_play: typeof row.play_id === 'string'
      ? {
          id: row.play_id,
          attempt_id: ctx?.maxAttemptId ?? (num(row.attempt_id) ?? undefined),
          manifest: masterUrl ? { master_url: masterUrl } : undefined,
        }
      : null,
    loop_count_server: num(row.loop_count_server),
    // control_revision is now String — go-proxy's RFC3339Nano "ETag".
    // Type-changed-in-place via DROP UInt64 + RENAME control_revision_str
    // → control_revision. maxControlRevision from ctx wins when present
    // (final value across the play).
    control_revision: ctx?.maxControlRevision
      ?? (typeof row.control_revision === 'string' ? row.control_revision : undefined),
    shape,
    raw_session: rawSession,
    // current_play.manifest.* isn't persisted per-snapshot; EventsTimeline
    // reads manifest_variants directly out of the raw CH row.
  } as unknown as PlayerRecord;
}
