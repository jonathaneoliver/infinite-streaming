/**
 * mergeMetricsOnly(existing, incoming) — merge an incoming PlayerRecord
 * into an existing one, dropping any operator-mutable CONTROL fields
 * from `incoming`. Everything else (server-derived state + identity)
 * gets merged.
 *
 * Used by usePlayer's SSE ingest when an incoming event has the same
 * `control_revision` as the local state (= "metrics tick", not a
 * control mutation). Lets bandwidth charts, RTT, byte counters, fault
 * counters, pattern runtime telemetry, etc. update WITHOUT risking a
 * stale-event stomp on user-edited control state (shape / fault_rules
 * / transfer_timeouts / content / labels).
 *
 * # Design choice: deny-list, not allow-list
 *
 * Before 2026-05-26 this used an explicit `METRIC_GROUPS` +
 * `METRIC_SCALARS` allow-list. That had a real bug: every new
 * server-derived field on PlayerRecord silently fell through and
 * NEVER got merged from SSE. The original instance: `user_agent`
 * was populated on the iOS app's first metrics POST, but because
 * it wasn't in the allow-list, the cache stayed empty forever
 * until a hard-refresh. Same was true for origination_ip / player_ip
 * / first_seen_at / server_received_at_ms.
 *
 * Inverting the model: the deny-list is SHORTER, more stable, and
 * easier to keep correct. The operator-mutable control surface on
 * PlayerRecord is small and well-defined; anything outside that set
 * is server-derived and safe to merge. New PlayerRecord fields are
 * merged by default, which is the correct default — if a future field
 * is operator-editable, the author should add it to CONTROL_FIELDS
 * here as part of the same change that introduces the PATCH endpoint.
 *
 * # The deny-list
 *
 * Sourced from PlayerRecord's own field-level docs in
 * api/openapi/v2/proxy.yaml § PlayerRecord. Each entry is a field
 * whose description says "*Broadcasts to group on PATCH*" (the
 * canonical signal for "operator-mutable") OR is an identity/etag
 * field that must not be overwritten by a metric tick.
 */
import type { PlayerRecord } from '@/repo/v2-repo';

const CONTROL_FIELDS = [
  // Identity — fixed at creation; metric ticks have no business
  // touching these.
  'id',
  'display_id',
  // Concurrency token — only `controls.updated` SSE events should
  // change this. Metric ticks carry the same revision; defensive
  // skip just in case.
  'control_revision',
  // Operator-mutable control surfaces. PATCH writes broadcast to
  // the group; a metric tick from an outdated cached state would
  // stomp a recent operator edit.
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
    if (CONTROL_SET.has(k as string)) continue;          // operator-mutable; preserve existing
    const v = (incoming as any)[k];
    if (v === undefined) continue;                       // missing in tick → keep existing
    out[k] = v;
  }
  return out as PlayerRecord;
}
