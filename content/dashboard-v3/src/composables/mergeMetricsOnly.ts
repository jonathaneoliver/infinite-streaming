/**
 * mergeMetricsOnly(existing, incoming) — merge an incoming PlayerRecord
 * into an existing one, dropping any operator-mutable CONTROL fields
 * from `incoming`. Everything else (server-derived state + identity)
 * gets merged wholesale from `incoming`.
 *
 * Used by usePlayer's SSE ingest when an incoming event has the same
 * `control_revision` as the local state (= "metrics tick", not a
 * control mutation). Lets bandwidth charts, RTT, byte counters, fault
 * counters, pattern runtime telemetry, etc. update WITHOUT risking a
 * stale-event stomp on user-edited control state (shape / fault_rules
 * / transfer_timeouts / content / labels).
 *
 * # Design: deny-list wholesale-replace
 *
 * Earlier iterations used an explicit allow-list, which had a real
 * bug: every new server-derived field on PlayerRecord silently fell
 * through and was never merged from SSE. The smoking gun was
 * `user_agent` — set by iOS on first POST but missing from the
 * allow-list, so the cache stayed empty forever until hard refresh.
 * Same for origination_ip, player_ip, first_seen_at,
 * server_received_at_ms.
 *
 * Inverting to a deny-list: the operator-mutable surface is small
 * and well-defined; anything outside that set is server-derived and
 * safe to merge. New PlayerRecord fields are merged by default. If
 * a future field is operator-mutable, add it to CONTROL_FIELDS as
 * part of the same change that introduces its PATCH endpoint.
 *
 * # Why wholesale-replace, not deep-merge
 *
 * Verified empirically against the live SSE wire (2026-05-26): every
 * `player.updated` frame carries a FULL `player_metrics` blob with
 * all 28-30 fields populated and non-null, across every event type
 * iOS emits (heartbeat, state_change, timejump, video_first_frame,
 * buffering_start/end, rate_shift_up/down, restart, playing,
 * video_start_time). There is no sparseness to defend against on
 * this side of the wire. iOS filters nil Optionals before POSTing,
 * and the proxy projects from cumulative per-player state.
 *
 * Wholesale-replace is therefore correct and simpler. An earlier
 * deep-merge layer was added on a misread of the wire and reverted.
 *
 * # The deny-list
 *
 * Sourced from PlayerRecord's field-level docs in
 * api/openapi/v2/proxy.yaml § PlayerRecord. Each entry is either an
 * identity/etag field or one whose description says "*Broadcasts to
 * group on PATCH*" — the canonical signal for "operator-mutable".
 */
import type { PlayerRecord } from '@/repo/v2-repo';

const CONTROL_FIELDS = [
  'id',
  'display_id',
  'control_revision',
  'labels',
  'fault_rules',
  'shape',
  'transfer_timeouts',
  'content',
] as const satisfies readonly (keyof PlayerRecord)[];

const CONTROL_SET = new Set<string>(CONTROL_FIELDS);

export function mergeMetricsOnly(
  existing: PlayerRecord,
  incoming: PlayerRecord,
): PlayerRecord {
  const out: any = { ...existing };
  for (const k of Object.keys(incoming) as (keyof PlayerRecord)[]) {
    if (CONTROL_SET.has(k as string)) continue;
    const v = (incoming as any)[k];
    if (v === undefined) continue;
    out[k] = v;
  }
  return out as PlayerRecord;
}
