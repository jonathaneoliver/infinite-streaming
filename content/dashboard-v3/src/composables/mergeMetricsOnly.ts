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

// Metric BLOB fields — server-derived nested objects. The proxy
// sometimes emits SSE `updated` events with PARTIAL blobs (e.g. on
// state-change events only carrying state + event_time + a few
// timing values). If we replaced the whole blob wholesale, we'd
// lose every field the partial didn't include — then the panel
// shows "—" for fields the data actually has, until the next full
// snapshot ~1s later restores them.
//
// Solution: deep-merge these per-field. Stale-by-one-tick is
// always better than blank; the next full snapshot restores the
// up-to-date value.
const METRIC_BLOBS = [
  'player_metrics',
  'server_metrics',
  'fault_counters',
  'current_play',
] as const satisfies readonly (keyof PlayerRecord)[];

const METRIC_BLOB_SET = new Set<string>(METRIC_BLOBS);

export function mergeMetricsOnly(
  existing: PlayerRecord,
  incoming: PlayerRecord,
): PlayerRecord {
  // SSE event-type-aware merge of metric BLOBS.
  //
  // The proxy always sends a full PlayerRecord on every `player.updated`
  // SSE frame, but iOS's nested `player_metrics` blob is SPARSE when
  // the originating iOS event was non-heartbeat (state_change,
  // buffering_start, etc. only include the fields relevant to that
  // event). Heartbeats — fired ~1Hz — are the FULL snapshots.
  //
  // Detection: look at incoming.player_metrics.last_event. If it's
  // "heartbeat", treat this as a full snapshot and REPLACE the
  // metric blobs wholesale (clears stale values that iOS has
  // stopped sending). Otherwise treat as a delta and DEEP-MERGE
  // (preserves fields that aren't in this delta).
  //
  // Trade-off: stale values can linger for up to ~1s between
  // heartbeats. The previous bug (fields blanking for 1s on every
  // state transition) was worse — there a field would go missing
  // even though the data had it. Heartbeat-driven refresh ensures
  // any "stale 0 from past iOS measurement" gets cleared on the
  // next ~1s tick.
  const lastEvent = (incoming as any).player_metrics?.last_event;
  const triggerType = (incoming as any).player_metrics?.trigger_type;
  const isHeartbeat = lastEvent === 'heartbeat' || triggerType === 'heartbeat';

  const out: any = { ...existing };
  for (const k of Object.keys(incoming) as (keyof PlayerRecord)[]) {
    if (CONTROL_SET.has(k as string)) continue;          // operator-mutable; preserve existing
    const v = (incoming as any)[k];
    if (v === undefined) continue;                       // missing in tick → keep existing
    // Metric BLOBS get either wholesale-replaced (heartbeat = full
    // snapshot) or deep-merged (delta event = preserve cached fields).
    // Anything else (scalars, strings) always gets the incoming value.
    if (METRIC_BLOB_SET.has(k as string) && v !== null && typeof v === 'object') {
      if (isHeartbeat) {
        out[k] = v;                                      // wholesale replace
      } else {
        const prev = (existing as any)[k];
        if (prev !== null && typeof prev === 'object') {
          out[k] = mergeShallow(prev, v);                // deep merge
          continue;
        }
        out[k] = v;
      }
      continue;
    }
    out[k] = v;
  }
  return out as PlayerRecord;
}

/**
 * mergeShallow — clone `prev`, then copy every DEFINED key from
 * `next` over the top. A defined null in `next` overwrites; a true
 * `undefined` (missing key) preserves prev's value. Used for the
 * metric BLOBS to absorb partial SSE updates without losing fields.
 */
function mergeShallow<T extends object>(prev: T, next: Partial<T>): T {
  const out: any = { ...prev };
  for (const k of Object.keys(next) as (keyof T)[]) {
    const v = (next as any)[k];
    if (v === undefined) continue;
    out[k] = v;
  }
  return out as T;
}
