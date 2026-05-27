/**
 * mergeMetricsOnly(existing, incoming) — merge an incoming PlayerRecord
 * into an existing one taking ONLY the telemetry fields, leaving all
 * control fields untouched.
 *
 * Used by usePlayer's SSE ingest when an incoming event has the same
 * `control_revision` as the local state (= "metrics tick", not a control
 * mutation). Lets bandwidth charts, RTT, byte counters, fault counters,
 * pattern runtime telemetry, etc. update without risking a stale-event
 * stomp on user-edited control state (shape / fault_rules /
 * transfer_timeouts / content / labels).
 *
 * Rule of thumb (see plan §"A field is 'metrics' or 'control' by
 * lifecycle"): a field is metrics if the server can change it
 * autonomously without an explicit PATCH; control otherwise.
 */
import type { PlayerRecord } from '@/repo/v2-repo';

// Typed nested groups whose contents are purely server-derived telemetry.
const METRIC_GROUPS = [
  'player_metrics',
  'server_metrics',
  'fault_counters',
  'current_play',
] as const satisfies readonly (keyof PlayerRecord)[];

// Top-level scalars that are server-derived runtime state.
const METRIC_SCALARS = ['last_seen_at', 'loop_count_server'] as const satisfies readonly (keyof PlayerRecord)[];

export function mergeMetricsOnly(
  existing: PlayerRecord,
  incoming: PlayerRecord,
): PlayerRecord {
  const out: any = { ...existing };
  for (const k of METRIC_GROUPS) {
    if (incoming[k] !== undefined) out[k] = incoming[k];
  }
  for (const k of METRIC_SCALARS) {
    if (incoming[k] !== undefined) out[k] = incoming[k];
  }
  return out as PlayerRecord;
}
