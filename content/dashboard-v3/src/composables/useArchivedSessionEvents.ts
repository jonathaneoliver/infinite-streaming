/**
 * useArchivedSessionEvents(playerId, playId)
 *
 * Fetches the notable-events list for the SessionViewer's rail markers
 * and jump-to-event dropdown. Mirrors the legacy session-replay.js
 * call to /analytics/api/v2/session_events (NDJSON) — one record per
 * notable thing (STALL, ERROR, SHIFT_UP/DOWN, FROZEN, LOOP, USER_MARK,
 * RESTART, etc).
 *
 * Each event has at minimum: { event_time, type, severity? }. Some
 * carry payload fields (e.g. STALL → stall_seconds, SHIFT_UP →
 * previous/next bitrate). The dropdown groups by priority (error /
 * warn / info) and chip colors come from the legacy palette.
 */
import { onScopeDispose, ref, watch, type Ref } from 'vue';

export interface SessionEvent {
  // Forwarder NDJSON stamps each row with `ts` (ClickHouse string
  // format "YYYY-MM-DD HH:MM:SS.fff"); `event_time` is the legacy
  // alias kept around for forwarder versions that still emit it.
  ts?: string;
  event_time?: string;
  type?: string;
  info?: string;
  // Two-dimensional taxonomy from the forwarder's session_events
  // handler (analytics/go-forwarder/main.go ~1143-1272):
  //   - kind: 'effect' (user-visible outcome) vs 'cause' (proxy/
  //           system action that may or may not have produced one)
  //   - priority: 1=Critical, 2=High, 3=Medium, 4=Low — computed
  //           server-side via a multiIf() on event type
  // Both are present on every row; this UI consumes them directly
  // rather than re-classifying client-side.
  kind?: 'effect' | 'cause';
  priority?: 1 | 2 | 3 | 4;
  // Legacy `severity` field — kept as fallback for old forwarder
  // builds that haven't been redeployed yet.
  severity?: string;
  [k: string]: unknown;
}

function buildQuery(p: Record<string, string | number | undefined>): string {
  const parts: string[] = [];
  for (const k of Object.keys(p)) {
    const v = p[k];
    if (v == null || v === '') continue;
    parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(v)));
  }
  return parts.length ? '?' + parts.join('&') : '';
}

export function useArchivedSessionEvents(
  playerId: Ref<string>,
  playId: Ref<string | null>,
) {
  const events = ref<SessionEvent[]>([]);
  const loading = ref(false);
  const error = ref<string | null>(null);

  // Re-fetch trigger sources:
  //  1. Identity refs (playerId / playId) change → wipe + full
  //     re-fetch (watch below).
  //  2. Periodic poll while the page is open → re-fetch in place so
  //     new server-derived events (HTTP 4xx/5xx, faults, stalls,
  //     downshifts) show up on the Focus Window within a few seconds
  //     of the forwarder ingesting them.
  // In-flight guard: skip a poll tick when the previous request
  // hasn't finished. Cancelling an almost-done fetch wastes the
  // server-side work AND surfaces as a "(canceled)" entry in the
  // DevTools network panel. Letting the previous one finish gives
  // the user the most recent successful response; the next 3 s tick
  // picks up any newer events. The AbortController is still wired
  // up so an identity change (player_id / play_id swap) can hard-
  // abort the previous query — that one IS worth cancelling.
  let abort: AbortController | null = null;
  let inFlight = false;
  // 10 s ceiling so a stuck request (e.g. browser connection coalesced
  // with a dead socket) doesn't pin the in-flight guard forever and
  // freeze all subsequent polls. Server query is ~200 ms typically;
  // 10 s is comfortably above the legitimate slow path.
  const REQUEST_TIMEOUT_MS = 10_000;
  async function fetchEvents(): Promise<void> {
    if (!playerId.value) {
      events.value = [];
      return;
    }
    if (inFlight) return; // skip; previous still running
    abort = new AbortController();
    const timeoutId = window.setTimeout(() => {
      try { abort?.abort(); } catch { /* ignore */ }
    }, REQUEST_TIMEOUT_MS);
    inFlight = true;
    loading.value = true;
    try {
      const qs = buildQuery({
        player_id: playerId.value,
        play_id: playId.value ?? undefined,
        // Match the forwarder's expanded cap so long sessions get
        // full event history. With ABR adaptation accounting for
        // ~67% of events on a thrashing player, capping at 5000
        // historically squeezed out rare faults / errors.
        limit: 50000,
      });
      const resp = await fetch('/analytics/api/v2/session_events' + qs, {
        headers: { Accept: 'application/x-ndjson' },
        signal: abort.signal,
      });
      if (!resp.ok) throw new Error(`session_events ${resp.status}`);
      const text = await resp.text();
      const out: SessionEvent[] = [];
      for (const line of text.split('\n')) {
        if (!line) continue;
        try {
          const ev = JSON.parse(line);
          if (ev && !ev._error) out.push(ev as SessionEvent);
        } catch { /* skip */ }
      }
      // Reassign whole array rather than mutating in place — the
      // brush rail / accordion watch this ref and need identity
      // change to react.
      events.value = out;
      error.value = null;
    } catch (e: any) {
      if (e?.name !== 'AbortError') error.value = String(e?.message ?? e);
    } finally {
      window.clearTimeout(timeoutId);
      loading.value = false;
      inFlight = false;
    }
  }

  // Identity-change trigger: wipe + re-fetch on player / play swap.
  // Hard-abort any in-flight query first — it would return data for
  // the OLD player which we'd then immediately discard.
  watch(
    [playerId, playId],
    () => {
      if (abort) { abort.abort(); abort = null; inFlight = false; }
      events.value = [];
      error.value = null;
      void fetchEvents();
    },
    { immediate: true },
  );

  // Periodic poll. 3 s lines up with the forwarder's batched ingest
  // (network rows + snapshots arrive in ~1-2 s batches), so a fresh
  // event lands on the Focus Window within ~3-5 s of injection.
  const POLL_MS = 3000;
  const timer = window.setInterval(() => { void fetchEvents(); }, POLL_MS);
  onScopeDispose(() => {
    window.clearInterval(timer);
    if (abort) abort.abort();
  });

  return { events, loading, error };
}
