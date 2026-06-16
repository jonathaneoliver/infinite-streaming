/**
 * useGroupSiblings(playerId) — the OTHER members of the active player's
 * group, for the compare-charts overlay (issue #579).
 *
 * Resolves the active player's group via membership (the same v5-derived
 * group id reasoning as GroupBanner: g.id never equals the raw v1
 * group_id, so we match on member_player_ids.includes(self)), then
 * returns every member that isn't `self`, each with a stable display
 * label and a palette index so overlay colours stay put as the brush
 * pans or peers come and go.
 *
 * Empty unless the active player is in a group with ≥2 members. Reuses
 * useGroups() (polling) + usePlayers() (SSE-invalidated) so the sibling
 * list tracks link/unlink without its own subscription.
 */
import { computed, isRef, ref, type Ref } from 'vue';
import { useGroups } from '@/composables/useGroups';
import { usePlayers } from '@/composables/usePlayers';
import { deviceFromUA } from '@/composables/useSessionLabels';

export interface GroupSibling {
  /** Canonical player UUID of the sibling. */
  playerId: string;
  /** Short human label — `#<display_id> <device>` when known, else a
   *  truncated UUID. Used in the per-member legend group name + tooltips. */
  label: string;
  /** Short tag for legend series, e.g. `S3` from display_id 3, falling
   *  back to the first UUID octet. Kept compact so the chart legend
   *  stays readable with several overlaid sessions. */
  tag: string;
  /** Stable 0-based index among the siblings (sorted by player id) so a
   *  given sibling keeps the same overlay colour across renders. */
  index: number;
}

export function useGroupSiblings(playerIdInput: string | Ref<string>) {
  const playerIdRef: Ref<string> = isRef(playerIdInput)
    ? playerIdInput
    : ref(playerIdInput);
  const { groups } = useGroups();
  const { players } = usePlayers();

  const groupInfo = computed(() => {
    const self = playerIdRef.value;
    return (
      (groups.value ?? []).find(
        (g) =>
          Array.isArray(g.member_player_ids) && g.member_player_ids.includes(self),
      ) ?? null
    );
  });

  const siblings = computed<GroupSibling[]>(() => {
    const g = groupInfo.value;
    if (!g || !Array.isArray(g.member_player_ids)) return [];
    // Only meaningful as a comparison when the group has ≥2 members
    // (self + at least one peer).
    if (g.member_player_ids.length < 2) return [];
    const self = playerIdRef.value;
    // Sort for a stable colour assignment regardless of membership
    // emission order.
    const otherIds = g.member_player_ids
      .filter((id) => id !== self)
      .slice()
      .sort();
    return otherIds.map((id, index) => {
      const p = (players.value ?? []).find((x) => x.id === id);
      const displayId = p?.display_id;
      const tag = displayId != null ? `S${displayId}` : `S${id.slice(0, 4)}`;
      const num = displayId != null ? `#${displayId}` : id.slice(0, 8);
      const device = deviceFromUA(p?.user_agent ?? null);
      const label = device ? `${num} ${device}` : num;
      return { playerId: id, label, tag, index };
    });
  });

  return { siblings, groupInfo };
}
