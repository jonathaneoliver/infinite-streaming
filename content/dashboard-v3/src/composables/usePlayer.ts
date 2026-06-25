/**
 * usePlayer(playerId) — the central per-player model composable.
 *
 * Holds canonical state in TanStack Query's cache (one source of
 * truth — no parallel Map). Hands the reactive Player record + a set
 * of typed mutation functions back to the view. Subscribes to SSE
 * and applies events via a revision cursor that:
 *
 *   - drops stale events (rev < cached rev),
 *   - merges metrics-only on same-rev (no clobber of control state),
 *   - full-applies strictly newer events,
 *   - special-cases `player.controls.updated` as definitively newest
 *     (matches the server's contract: it only fires on real control
 *     mutations).
 *
 * Mutations use useMutation with the standard optimistic-update +
 * onError rollback contract. The 412 path triggers invalidation so
 * the cache re-fetches authoritative state.
 *
 * View code stays trivial:
 *   const { player, setRate } = usePlayer(toRef(props, 'playerId'));
 *   <input :value="player?.shape?.rate_mbps" @input="setRate(+$event.target.value)" />
 *
 * No DOM reads at PATCH time. No pending-edit Maps. No
 * shouldSyncControls flicker. The framework owns it.
 */
import { computed, ref, watch, type Ref } from 'vue';
import { useQuery, useMutation, useQueryClient, type QueryClient } from '@tanstack/vue-query';
import * as repo from '@/repo/v2-repo';
import { isLivePlayerId } from '@/repo/v2-repo';
import type {
  PlayerRecord,
  Shape,
  Pattern,
  TransportFault,
  TransferTimeouts,
  ContentManipulation,
  FaultRule,
} from '@/repo/v2-repo';
import { usePlayerSSE } from './usePlayerSSE';
import { mergeMetricsOnly } from './mergeMetricsOnly';

type PlayerCacheValue = { player: PlayerRecord; etag?: string };

function playerKey(id: string) {
  return ['player', id] as const;
}

/** Pull the current etag from cache. Falls back to the player's
 *  control_revision when the cache entry lacks an explicit etag. */
function readEtag(qc: QueryClient, id: string): string | undefined {
  const entry = qc.getQueryData<PlayerCacheValue>(playerKey(id));
  return entry?.etag ?? entry?.player?.control_revision ?? undefined;
}

/** Compare two control_revisions for ordering. Both fields are
 *  monotonic strings; the suffix-form (`...Z-NNNN`) tail-sorts after
 *  the bare timestamp at the same instant. Lexicographic compare is
 *  correct for both forms. */
function revGreater(a: string | undefined, b: string | undefined): boolean {
  if (!a) return false;
  if (!b) return true;
  return a > b;
}

export function usePlayer(playerId: Ref<string>) {
  const qc = useQueryClient();

  /* ─── Connection state (forward declaration) ───────────────────── */
  // SSE is wired further down, but the query above needs to read its
  // connection state to drive the polling-fallback `refetchInterval`.
  // Declaring the ref here and mirroring `sse.state` into it below
  // avoids reordering the whole composable.
  type ConnState = 'connecting' | 'open' | 'closed';
  const sseState = ref<ConnState>('connecting');

  /* ─── Server cache ──────────────────────────────────────────────── */

  const query = useQuery<PlayerCacheValue>({
    queryKey: computed(() => playerKey(playerId.value)),
    queryFn: async () => repo.getPlayer(playerId.value),
    enabled: computed(() => !!playerId.value),
    // Stale-while-revalidate is fine — SSE keeps it fresh.
    staleTime: 30_000,
    // Polling fallback: when SSE is closed (server-side drop, network
    // partition, transient 5xx) the query polls every 5s so the model
    // doesn't sit on a stale snapshot indefinitely. As soon as SSE
    // reconnects (state → 'open') the watcher below toggles refetching
    // back off — SSE is the authoritative live feed. Also bail out when
    // the cached state is a permanent 4xx — no point polling for a
    // player_id the server has already said is malformed/missing.
    refetchInterval: (q: any) => {
      // Archive playerIds (v3 session-viewer) are static historical
      // data — no point polling. The SSE chain is also skipped for
      // these (see usePlayerSSE.ts); polling would just re-fire the
      // chart watcher with the same archived record every 5s.
      if (!isLivePlayerId(playerId.value)) return false;
      const errStatus = (q?.state?.error as any)?.status;
      if (typeof errStatus === 'number' && errStatus >= 400 && errStatus < 500) return false;
      return sseState.value === 'closed' ? 5_000 : false;
    },
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: (q: any) => {
      const errStatus = (q?.state?.error as any)?.status;
      return !(typeof errStatus === 'number' && errStatus >= 400 && errStatus < 500);
    },
    // Same 4xx guard for the mount-time refetch. Without this, every
    // component mounting usePlayer (VideoPlayerFrame, FaultRules,
    // SessionDetails, ...) re-fires the GET against a player_id the
    // server has already 404'd — multiplying the noise by N panels.
    refetchOnMount: (q: any) => {
      const errStatus = (q?.state?.error as any)?.status;
      return !(typeof errStatus === 'number' && errStatus >= 400 && errStatus < 500);
    },
    refetchOnReconnect: (q: any) => {
      const errStatus = (q?.state?.error as any)?.status;
      return !(typeof errStatus === 'number' && errStatus >= 400 && errStatus < 500);
    },
    // 4xx is permanent (bad UUID, missing player, etc.) — don't retry.
    // Other failures get one retry.
    retry: (failureCount, error: any) => {
      const s = error?.status;
      if (typeof s === 'number' && s >= 400 && s < 500) return false;
      return failureCount < 1;
    },
  });

  const player = computed<PlayerRecord | null>(() => query.data.value?.player ?? null);
  const controlRevision = computed(() => player.value?.control_revision);

  /* ─── SSE ingest with revision cursor ──────────────────────────── */

  function applyAuthoritative(incoming: PlayerRecord) {
    qc.setQueryData<PlayerCacheValue>(playerKey(playerId.value), {
      player: incoming,
      etag: incoming.control_revision,
    });
  }

  function applyMetricsTick(incoming: PlayerRecord) {
    const cur = qc.getQueryData<PlayerCacheValue>(playerKey(playerId.value));
    if (!cur) {
      applyAuthoritative(incoming);
      return;
    }
    const merged = mergeMetricsOnly(cur.player, incoming);
    qc.setQueryData<PlayerCacheValue>(playerKey(playerId.value), {
      player: merged,
      etag: cur.etag, // metrics tick doesn't change the control etag
    });
  }

  function ingest(kind: 'controls' | 'updated' | 'created', incoming: PlayerRecord) {
    const cur = qc.getQueryData<PlayerCacheValue>(playerKey(playerId.value));
    const curRev = cur?.player?.control_revision;
    const inRev = incoming.control_revision;
    // `controls.updated` is always authoritative — server fires it on
    // an actual control mutation, with the new control_revision in tow.
    if (kind === 'controls' || kind === 'created') {
      applyAuthoritative(incoming);
      return;
    }
    // `updated` is the metrics tick. Same rev → metrics-only merge.
    // Strictly newer rev → there was a control mutation we hadn't yet
    // received via controls.updated; adopt full state. Older → drop.
    if (revGreater(inRev, curRev)) {
      applyAuthoritative(incoming);
    } else if (inRev === curRev) {
      applyMetricsTick(incoming);
    } else {
      // stale; drop
    }
  }

  // SSE subscribes to a CANONICAL player_id — once getPlayer resolves
  // the canonical casing (via the case-insensitive lookup in v2-repo)
  // we switch the SSE filter to that, so events flow even when the
  // user pasted a URL with the wrong case.
  //
  // For `archive:` ids (v3 session-viewer) we MUST NOT do the canonical
  // swap: the cached PlayerRecord's `id` field is the original live
  // player UUID, NOT the synthetic archive id. Swapping would subscribe
  // SSE to that live UUID and pour current-time events into the chart,
  // which then trims away the archived history on every push.
  const sseId = ref<string>(playerId.value);
  watch(playerId, (v) => { sseId.value = v; });
  watch(
    () => player.value?.id,
    (canonicalId) => {
      if (!isLivePlayerId(playerId.value)) return;
      if (canonicalId && canonicalId !== sseId.value) sseId.value = canonicalId;
    },
  );
  const sse = usePlayerSSE(sseId, {
    onCreated: (d) => ingest('created', d),
    onUpdated: (d) => ingest('updated', d),
    onControlsUpdated: (d) => ingest('controls', d),
    onDeleted: () => qc.removeQueries({ queryKey: playerKey(playerId.value) }),
  });
  // Mirror the SSE connection state into our forward-declared ref so
  // the query's refetchInterval can react to it.
  watch(() => sse.state.value, (s) => { sseState.value = s; }, { immediate: true });

  /* ─── Mutations ─────────────────────────────────────────────────── */

  /**
   * Generic top-level merge-patch mutation. Updates a typed top-level
   * group (shape / transfer_timeouts / content / labels) with optimistic
   * cache update + 412 rollback. All five group setters below funnel
   * through this — keeps the optimistic-rollback plumbing in one place.
   */
  function makeGroupMutation<K extends 'shape' | 'transfer_timeouts' | 'content' | 'labels'>(
    key: K,
  ) {
    type PartialGroup = Partial<NonNullable<PlayerRecord[K]>>;
    return useMutation({
      mutationFn: (partial: PartialGroup) =>
        repo.patchPlayer(playerId.value, { [key]: partial } as any, readEtag(qc, playerId.value)),
      onMutate: async (partial) => {
        await qc.cancelQueries({ queryKey: playerKey(playerId.value) });
        const prev = qc.getQueryData<PlayerCacheValue>(playerKey(playerId.value));
        if (prev) {
          const nextGroup = { ...(prev.player[key] as any), ...(partial as any) };
          qc.setQueryData<PlayerCacheValue>(playerKey(playerId.value), {
            ...prev,
            player: { ...prev.player, [key]: nextGroup } as PlayerRecord,
          });
        }
        return { prev };
      },
      onError: (err: any, _vars, ctx) => {
        if (ctx?.prev) {
          qc.setQueryData(playerKey(playerId.value), ctx.prev);
        }
        // 404 = player is gone; don't bother refetching (would 404 again).
        // 412 = conflict; refetch authoritative state.
        // Other errors (5xx, network) — same refetch path is safe.
        if (err?.status !== 404) {
          qc.invalidateQueries({ queryKey: playerKey(playerId.value) });
        }
      },
      onSuccess: ({ player: fresh, etag }) => {
        qc.setQueryData<PlayerCacheValue>(playerKey(playerId.value), {
          player: fresh,
          etag: etag ?? fresh.control_revision,
        });
      },
    });
  }

  const patchShape = makeGroupMutation('shape');
  const patchTransferTimeouts = makeGroupMutation('transfer_timeouts');
  const patchContent = makeGroupMutation('content');
  const patchLabels = makeGroupMutation('labels');

  /* ─── Fault rules (per-rule sub-resource) ──────────────────────── */

  const mutateFaultRule = useMutation({
    mutationFn: (rule: FaultRule) =>
      repo.upsertFaultRule(playerId.value, rule, readEtag(qc, playerId.value)),
    onMutate: async (rule) => {
      await qc.cancelQueries({ queryKey: playerKey(playerId.value) });
      const prev = qc.getQueryData<PlayerCacheValue>(playerKey(playerId.value));
      if (prev) {
        const rules = [...(prev.player.fault_rules ?? [])];
        const idx = rule.id ? rules.findIndex((r) => r.id === rule.id) : -1;
        if (idx >= 0) rules[idx] = { ...rules[idx], ...rule };
        else rules.push(rule);
        qc.setQueryData<PlayerCacheValue>(playerKey(playerId.value), {
          ...prev,
          player: { ...prev.player, fault_rules: rules },
        });
      }
      return { prev };
    },
    onError: (err: any, _vars, ctx) => {
      if (ctx?.prev) qc.setQueryData(playerKey(playerId.value), ctx.prev);
      if (err?.status !== 404) {
        qc.invalidateQueries({ queryKey: playerKey(playerId.value) });
      }
    },
    onSuccess: () => {
      // The PATCH response is just the rule; refetch the player so we
      // get the recomputed full fault_rules array + new control_revision.
      qc.invalidateQueries({ queryKey: playerKey(playerId.value) });
    },
  });

  const removeFaultRuleMutation = useMutation({
    mutationFn: (ruleId: string) =>
      repo.deleteFaultRule(playerId.value, ruleId, readEtag(qc, playerId.value)),
    onMutate: async (ruleId) => {
      await qc.cancelQueries({ queryKey: playerKey(playerId.value) });
      const prev = qc.getQueryData<PlayerCacheValue>(playerKey(playerId.value));
      if (prev) {
        qc.setQueryData<PlayerCacheValue>(playerKey(playerId.value), {
          ...prev,
          player: {
            ...prev.player,
            fault_rules: (prev.player.fault_rules ?? []).filter((r) => r.id !== ruleId),
          },
        });
      }
      return { prev };
    },
    onError: (err: any, _vars, ctx) => {
      if (ctx?.prev) qc.setQueryData(playerKey(playerId.value), ctx.prev);
      if (err?.status !== 404) {
        qc.invalidateQueries({ queryKey: playerKey(playerId.value) });
      }
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: playerKey(playerId.value) }),
  });

  /* ─── Public surface ────────────────────────────────────────────── */

  return {
    player,
    controlRevision,
    isLoading: query.isLoading,
    isError: query.isError,
    error: query.error,
    sseState: sse.state,

    // Shape (rate / delay / loss / pattern / transport_fault)
    setShape: (partial: Partial<Shape>) => patchShape.mutate(partial),
    // setRate disarms any active throughput pattern — the rate slider and
    // the pattern are mutually exclusive sources-of-truth for the kernel
    // cap. Delay and loss are orthogonal axes that can coexist with a
    // running pattern, so setDelay / setLoss DON'T touch pattern.
    setRate: (rate_mbps: number) => patchShape.mutate({ rate_mbps, pattern: null as any }),
    setDelay: (delay_ms: number) => patchShape.mutate({ delay_ms }),
    setLoss: (loss_pct: number) => patchShape.mutate({ loss_pct }),
    // #826 link-impairment knobs — orthogonal axes that coexist with a pattern,
    // so (like delay/loss) they DON'T disarm it.
    setJitter: (jitter_ms: number) => patchShape.mutate({ jitter_ms }),
    setLossCorrelation: (loss_correlation_pct: number) =>
      patchShape.mutate({ loss_correlation_pct }),
    setJitterCorrelation: (jitter_correlation_pct: number) =>
      patchShape.mutate({ jitter_correlation_pct }),
    // applyProfile sets the whole impairment block at once from a named link
    // profile (clean / home / mobile-poor / nlc-*). Profiles with no rate_mbps
    // are pure overlays — they leave the throughput cap untouched.
    applyProfile: (p: Partial<Shape>) => patchShape.mutate(p),
    setPattern: (pattern: Pattern | null) => patchShape.mutate({ pattern: pattern as any }),
    setTransportFault: (transport_fault: TransportFault) =>
      patchShape.mutate({ transport_fault }),

    // Transfer timeouts
    setTransferTimeouts: (p: Partial<TransferTimeouts>) => patchTransferTimeouts.mutate(p),

    // Content manipulation
    setContent: (p: Partial<ContentManipulation>) => patchContent.mutate(p),

    // Labels
    setLabel: (k: string, v: string) => patchLabels.mutate({ [k]: v }),
    clearLabel: (k: string) => patchLabels.mutate({ [k]: null as any }),

    // Fault rules
    upsertFaultRule: (rule: FaultRule) => mutateFaultRule.mutate(rule),
    removeFaultRule: (id: string) => removeFaultRuleMutation.mutate(id),

    // Pending flags (drive disabled-input UI / spinners)
    isShapeWriting: computed(() => patchShape.isPending.value),
    isFaultRuleWriting: computed(
      () => mutateFaultRule.isPending.value || removeFaultRuleMutation.isPending.value,
    ),
  };
}
