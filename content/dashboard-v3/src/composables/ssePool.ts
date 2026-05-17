/**
 * ssePool — shared singleton EventSource for `/api/v2/events?include=raw`
 * (no player_id filter, gets every player event). Used by both:
 *
 *   - usePlayers()    — the picker. Wants every player's events.
 *   - usePlayerSSE()  — per-player. Auto-detects this pool and
 *                       piggybacks on it (filtering by player_id in
 *                       JS) rather than opening a duplicate socket.
 *
 * The motivation is twofold:
 *   1. Chrome caps per-origin connections at 6 — when testing.html
 *      opened one filtered SSE per player_id plus one unfiltered, it
 *      blew the cap and got `ERR_ABORTED`d.
 *   2. Even under the cap, any open EventSource keeps the tab in the
 *      "loading" state. Fewer sockets = spinner stops sooner.
 *
 * Pool semantics: created on first subscriber, closed when the last
 * subscriber releases.
 */

export type ConnectionState = 'connecting' | 'open' | 'closed';

export interface AllPlayersSubscriber {
  onCreated?: (d: any) => void;
  onUpdated?: (d: any) => void;
  onControlsUpdated?: (d: any) => void;
  onDeleted?: (d: any) => void;
  onHeartbeat?: () => void;
  onStateChange?: (s: ConnectionState) => void;
}

let allEs: EventSource | null = null;
let allState: ConnectionState = 'connecting';
const allSubs = new Set<AllPlayersSubscriber>();

function parsePayload(e: Event): any {
  try {
    return JSON.parse((e as MessageEvent).data).data;
  } catch {
    return null;
  }
}

function setAllState(s: ConnectionState) {
  allState = s;
  for (const sub of allSubs) sub.onStateChange?.(s);
}

function fanOut(
  method: 'onCreated' | 'onUpdated' | 'onControlsUpdated' | 'onDeleted' | 'onHeartbeat',
  d?: any,
) {
  for (const sub of allSubs) {
    try {
      const fn = sub[method];
      if (typeof fn === 'function') (fn as any)(d);
    } catch (err) {
      console.warn('[ssePool] subscriber threw', err);
    }
  }
}

function ensureSocket() {
  if (allEs) return;
  const es = new EventSource('/api/v2/events?include=raw');
  allEs = es;
  setAllState('connecting');
  es.addEventListener('open', () => setAllState('open'));
  es.addEventListener('error', () => {
    // EventSource auto-retries until readyState === CLOSED.
    if (es.readyState === EventSource.CLOSED) setAllState('closed');
  });
  es.addEventListener('heartbeat', () => fanOut('onHeartbeat'));
  es.addEventListener('player.created', (e) => {
    const d = parsePayload(e);
    if (d) fanOut('onCreated', d);
  });
  es.addEventListener('player.updated', (e) => {
    const d = parsePayload(e);
    if (d) fanOut('onUpdated', d);
  });
  es.addEventListener('player.controls.updated', (e) => {
    const d = parsePayload(e);
    if (d) fanOut('onControlsUpdated', d);
  });
  es.addEventListener('player.deleted', (e) => {
    const d = parsePayload(e);
    if (d) fanOut('onDeleted', d);
  });
}

/** Is anyone currently subscribed to the all-players SSE? */
export function isAllPoolActive(): boolean {
  return allEs != null && allSubs.size > 0;
}

/** Register a subscriber; opens the socket if it's not already open. */
export function subscribeAllPlayers(sub: AllPlayersSubscriber): { release: () => void } {
  allSubs.add(sub);
  ensureSocket();
  // Tell new subscriber the current connection state immediately so
  // its UI badge doesn't sit at 'connecting' until the next state
  // transition.
  sub.onStateChange?.(allState);
  return {
    release() {
      allSubs.delete(sub);
      if (allSubs.size === 0 && allEs) {
        try {
          allEs.close();
        } catch {
          /* ignore */
        }
        allEs = null;
        allState = 'connecting';
      }
    },
  };
}
