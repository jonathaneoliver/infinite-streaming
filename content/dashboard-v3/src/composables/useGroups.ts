/**
 * useGroups() — player-group affiliations. Picker uses this to show
 * the group banner + provide link/unlink/disband actions.
 *
 * Groups don't have their own SSE event channel today; the player
 * SSE pipeline emits group_id on each PlayerRecord, so the group
 * list naturally invalidates whenever players are updated. For now
 * we use a polling fallback (~5s) and let usePlayers' SSE invalidate.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query';
import * as repo from '@/repo/v2-repo';

function key() {
  return ['groups'] as const;
}

export function useGroups() {
  const qc = useQueryClient();

  const query = useQuery({
    queryKey: key(),
    queryFn: () => repo.listGroups(),
    staleTime: 5_000,
    refetchInterval: 5_000,
    refetchOnWindowFocus: true,
  });

  // Surface mutation failures — without an onError handler the
  // TanStack Query mutation just logs to its internal state and the
  // UI never moves, which is exactly how the earlier
  // `{player_ids: ...}` POST regression slipped through.
  function reportMutationError(action: string, err: unknown) {
    const msg = (err as any)?.message ?? String(err);
    console.error(`[useGroups] ${action} failed`, err);
    if (typeof window !== 'undefined') {
      window.alert(`${action} failed: ${msg}`);
    }
  }

  const link = useMutation({
    mutationFn: (playerIds: string[]) => repo.linkGroup(playerIds),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: key() });
      qc.invalidateQueries({ queryKey: ['players'] });
    },
    onError: (err) => reportMutationError('Group sessions', err),
  });

  const disband = useMutation({
    mutationFn: (groupId: string) => repo.disbandGroup(groupId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: key() });
      qc.invalidateQueries({ queryKey: ['players'] });
    },
    onError: (err) => reportMutationError('Disband group', err),
  });

  /** Update a group's membership wholesale. Caller provides the
   *  desired final list; handler diffs against the current set. */
  const updateMembers = useMutation({
    mutationFn: (args: { groupId: string; memberPlayerIds: string[]; ifMatch: string }) =>
      repo.updateGroupMembers(args.groupId, args.memberPlayerIds, args.ifMatch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: key() });
      qc.invalidateQueries({ queryKey: ['players'] });
    },
    onError: (err) => reportMutationError('Update group membership', err),
  });

  return {
    groups: query.data,
    isLoading: query.isLoading,
    link: (playerIds: string[]) => link.mutate(playerIds),
    disband: (groupId: string) => disband.mutate(groupId),
    updateMembers: (groupId: string, memberPlayerIds: string[], ifMatch: string) =>
      updateMembers.mutate({ groupId, memberPlayerIds, ifMatch }),
  };
}
