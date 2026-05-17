/**
 * usePlayers() — list of every connected player. Used by the picker
 * (testing.html) to render the session cards.
 *
 * Wires:
 *   - useQuery(['players']) backed by GET /api/v2/players?include=raw
 *   - A single SSE EventSource on /api/v2/events?include=raw (no
 *     player_id filter — cross-cutting feed for created/deleted)
 *   - The same revision-cursor ingest logic as usePlayer, applied
 *     per-player into the players list cache
 *
 * Each individual player's per-id cache (used by usePlayer) is also
 * kept in sync so a picker → drill-down doesn't need to refetch.
 */
import { computed, onScopeDispose, ref } from 'vue';
import { useQuery, useQueryClient } from '@tanstack/vue-query';
import * as repo from '@/repo/v2-repo';
import type { PlayerRecord } from '@/repo/v2-repo';
import { mergeMetricsOnly } from './mergeMetricsOnly';
import { subscribeAllPlayers, type ConnectionState } from './ssePool';

function listKey() {
  return ['players'] as const;
}
function playerKey(id: string) {
  return ['player', id] as const;
}

function revGreater(a: string | undefined, b: string | undefined): boolean {
  if (!a) return false;
  if (!b) return true;
  return a > b;
}

export function usePlayers() {
  const qc = useQueryClient();
  const sseState = ref<ConnectionState>('connecting');

  const query = useQuery<PlayerRecord[]>({
    queryKey: listKey(),
    queryFn: () => repo.listPlayers(),
    staleTime: 30_000,
    refetchOnWindowFocus: true,
  });

  const players = computed<PlayerRecord[]>(() => query.data.value ?? []);

  /* ─── Per-player ingest helpers ────────────────────────────────── */

  function upsertInList(incoming: PlayerRecord, mergeMode: 'authoritative' | 'metrics-only') {
    const list = qc.getQueryData<PlayerRecord[]>(listKey()) ?? [];
    const idx = list.findIndex((p) => p.id === incoming.id);
    let next: PlayerRecord[];
    if (idx < 0) {
      next = [...list, incoming];
    } else if (mergeMode === 'authoritative') {
      next = list.slice();
      next[idx] = incoming;
    } else {
      next = list.slice();
      next[idx] = mergeMetricsOnly(list[idx], incoming);
    }
    qc.setQueryData<PlayerRecord[]>(listKey(), next);

    // Mirror into the per-player cache so usePlayer(id) sees the same.
    const perPlayer = qc.getQueryData<{ player: PlayerRecord; etag?: string }>(
      playerKey(incoming.id),
    );
    if (mergeMode === 'authoritative' || !perPlayer) {
      qc.setQueryData(playerKey(incoming.id), {
        player: incoming,
        etag: incoming.control_revision,
      });
    } else {
      qc.setQueryData(playerKey(incoming.id), {
        player: mergeMetricsOnly(perPlayer.player, incoming),
        etag: perPlayer.etag,
      });
    }
  }

  function ingest(kind: 'controls' | 'updated' | 'created', incoming: PlayerRecord) {
    if (kind === 'controls' || kind === 'created') {
      upsertInList(incoming, 'authoritative');
      return;
    }
    // updated = metrics tick by default
    const list = qc.getQueryData<PlayerRecord[]>(listKey()) ?? [];
    const cur = list.find((p) => p.id === incoming.id);
    if (revGreater(incoming.control_revision, cur?.control_revision)) {
      upsertInList(incoming, 'authoritative');
    } else if (incoming.control_revision === cur?.control_revision) {
      upsertInList(incoming, 'metrics-only');
    } else {
      // stale, drop
    }
  }

  function removeFromList(id: string) {
    const list = qc.getQueryData<PlayerRecord[]>(listKey()) ?? [];
    qc.setQueryData<PlayerRecord[]>(listKey(), list.filter((p) => p.id !== id));
    qc.removeQueries({ queryKey: playerKey(id) });
  }

  /* ─── SSE subscription ──────────────────────────────────────────── */
  // Pooled cross-page singleton — for the same reason usePlayerSSE
  // pools per-player_id sockets: Chrome caps connections per origin at
  // 6 and the loading spinner spins forever while any EventSource is
  // open. The picker page mounts usePlayers() exactly once today, but
  // making it pool-safe means future callers can't accidentally
  // multiply sockets.
  const sub = subscribeAllPlayers({
    onCreated: (d) => ingest('created', d),
    onUpdated: (d) => ingest('updated', d),
    onControlsUpdated: (d) => ingest('controls', d),
    onDeleted: (d: any) => {
      const id = d?.player_id || d?.id;
      if (id) removeFromList(id);
    },
    onStateChange: (s) => { sseState.value = s; },
  });

  onScopeDispose(() => sub.release());

  return {
    players,
    isLoading: query.isLoading,
    isError: query.isError,
    error: query.error,
    sseState,
    refetch: () => query.refetch(),
    createPlayer: (body: Partial<PlayerRecord> = {}) => repo.createPlayer(body),
    deletePlayer: (id: string) => repo.deletePlayer(id),
  };
}
