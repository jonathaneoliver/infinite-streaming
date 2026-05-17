<script setup lang="ts">
/**
 * Testing.vue — multi-session monitor. Mirrors the legacy
 * `testing.html` page model:
 *
 *   - Toolbar: "Active Sessions" title · SSE-missed badge ·
 *     Refresh · Release All Sessions
 *   - Group-controls bar (when ≥2 sessions): "Group Selected Sessions"
 *     button + "Select 2+ sessions to group" status text
 *   - Session-tab pill rail: one pill per session, with a checkbox for
 *     group selection. Clicking a pill makes that session active.
 *   - Active session's full control + stats panel stack underneath
 *     (same components used on the per-session page, no embedded video
 *     player — that's only on testing-session.html).
 *
 * Group-checkbox sync rules (match legacy applyTabSelection):
 *   - Checking a pill that belongs to a group also auto-checks every
 *     sibling member, so grouping operations always touch the whole
 *     group.
 *   - Unchecking a grouped pill is treated as a request to *unlink*
 *     that member from the group (group composition mutation, not
 *     just selection state).
 *   - Plain-session pills toggle selection only.
 */
import { computed, ref, watch } from 'vue';
import { usePlayers } from '@/composables/usePlayers';
import { useGroups } from '@/composables/useGroups';
import { deviceContentLabel, groupNameFor } from '@/composables/useSessionLabels';
import type { PlayerRecord } from '@/repo/v2-repo';
import * as repo from '@/repo/v2-repo';
import ShapeSliders from '@/components/ShapeSliders.vue';
import NetworkShapingPattern from '@/components/NetworkShapingPattern.vue';
import TransferTimeouts from '@/components/TransferTimeouts.vue';
import ContentManipulation from '@/components/ContentManipulation.vue';
import FaultRules from '@/components/FaultRules.vue';
import GroupBanner from '@/components/GroupBanner.vue';
import SessionDetails from '@/components/SessionDetails.vue';
import CollapsibleSection from '@/components/CollapsibleSection.vue';
import ShellLayout from '@/components/ShellLayout.vue';
import StatusBanners from '@/components/StatusBanners.vue';
// Shared display half — same component testing-session.html and
// session-viewer.html mount. Brings historical preload + brush +
// accordion + cursor sync along automatically. Brush stays hidden
// while live-following; appears once the operator pauses.
import SessionDisplay from '@/components/SessionDisplay.vue';

const { players, isLoading, isError, error, sseState, refetch, deletePlayer } = usePlayers();
const { groups, link, disband } = useGroups();

/** Selected pill checkboxes (group-edit selection set). */
const selected = ref<Set<string>>(new Set());

/** Currently active session — only one card stack visible at a time.
 *  Sticky once chosen; we don't auto-reset when other sessions show up. */
const activeId = ref<string | null>(null);

watch(
  () => players.value.map((p) => p.id).join('|'),
  () => {
    // If activeId no longer maps to an existing player, fall through to
    // the most-recently-seen one (matches legacy "pick first" behaviour).
    if (activeId.value && !players.value.find((p) => p.id === activeId.value)) {
      activeId.value = null;
    }
    if (!activeId.value && players.value.length) {
      const sorted = players.value.slice().sort((a, b) => {
        const ta = a.last_seen_at ? Date.parse(a.last_seen_at) : 0;
        const tb = b.last_seen_at ? Date.parse(b.last_seen_at) : 0;
        return tb - ta;
      });
      activeId.value = sorted[0].id;
    }
    // Prune stale selections (matches legacy renderSessionTabs cleanup).
    const valid = new Set(players.value.map((p) => p.id));
    const next = new Set<string>();
    for (const id of selected.value) if (valid.has(id)) next.add(id);
    if (next.size !== selected.value.size) selected.value = next;
  },
  { immediate: true },
);

const activePlayer = computed<PlayerRecord | null>(() => {
  if (!activeId.value) return null;
  return players.value.find((p) => p.id === activeId.value) ?? null;
});

// play_id comes straight off the active PlayerRecord; SessionDisplay
// passes player_id (the live UUID) to the forwarder for archive
// lookups, no client-side session resolution needed.
const activePlayIdRef = computed<string | null>(() => activePlayer.value?.current_play?.id ?? null);

/** Per-player group_id lookup (sourced from PlayerGroup.member_player_ids). */
const groupIdOf = computed(() => {
  const map = new Map<string, string>();
  for (const g of groups.value ?? []) {
    const members = g.member_player_ids;
    if (!Array.isArray(members)) continue;
    for (const pid of members) map.set(pid, g.id);
  }
  return map;
});

function isGrouped(p: PlayerRecord): boolean {
  return groupIdOf.value.has(p.id);
}

function groupMembersOf(playerId: string): string[] {
  const gid = groupIdOf.value.get(playerId);
  if (!gid) return [];
  const g = (groups.value ?? []).find((x) => x.id === gid);
  return g?.member_player_ids ?? [];
}

function setActive(id: string) {
  activeId.value = id;
}

/** Toggle a pill's selection state.
 *  - For grouped sessions, checking one auto-checks all peers; unchecking
 *    requests an unlink of that member (legacy `v2GroupUnlinkMember`).
 *  - For ungrouped sessions, plain selection. */
function togglePillCheckbox(id: string, checked: boolean, e?: Event) {
  // Avoid the click bubbling to the pill button (which would change activeId).
  e?.stopPropagation?.();
  const next = new Set(selected.value);
  if (isGrouped(players.value.find((p) => p.id === id)!)) {
    if (checked) {
      // Auto-add every group sibling.
      for (const memberId of groupMembersOf(id)) next.add(memberId);
    } else {
      // Unchecking a grouped pill → ask to unlink that one member.
      next.delete(id);
      // Best-effort: legacy calls v2GroupUnlinkMember(player_id). The v2
      // group API as we've modelled it only supports disband (drop the
      // whole group) — fall back to that with a confirmation.
      const gid = groupIdOf.value.get(id);
      if (gid && confirm(`Unlink session — this disbands group ${gid.slice(0, 8)}. Continue?`)) {
        disband(gid);
      }
    }
  } else {
    if (checked) next.add(id);
    else next.delete(id);
  }
  selected.value = next;
}

function linkSelected() {
  if (selected.value.size < 2) return;
  link(Array.from(selected.value));
  selected.value = new Set();
}

async function releaseAll() {
  if (!players.value.length) return;
  if (!confirm(`Release all ${players.value.length} session(s)?`)) return;
  // Sequential — same effect as legacy bulk-delete on the v2 endpoint.
  for (const p of players.value) {
    try { await deletePlayer(p.id); } catch (e) { console.error(e); }
  }
}

function portOf(p: PlayerRecord): string {
  const raw = (p as any).raw_session ?? {};
  const port = raw.x_forwarded_port_external ?? raw.x_forwarded_port ?? null;
  return port != null ? String(port) : '';
}

function pillLabel(p: PlayerRecord): string {
  const port = portOf(p);
  const portSuffix = port ? ` (Port ${port})` : '';
  const tail = deviceContentLabel(p);
  // Drop the trailing " · …" entirely if we can't infer device or
  // content — a hex UUID slice doesn't help a human, so don't show
  // anything rather than fall back to it.
  const tailSuffix = tail ? ` · ${tail}` : '';
  return `Session #${p.display_id ?? '?'}${portSuffix}${tailSuffix}`;
}

/** Stable group-badge text shared with the GroupBanner chip. */
function groupBadgeFor(playerId: string): string {
  const gid = groupIdOf.value.get(playerId);
  if (!gid) return '';
  const g = (groups.value ?? []).find((x) => x.id === gid);
  if (!g) return `Group ${gid.slice(0, 6)}`;
  return groupNameFor(g, players.value ?? []);
}

const sortedPlayers = computed<PlayerRecord[]>(() => {
  return players.value.slice().sort((a, b) => {
    const ta = a.last_seen_at ? Date.parse(a.last_seen_at) : 0;
    const tb = b.last_seen_at ? Date.parse(b.last_seen_at) : 0;
    return tb - ta;
  });
});

</script>

<template>
  <ShellLayout active-page="testing">
    <template #header-right>
      <span class="sse" :data-state="sseState">{{ sseState }}</span>
      <button class="refresh" type="button" @click="refetch()" title="Force refresh">↻</button>
    </template>

    <div class="page">
      <header class="page-header">
        <div>
          <div class="page-title">Testing Monitor</div>
          <div class="page-subtitle">Launch sessions and tune failure settings per session.</div>
        </div>
      </header>

      <StatusBanners />

      <div class="page-card">
        <div class="page-card-header">
          <div class="page-card-title">
            Active Sessions
            <span v-if="players.length" class="count-badge">{{ players.length }}</span>
          </div>
          <div class="btn-row">
            <button class="btn btn-secondary" type="button" @click="refetch()">Refresh</button>
            <button class="btn btn-danger" type="button" :disabled="!players.length" @click="releaseAll">
              Release All Sessions
            </button>
          </div>
        </div>

        <div v-if="isLoading" class="empty">Loading sessions…</div>
        <div v-else-if="isError" class="empty error">
          Error: {{ error?.message ?? 'unknown' }}
        </div>
        <div v-else-if="!sortedPlayers.length" class="empty">
          No connected sessions. Launch one on a device.
        </div>

        <template v-else>
          <!-- Session-tab pill rail. Click selects active; checkbox is the
               group-selection toggle (auto-syncs grouped siblings). -->
          <div class="session-tabs">
            <button
              v-for="p in sortedPlayers"
              :key="p.id"
              class="session-tab"
              :class="{
                active: activeId === p.id,
                grouped: isGrouped(p),
              }"
              type="button"
              @click="setActive(p.id)"
            >
              <input
                v-if="sortedPlayers.length > 1"
                type="checkbox"
                class="session-checkbox"
                :checked="selected.has(p.id)"
                @click.stop
                @change="togglePillCheckbox(p.id, ($event.target as HTMLInputElement).checked, $event)"
              />
              {{ pillLabel(p) }}
              <span v-if="groupIdOf.get(p.id)" class="group-badge">
                {{ groupBadgeFor(p.id) }}
              </span>
            </button>
            <!-- "Group" appears at the tail of the pill rail once the
                 operator has ticked 2+ checkboxes. This replaces the
                 separate group-controls bar — the action lives where the
                 selection happens. -->
            <button
              v-if="selected.size >= 2"
              class="group-trigger"
              type="button"
              @click="linkSelected"
              :title="`Group ${selected.size} selected sessions`"
            >
              Group ({{ selected.size }})
            </button>
          </div>

          <!-- Active session's panel stack. Identical structure to
               TestingSession.vue minus the Playback (video) frame. -->
          <template v-if="activePlayer">
            <GroupBanner :player-id="activePlayer.id" />

            <!-- Session Details sits above the control panels for
                 quick "what am I looking at" reference. SessionDisplay
                 below suppresses its duplicate via :hide-session-details. -->
            <CollapsibleSection title="Session Details" persist-key="session-details">
              <SessionDetails :player-id="activePlayer.id" />
            </CollapsibleSection>

            <h3 class="session-controls-heading">Session Controls</h3>

            <!-- Control panels — same as testing-session.html. These
                 mutate live server state so they stay outside
                 SessionDisplay (which is read-only). The Delete
                 Session affordance lives on GroupBanner above, so no
                 inline release button is needed here. -->
            <CollapsibleSection title="Fault Injection" :open="true" persist-key="fault-injection">
              <FaultRules :player-id="activePlayer.id" />
            </CollapsibleSection>

            <CollapsibleSection title="Content Manipulation" persist-key="content-manipulation">
              <ContentManipulation :player-id="activePlayer.id" />
            </CollapsibleSection>

            <CollapsibleSection title="Server Timeouts" persist-key="server-timeouts">
              <TransferTimeouts :player-id="activePlayer.id" />
            </CollapsibleSection>

            <CollapsibleSection title="Network Shaping" :open="true" persist-key="network-shaping">
              <ShapeSliders :player-id="activePlayer.id" />
              <h3 class="subhead">Pattern</h3>
              <NetworkShapingPattern :player-id="activePlayer.id" />
            </CollapsibleSection>

            <h3 class="session-controls-heading">Session Display</h3>

            <!-- Display half — historical preload + Focus Window fold
                 + cursor sync, shared with testing-session.html and
                 session-viewer.html. Brush + event filter + nav-bar
                 live inside the Focus Window fold (collapse via its
                 chevron to hide). -->
            <SessionDisplay
              :player-id="activePlayer.id"
              :play-id="activePlayIdRef"
              mode="live"
              hide-session-details
            />
          </template>
        </template>
      </div>
    </div>
  </ShellLayout>
</template>

<style scoped>
.page {
  padding: 24px;
  margin: 0 auto;
}

.page-header {
  margin-bottom: 16px;
}
.page-title {
  font-size: 24px;
  font-weight: 600;
  color: #202124;
}
.page-subtitle {
  font-size: 13px;
  color: #5f6368;
  margin-top: 2px;
}

.page-card {
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 12px;
  padding: 16px;
  box-shadow: 0 1px 3px rgba(60, 64, 67, 0.05);
}
.page-card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
  gap: 12px;
  flex-wrap: wrap;
}
.page-card-title {
  font-size: 16px;
  font-weight: 600;
  color: #202124;
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
.count-badge {
  font-size: 11px;
  font-weight: 600;
  background: #e8f0fe;
  color: #1a73e8;
  padding: 2px 8px;
  border-radius: 10px;
}

.btn-row { display: flex; gap: 10px; flex-wrap: wrap; }
.btn {
  background: #f1f3f4;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 6px 12px;
  font-size: 13px;
  font-weight: 500;
  color: #202124;
  cursor: pointer;
}
.btn:hover { background: #e8eaed; }
.btn:disabled { opacity: 0.55; cursor: not-allowed; }
.btn-sm { padding: 4px 10px; font-size: 12px; }
.btn-secondary { background: #f1f3f4; }
.btn-danger {
  background: #fee2e2;
  border-color: #fca5a5;
  color: #991b1b;
}
.btn-danger:hover { background: #fecaca; }
.btn-text {
  background: transparent;
  border: 1px solid transparent;
  color: #d93025;
  padding: 2px 8px;
}
.btn-text:hover { background: #fce8e6; }

.refresh {
  background: transparent;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 4px 8px;
  font-size: 14px;
  cursor: pointer;
}
.refresh:hover { background: #f1f3f4; }

.sse {
  text-transform: uppercase;
  padding: 2px 8px;
  border-radius: 10px;
  font-weight: 600;
  font-size: 10px;
  letter-spacing: 0.5px;
}
.sse[data-state='connecting'] { background: #fef3c7; color: #92400e; }
.sse[data-state='open']       { background: #d1fae5; color: #065f46; }
.sse[data-state='closed']     { background: #fee2e2; color: #991b1b; }

.session-tabs {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-bottom: 16px;
}

/* Tail-of-rail "Group (N)" trigger — appears only when ≥2 pills are
 * ticked. Visually adjacent to the last pill so the action reads as
 * "and now group these N". */
.group-trigger {
  background: #10b981;
  color: white;
  border: 1px solid #059669;
  border-radius: 999px;
  padding: 8px 16px;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  transition: background 0.15s ease, box-shadow 0.15s ease;
  box-shadow: 0 4px 10px rgba(16, 185, 129, 0.25);
}
.group-trigger:hover {
  background: #059669;
  box-shadow: 0 6px 14px rgba(5, 150, 105, 0.32);
}
.session-tab {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  border: 1px solid #c7d2fe;
  background: #eef2ff;
  color: #1e1b4b;
  padding: 8px 14px;
  border-radius: 999px;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  transition: transform 0.08s ease, box-shadow 0.15s ease, background 0.15s ease;
}
.session-tab:hover {
  background: #e0e7ff;
  box-shadow: 0 4px 12px rgba(59, 130, 246, 0.18);
  transform: translateY(-1px);
}
.session-tab.active {
  background: #1e40af;
  color: #fff;
  border-color: #1e40af;
  box-shadow: 0 6px 14px rgba(30, 64, 175, 0.32);
}
.session-tab.grouped {
  border-left: 4px solid #10b981;
  padding-left: 10px;
}
.session-tab.grouped.active { border-left-color: #34d399; }

.session-checkbox {
  width: 16px;
  height: 16px;
  margin: 0;
  cursor: pointer;
  accent-color: #1e40af;
}

.group-badge {
  background: #10b981;
  color: white;
  font-size: 10px;
  font-weight: 700;
  padding: 1px 8px;
  border-radius: 8px;
  font-family: ui-monospace, monospace;
}
.session-tab.active .group-badge { background: #34d399; }

.session-controls-heading {
  margin: 20px 0 12px;
  font-size: 16px;
  font-weight: 600;
  color: #202124;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.empty {
  text-align: center;
  padding: 48px 20px;
  color: #5f6368;
  background: #f8f9fa;
  border-radius: 6px;
  font-size: 13px;
}
.empty.error { color: #991b1b; background: #fce8e6; }

.chart-stack {
  display: grid;
  gap: 20px;
}

.subhead {
  margin: 14px 0 8px 0;
  font-size: 11px;
  font-weight: 600;
  color: #6b7280;
  text-transform: uppercase;
  letter-spacing: 0.5px;
}
</style>
