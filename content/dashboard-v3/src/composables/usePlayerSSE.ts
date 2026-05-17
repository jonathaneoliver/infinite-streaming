/**
 * usePlayerSSE — single-player SSE subscription with three layers of
 * deduplication so a single page never opens more sockets than needed:
 *
 *   1. **Piggyback on the all-players pool** when it's active. The
 *      picker (testing.html) already has an unfiltered SSE listening
 *      to every player. Per-panel `usePlayer(id)` calls on that page
 *      filter events client-side from that one socket instead of
 *      opening N more filtered sockets to the server.
 *
 *   2. **Per-player pool** for pages without the all-players SSE
 *      (e.g. testing-session.html drill-in). One filtered socket per
 *      player_id, shared by all panels that subscribe to that id.
 *
 *   3. **Per-instance dispatch** — multiple `usePlayer(id)` callers in
 *      the same page register distinct `handlers` objects that all
 *      fire from the shared socket.
 *
 * Why bother: Chrome caps per-origin HTTP connections at 6, and any
 * open EventSource keeps the tab in the "loading" state. Without
 * pooling, testing.html opened ~one socket per panel × players ≈ 30+
 * sockets, blew the cap, and the tab spun forever.
 *
 * Handlers:
 *   - onUpdated:          `player.updated` (≈1Hz metrics tick)
 *   - onControlsUpdated:  `player.controls.updated` (server emits only
 *                         on real control mutations — treat as
 *                         definitive new control snapshot)
 *   - onCreated/onDeleted: lifecycle
 *   - onHeartbeat:        SSE keep-alive (no payload)
 */
import { onScopeDispose, ref, watch, type Ref } from 'vue';
import {
  isAllPoolActive,
  subscribeAllPlayers,
  type AllPlayersSubscriber,
  type ConnectionState as PoolState,
} from './ssePool';

type Handlers = {
  onCreated?: (data: any) => void;
  onUpdated?: (data: any) => void;
  onControlsUpdated?: (data: any) => void;
  onDeleted?: (data: any) => void;
  onHeartbeat?: () => void;
};

export type ConnectionState = PoolState;

/* ─── Per-player-id pool (fallback when all-pool not active) ────── */

interface PerPlayerEntry {
  es: EventSource;
  state: ConnectionState;
  subscribers: Set<Handlers>;
  stateRefs: Set<Ref<ConnectionState>>;
}

const perPlayerPool = new Map<string, PerPlayerEntry>();

function parsePayload(e: MessageEvent): any {
  try {
    return JSON.parse((e as MessageEvent).data).data;
  } catch {
    return null;
  }
}

function getOrCreatePerPlayer(pid: string): PerPlayerEntry {
  const cached = perPlayerPool.get(pid);
  if (cached) return cached;
  const url = `/api/v2/events?include=raw&player_id=${encodeURIComponent(pid)}`;
  const es = new EventSource(url);
  const entry: PerPlayerEntry = {
    es,
    state: 'connecting',
    subscribers: new Set(),
    stateRefs: new Set(),
  };

  function setState(s: ConnectionState) {
    entry.state = s;
    for (const r of entry.stateRefs) r.value = s;
  }
  function fan(method: keyof Handlers, payload?: any) {
    for (const h of entry.subscribers) {
      try {
        const fn = h[method];
        if (typeof fn === 'function') (fn as any)(payload);
      } catch (err) {
        console.warn('[usePlayerSSE] subscriber threw', err);
      }
    }
  }

  es.addEventListener('open', () => setState('open'));
  es.addEventListener('error', () => {
    if (es.readyState === EventSource.CLOSED) setState('closed');
  });
  es.addEventListener('heartbeat', () => fan('onHeartbeat'));
  es.addEventListener('player.created', (e) => {
    const d = parsePayload(e as MessageEvent);
    if (d) fan('onCreated', d);
  });
  es.addEventListener('player.updated', (e) => {
    const d = parsePayload(e as MessageEvent);
    if (d) fan('onUpdated', d);
  });
  es.addEventListener('player.controls.updated', (e) => {
    const d = parsePayload(e as MessageEvent);
    if (d) fan('onControlsUpdated', d);
  });
  es.addEventListener('player.deleted', (e) => {
    const d = parsePayload(e as MessageEvent);
    if (d) fan('onDeleted', d);
  });

  perPlayerPool.set(pid, entry);
  return entry;
}

function releasePerPlayer(pid: string, handlers: Handlers, stateRef: Ref<ConnectionState>) {
  const entry = perPlayerPool.get(pid);
  if (!entry) return;
  entry.subscribers.delete(handlers);
  entry.stateRefs.delete(stateRef);
  if (entry.subscribers.size === 0) {
    try {
      entry.es.close();
    } catch {
      /* ignore */
    }
    perPlayerPool.delete(pid);
  }
}

/* ─── Public composable ─────────────────────────────────────────── */

export function usePlayerSSE(playerId: Ref<string | null | undefined>, handlers: Handlers) {
  const state = ref<ConnectionState>('connecting');

  // Active subscription handle. Tagged union so cleanup picks the
  // right release path.
  type Sub =
    | { kind: 'all'; pid: string; release: () => void }
    | { kind: 'per'; pid: string }
    | null;
  let sub: Sub = null;

  function attach(pid: string) {
    if (isAllPoolActive()) {
      // Filter events from the shared all-players socket by player_id.
      const filtered: AllPlayersSubscriber = {
        onCreated: (d) => {
          if (d?.id === pid) handlers.onCreated?.(d);
        },
        onUpdated: (d) => {
          if (d?.id === pid) handlers.onUpdated?.(d);
        },
        onControlsUpdated: (d) => {
          if (d?.id === pid) handlers.onControlsUpdated?.(d);
        },
        onDeleted: (d) => {
          if (d?.id === pid || d?.player_id === pid) handlers.onDeleted?.(d);
        },
        onHeartbeat: () => handlers.onHeartbeat?.(),
        onStateChange: (s) => { state.value = s; },
      };
      const handle = subscribeAllPlayers(filtered);
      sub = { kind: 'all', pid, release: handle.release };
    } else {
      const entry = getOrCreatePerPlayer(pid);
      entry.subscribers.add(handlers);
      entry.stateRefs.add(state);
      state.value = entry.state;
      sub = { kind: 'per', pid };
    }
  }

  function detach() {
    if (!sub) return;
    if (sub.kind === 'all') {
      sub.release();
    } else {
      releasePerPlayer(sub.pid, handlers, state);
    }
    sub = null;
    state.value = 'closed';
  }

  watch(
    playerId,
    (pid) => {
      detach();
      if (pid) attach(pid);
    },
    { immediate: true },
  );

  onScopeDispose(() => detach());

  // Backwards-compat ref kept for any consumer that read `.source`.
  // Always null now since the actual EventSource is in the pool.
  const source = ref<EventSource | null>(null);

  return { state, source };
}
