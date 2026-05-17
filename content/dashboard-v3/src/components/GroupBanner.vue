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
import * as repo from '@/repo/v2-repo';

const props = defineProps<{ playerId: string }>();
const { player } = usePlayer(toRef(props, 'playerId'));
const { groups, link, disband } = useGroups();
const { players } = usePlayers();

const groupId = computed<string | null>(() => {
  const raw = (player.value as any)?.raw_session;
  const gid = raw?.group_id;
  return typeof gid === 'string' && gid.length ? gid : null;
});

const groupInfo = computed(() => {
  if (!groupId.value) return null;
  return (groups.value ?? []).find((g) => g.id === groupId.value) ?? null;
});

const memberNames = computed(() => {
  const ids = groupInfo.value?.member_player_ids ?? [];
  return ids
    .filter((id) => id !== props.playerId)
    .map((id) => {
      const p = (players.value ?? []).find((x) => x.id === id);
      return p?.display_id ? `#${p.display_id}` : id.slice(0, 8);
    });
});

const candidates = computed(() => {
  return (players.value ?? [])
    .filter((p) => p.id !== props.playerId)
    .filter((p) => {
      const raw = (p as any).raw_session;
      const otherGid = raw?.group_id;
      return !otherGid || otherGid === groupId.value;
    });
});

import { ref } from 'vue';
const groupWith = ref<string>('');

function linkSelected() {
  if (!groupWith.value) return;
  link([props.playerId, groupWith.value]);
  groupWith.value = '';
}

function ungroup() {
  if (groupId.value) disband(groupId.value);
}

async function deleteSession() {
  if (!confirm(`Release session ${props.playerId}?`)) return;
  try {
    await repo.deletePlayer(props.playerId);
    window.location.href = '/dashboard/v3/testing.html';
  } catch (err: any) {
    console.error('release failed', err);
    alert(`Release failed: ${err?.message ?? err}`);
  }
}
</script>

<template>
  <div v-if="player" class="group-controls-row">
    <div class="row">
      <template v-if="groupId">
        <span class="banner grouped">
          🔗 Grouped with: <strong>{{ memberNames.join(', ') || '(only you)' }}</strong>
          <span class="chip">{{ groupInfo?.label || groupId.slice(0, 8) }}</span>
        </span>
        <button type="button" class="btn btn-warn" @click="ungroup">Ungroup</button>
      </template>
      <template v-else>
        <label class="link-form">
          Group with
          <select v-model="groupWith">
            <option value="">— select session —</option>
            <option v-for="p in candidates" :key="p.id" :value="p.id">
              #{{ p.display_id }} {{ p.player_ip || p.origination_ip || '' }}
            </option>
          </select>
        </label>
        <button type="button" class="btn btn-primary" :disabled="!groupWith" @click="linkSelected">
          Group
        </button>
        <span v-if="!candidates.length" class="hint">No other sessions available to group with.</span>
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

.link-form {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: #5f6368;
}
.link-form select {
  background: #fff;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 4px 8px;
  font-size: 13px;
  min-width: 220px;
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
.btn-primary { background: #1a73e8; color: white; border-color: #1a73e8; }
.btn-primary:hover { background: #1765cc; }
.btn-warn { background: #ef4444; color: white; border-color: #ef4444; font-size: 11px; padding: 4px 10px; }
.btn-warn:hover { background: #dc2626; }
.btn-danger { background: #fee2e2; color: #991b1b; border-color: #fca5a5; }
.btn-danger:hover { background: #fecaca; }

.hint { color: #9aa0a6; font-size: 12px; font-style: italic; }
.ml-auto { margin-left: auto; }
</style>
