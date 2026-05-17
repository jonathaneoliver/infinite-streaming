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

  const link = useMutation({
    mutationFn: (playerIds: string[]) => repo.linkGroup(playerIds),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: key() });
      qc.invalidateQueries({ queryKey: ['players'] });
    },
  });

  const disband = useMutation({
    mutationFn: (groupId: string) => repo.disbandGroup(groupId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: key() });
      qc.invalidateQueries({ queryKey: ['players'] });
    },
  });

  return {
    groups: query.data,
    isLoading: query.isLoading,
    link: (playerIds: string[]) => link.mutate(playerIds),
    disband: (groupId: string) => disband.mutate(groupId),
  };
}
