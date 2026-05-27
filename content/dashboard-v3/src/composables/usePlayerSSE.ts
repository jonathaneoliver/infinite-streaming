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
import { isLivePlayerId } from '@/repo/v2-repo';
import {
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

/* ─── Public composable ─────────────────────────────────────────── */

export function usePlayerSSE(playerId: Ref<string | null | undefined>, handlers: Handlers) {
  const state = ref<ConnectionState>('connecting');

  // Active subscription handle — always the all-pool path now.
  type Sub = { pid: string; release: () => void } | null;
  let sub: Sub = null;

  function attach(pid: string) {
    // Always go through the shared all-players SSE — opening a per-
    // player SSE in parallel doubles the EventSource count per page,
    // which (under HTTP/1.1's 6-per-origin cap) starves the second
    // browser tab's REST polls. The all-pool socket is server-side
    // unfiltered; we filter client-side by player_id.
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
    sub = { pid, release: handle.release };
  }

  function detach() {
    if (!sub) return;
    sub.release();
    sub = null;
    state.value = 'closed';
  }

  watch(
    playerId,
    (pid) => {
      detach();
      // Skip SSE entirely for archive playerIds. The v3 session-viewer
      // uses synthetic ids like `archive:<sid>:<pid>` to reuse the
      // live-mode usePlayer chain over historical data. Subscribing
      // those to /api/v2/events would either 404 (no live player) or
      // — worse — feed unrelated live events into the chart's
      // pushSample loop, where the current-time `event_time` on those
      // events triggers the trim cutoff and wipes the archived history.
      if (pid && !isLivePlayerId(pid)) {
        state.value = 'closed';
        return;
      }
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
