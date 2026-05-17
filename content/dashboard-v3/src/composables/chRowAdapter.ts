/**
 * chRowAdapter — adapter from a CH session_snapshots row (wire shape
 * from /api/v3/timeseries) to the v2 PlayerRecord shape the rest of
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

/** Adapter — synthesize a PlayerRecord-shaped object from one CH row.
 *  CH stores flat columns; the v2 wire shape nests them under
 *  player_metrics.* and server_metrics.*. Map here so per-series
 *  accessors and panel templates don't need to know the storage
 *  shape. */
export function chRowToPlayerRecord(row: Record<string, unknown>): PlayerRecord {
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
    // CH stores `stall_count`; v2 wire uses `stalls`.
    stalls: num(row.stall_count),
    stall_time_s: num(row.stall_time_s),
    first_frame_time_s: num(row.video_first_frame_time_s),
    video_start_time_s: num(row.video_start_time_s),
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

  return {
    id: typeof row.player_id === 'string' ? row.player_id : '',
    last_seen_at: typeof row.event_time === 'string' ? row.event_time
      : (typeof row.ts === 'string' ? row.ts : ''),
    player_metrics: playerMetrics,
    server_metrics: serverMetrics,
    current_play: typeof row.play_id === 'string' ? { id: row.play_id } : null,
    loop_count_server: num(row.loop_count_server),
    control_revision: row.control_revision == null ? undefined : String(row.control_revision),
    shape,
    // current_play.manifest.* isn't persisted per-snapshot; EventsTimeline
    // reads manifest_variants directly out of the raw CH row.
  } as unknown as PlayerRecord;
}
