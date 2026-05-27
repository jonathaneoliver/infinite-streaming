<script setup lang="ts">
/**
 * GroupBanner.vue — group affiliation strip above the panels. Matches
 * the legacy `#sessionGroupControls`:
 *   - "Grouped with: …" when in a group, with an Ungroup button
 *   - "Group with: <select>" + Group button when not in a group,
 *     listing every other live session as a candidate
 *   - Right-aligned "Delete Session" (danger) button
 *
 * Group membership lives on the v1 session passthrough — the v2 spec
 * doesn't yet have a typed `group_id` on PlayerRecord, so we read it
 * from raw_session for now.
 */
import { computed, toRef } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import { useGroups } from '@/composables/useGroups';
import { usePlayers } from '@/composables/usePlayers';
import { deviceFromUA, groupNameFor } from '@/composables/useSessionLabels';
import * as repo from '@/repo/v2-repo';

const props = defineProps<{ playerId: string }>();
const { player } = usePlayer(toRef(props, 'playerId'));
const { groups, disband, updateMembers } = useGroups();
const { players } = usePlayers();

// Identify the active player's group via membership rather than by
// matching raw_session.group_id against PlayerGroup.id. The server
// derives `g.id` as a stable v5 UUID under a fixed namespace
// (v2translate.StableGroupUUID), so g.id never equals the raw v1
// group_id string — comparing them gave a null groupInfo and the
// banner fell through to "(only you)" even when peers were present.
const groupInfo = computed(() => {
  return (
    (groups.value ?? []).find((g) =>
      Array.isArray(g.member_player_ids) && g.member_player_ids.includes(props.playerId),
    ) ?? null
  );
});

const groupId = computed<string | null>(() => {
  // Surface the v2 PlayerGroup id (v5 derivation) for the "Ungroup"
  // call + chip label. raw_session.group_id is the v1 tag; we don't
  // need it directly now that membership drives groupInfo.
  return groupInfo.value ? String(groupInfo.value.id) : null;
});

const memberNames = computed(() => {
  const ids = groupInfo.value?.member_player_ids ?? [];
  return ids
    .filter((id) => id !== props.playerId)
    .map((id) => {
      const p = (players.value ?? []).find((x) => x.id === id);
      if (!p) return id.slice(0, 8);
      const num = p.display_id != null ? `#${p.display_id}` : id.slice(0, 8);
      const device = deviceFromUA(p.user_agent ?? null);
      return device ? `${num} ${device}` : num;
    });
});

/** Group name shared with the pill badge in Testing.vue. */
const groupName = computed<string>(() => {
  if (!groupInfo.value) return '';
  return groupNameFor(
    {
      id: String(groupInfo.value.id),
      member_player_ids: groupInfo.value.member_player_ids,
    },
    players.value ?? [],
  );
});

/** Member count drives the Leave vs Disband split.
 *  - 2 members: only "Ungroup" makes sense (the surviving 1 is a
 *    degenerate group, so leaving == disbanding).
 *  - 3+ members: surface both Leave (only this session exits) and
 *    Disband (drop the whole group). */
const memberCount = computed<number>(
  () => groupInfo.value?.member_player_ids?.length ?? 0,
);

function disbandGroup() {
  if (groupId.value) disband(groupId.value);
}

function leaveGroup() {
  const g = groupInfo.value;
  if (!g) return;
  const rev = g.control_revision;
  if (!rev) {
    // No revision means the listGroups poll hasn't caught up to the
    // most recent membership write; ask the user to retry rather
    // than racing the PATCH without an If-Match.
    window.alert('Group revision not yet known — try again in a moment.');
    return;
  }
  const remaining = (g.member_player_ids ?? []).filter((id) => id !== props.playerId);
  updateMembers(String(g.id), remaining, rev);
}

async function deleteSession() {
  if (!confirm(`Release session ${props.playerId}?`)) return;
  try {
    await repo.deletePlayer(props.playerId);
    window.location.href = '/dashboard/testing.html';
  } catch (err: any) {
    console.error('release failed', err);
    alert(`Release failed: ${err?.message ?? err}`);
  }
}
</script>

<template>
  <!-- Group banner now exists only to surface grouped state + Delete
       Session. Linking happens in the pill rail (Testing.vue's
       Group(N) trigger), so the "Group with…" select form is gone. -->
  <div v-if="player" class="group-controls-row">
    <div class="row">
      <template v-if="groupId">
        <span class="banner grouped">
          🔗 Grouped with: <strong>{{ memberNames.join(', ') || '(only you)' }}</strong>
          <span class="chip">{{ groupName }}</span>
        </span>
        <!-- "Ungroup" = this session leaves; "Delete Group" = drop the
             whole group. With only 2 members, leaving leaves a
             degenerate 1-member group, so we surface only "Delete
             Group" in that case. -->
        <button
          v-if="memberCount >= 3"
          type="button"
          class="btn btn-secondary"
          @click="leaveGroup"
        >
          Ungroup
        </button>
        <button type="button" class="btn btn-warn" @click="disbandGroup">
          Delete Group
        </button>
      </template>

      <button type="button" class="btn btn-danger ml-auto" @click="deleteSession">
        Delete Session
      </button>
    </div>
  </div>
</template>

<style scoped>
.group-controls-row {
  background: #f8fafc;
  border: 1px solid #e5e7eb;
  border-radius: 10px;
  padding: 8px;
  margin-bottom: 12px;
}
.row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  font-size: 13px;
}
.banner {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  background: #ecfdf3;
  border: 1px solid #bbf7d0;
  color: #166534;
  padding: 6px 10px;
  border-radius: 8px;
  font-weight: 600;
}
.banner.grouped strong { font-weight: 700; }
.chip {
  background: #d1fae5;
  color: #064e3b;
  font-family: ui-monospace, monospace;
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 10px;
  font-weight: 500;
}

.btn {
  background: #f1f3f4;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 6px 12px;
  font-size: 12px;
  font-weight: 500;
  color: #202124;
  cursor: pointer;
}
.btn:hover { background: #e8eaed; }
.btn:disabled { opacity: 0.5; cursor: not-allowed; }
/* "Ungroup" = neutral, this session leaves; visually muted so it
 * doesn't read as destructive next to "Delete Group". */
.btn-secondary { background: #e5e7eb; color: #1f2937; border-color: #d1d5db; font-size: 11px; padding: 4px 10px; }
.btn-secondary:hover { background: #d1d5db; }
.btn-warn { background: #ef4444; color: white; border-color: #ef4444; font-size: 11px; padding: 4px 10px; }
.btn-warn:hover { background: #dc2626; }
.btn-danger { background: #fee2e2; color: #991b1b; border-color: #fca5a5; }
.btn-danger:hover { background: #fecaca; }

.ml-auto { margin-left: auto; }
</style>
