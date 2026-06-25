<script setup lang="ts">
/**
 * SessionViewer.vue — v3 archive replay page.
 *
 * Replaces the legacy /dashboard/session-viewer.html for sessions
 * archived in ClickHouse (forwarder). Reads `session_id` (and
 * optional `play_id`) from the URL.
 *
 * The display half (panels + brush + accordion + cursor sync) lives
 * in SessionDisplay.vue, shared with the live testing-session page.
 * This page just owns the REPLAY banner (Star / Download bundle /
 * Back to sessions) plus the synthetic `archive:` cache key that
 * routes display panels to the side-channel store in v2-repo instead
 * of the live `/api/v2/players` fetch.
 */
import { ref, computed } from 'vue';
import { useQuery, useQueries, useMutation, useQueryClient } from '@tanstack/vue-query';
import ShellLayout from '@/components/ShellLayout.vue';
import SessionDisplay from '@/components/SessionDisplay.vue';
import ChatPanel from '@/components/chat/ChatPanel.vue';
import { parseTimeAny, canonicalUUID, parseCompareParam } from '@/composables/urlTimeFormat';
import { contentFromMasterUrl } from '@/composables/useSessionLabels';
import { getPlay, patchPlayClassification, type PlaySummary } from '@/repo/v2-repo';
import { useChartCoordination } from '@/composables/useChartCoordination';
import type { ChatScope } from '@/types/chat';

const qs = new URLSearchParams(window.location.search);
// v3 canonical: identify an archived play by (player_id, play_id).
// session_id was the legacy proxy-port handle — not needed here since
// the v3 timeseries endpoint and the SSE pool both key by player_id.
// UUIDs are lowercased — CH stores them lowercase and iOS sometimes
// emits uppercase (case_sensitivity_ids memory).
// #736: `compare=<player>~<play>~<tag>,…` — the grouped set to overlay in
// archive compare mode (the active play is included). Like testing.html's
// session-tab pill rail, the active member is switchable: player_id / play_id
// FOLLOW the selected tab, SessionDisplay re-keys reactively, and it
// re-derives self vs siblings from the (unchanged) compare set. Empty
// `compare` ⇒ ordinary single-play view.
const comparePlays = parseCompareParam(qs.get('compare'));
const urlPlayerId = canonicalUUID(qs.get('player_id') ?? '');
const urlPlayId = qs.get('play_id') ? canonicalUUID(qs.get('play_id')!) : null;
// Active tab index — defaults to the play the user clicked (the URL player_id).
const activeIdx = ref(Math.max(0, comparePlays.findIndex((m) => m.playerId === urlPlayerId)));
const playerId = computed<string>(() =>
  comparePlays.length ? comparePlays[activeIdx.value].playerId : urlPlayerId,
);
const playId = computed<string | null>(() =>
  comparePlays.length ? (comparePlays[activeIdx.value].playId ?? null) : urlPlayId,
);

/** Initial time window. New canonical param names are `from` / `to`
 *  (shorter, no `:` in compact ISO → no `%3A` clutter). Legacy
 *  `start_time` / `end_time` still accepted so already-copied links
 *  keep working. parseTimeAny handles BOTH compact ISO basic
 *  (`20260522T170417Z`) and traditional ISO (`2026-05-22T17:04:17Z`).
 *
 *  `to` absent OR `to=live`/`to=now` ⇒ follow live edge.
 */
const startMs = ref<number | null>(parseTimeAny(qs.get('from') ?? qs.get('start_time')));
const endMs = ref<number | null>(parseTimeAny(qs.get('to') ?? qs.get('end_time')));

/** Same per-player coord instance SessionDisplay uses. Reading
 *  effectiveRange here gives us the live brush window for the AI
 *  chat scope — no event plumbing, both components read the same
 *  module-level reactive state by playerId. */
const coord = useChartCoordination(playerId);

/** Brush-vs-full-play detection. If the brush covers the whole
 *  play (or play bounds aren't known yet), treat as scope='play';
 *  if it's a strict subset, scope='range' with from/to so the
 *  bot's system-prompt preamble flips to "focus on this window".
 *  Tolerance handles tiny rounding gaps between the brush extent
 *  and the play's started_at / last_seen_at. */
const BRUSH_FULL_TOLERANCE_MS = 1500;
const chatScope = computed<ChatScope>(() => {
  if (!playId.value || !playerId.value) return { kind: 'fleet' };
  const brush = coord.effectiveRange.value;
  const summary = playQuery.data.value;
  const playStart = summary?.started_at ? Date.parse(summary.started_at) : NaN;
  const playEnd = summary?.last_seen_at ? Date.parse(summary.last_seen_at) : NaN;
  const isFullPlay = !Number.isFinite(playStart) || !Number.isFinite(playEnd)
    || (brush.min <= playStart + BRUSH_FULL_TOLERANCE_MS && brush.max >= playEnd - BRUSH_FULL_TOLERANCE_MS);
  if (isFullPlay) {
    return { kind: 'play', play_id: playId.value, player_id: playerId.value };
  }
  return {
    kind: 'range',
    play_id: playId.value,
    player_id: playerId.value,
    from: new Date(brush.min).toISOString(),
    to: new Date(brush.max).toISOString(),
  };
});

// Session-viewer is always scoped to this play — play_id is passed straight to
// the SSE, the same play-scoped view testing.html uses. The old "showing context
// / this play only" padlock toggle was removed for parity with testing.html (and
// because it was confusing); the play_id filter is simply always on here.

// Starred state — backed by TanStack so the optimistic flip, the
// mutation rollback, and any future cache invalidations follow the
// same contract as Sessions.vue / usePlayer.ts § makeGroupMutation.
// No auto-refresh on this page, so cancelQueries is mostly defensive.
const qc = useQueryClient();
const playQueryKey = computed(() => ['play', playId.value] as const);
const playQuery = useQuery<PlaySummary | null>({
  queryKey: playQueryKey,
  queryFn: () => getPlay(playId.value as string),
  enabled: computed(() => !!playId.value),
  // One-shot for the initial starred state; refetch on focus is
  // enough — no point polling, this is a finished play.
  refetchInterval: false,
});
const starred = computed<boolean>(
  () => String(playQuery.data.value?.classification ?? '') === 'favourite',
);

// #736 member-tab labels — mirror testing.html's `Session #N (Port …) · device
// · content`. Port is derived from the session number (external band 21<sid>81,
// the third-from-last digit carrying the session); device · content + the group
// badge come off the active play summary (fleet members share them).
function portForTag(tag?: string): string {
  return tag && /^\d$/.test(tag) ? `21${tag}81` : '';
}
function memberLabel(m: { tag?: string }): string {
  const port = portForTag(m.tag);
  return `Session #${m.tag ?? '?'}${port ? ` (Port ${port})` : ''}`;
}
// Per-member device tail. One getPlay() per compare member, keyed identically
// to the active playQuery (['play', playId]) so the active member dedupes
// against the existing cache — one-shot, no polling, no SSE. Each tab shows
// ITS OWN device: previously a single computed read the ACTIVE play, so every
// tab showed the same device (a phone+TV compare read "· phone" on both).
const memberPlayQueries = useQueries({
  queries: computed(() => comparePlays.map((m) => ({
    queryKey: ['play', m.playId] as const,
    queryFn: () => getPlay(m.playId),
    enabled: !!m.playId,
    refetchInterval: false as const,
  }))),
});
// Bare CPU arch is not a meaningful device model (iOS reports "arm64"); skip it.
const ARCH_ONLY = /^(arm64|aarch64|x86_64|x86|arm)$/i;
function memberTail(i: number): string {
  const p = memberPlayQueries.value[i]?.data as Record<string, unknown> | undefined | null;
  if (!p) return '';
  const deviceClass = p.device_class ? String(p.device_class) : '';
  const modelRaw = p.device_model ? String(p.device_model) : '';
  const model = modelRaw && !ARCH_ONLY.test(modelRaw) ? modelRaw : '';
  const content = (p.content_id ? String(p.content_id) : '')
    || contentFromMasterUrl((p.master_manifest_url as string | null | undefined) ?? null);
  return [deviceClass, model, content].filter(Boolean).join(' · ');
}
const groupBadge = computed<string>(() => String(playQuery.data.value?.group_id ?? ''));

const starMutation = useMutation({
  mutationFn: (next: boolean) =>
    patchPlayClassification(playId.value as string, next ? 'favourite' : 'auto'),
  onMutate: async (next) => {
    if (!playId.value) return { prev: undefined };
    await qc.cancelQueries({ queryKey: playQueryKey.value });
    const prev = qc.getQueryData<PlaySummary | null>(playQueryKey.value);
    if (prev) {
      qc.setQueryData<PlaySummary | null>(playQueryKey.value, {
        ...prev,
        classification: next ? 'favourite' : '',
      });
    }
    return { prev };
  },
  onError: (_err, _vars, ctx) => {
    if (ctx && 'prev' in ctx) {
      qc.setQueryData(playQueryKey.value, ctx.prev);
    }
  },
  onSuccess: (settled) => {
    if (settled) qc.setQueryData(playQueryKey.value, settled);
  },
});

function toggleStarred() {
  if (!playerId.value || !playId.value) return;
  starMutation.mutate(!starred.value);
}

const bundleHref = computed(() => {
  if (!playerId.value) return '#';
  const p = new URLSearchParams();
  p.set('player_id', playerId.value);
  if (playId.value) p.set('play_id', playId.value);
  return '/analytics/api/session_bundle?' + p.toString();
});
const backHref = '/dashboard/sessions.html';

// (Initial starred-state lookup now lives in the playQuery above —
// useQuery fires automatically when playId is set; the onMounted
// fetch + try/catch is gone.)

</script>

<template>
  <ShellLayout active-page="sessions">
    <template #header>
      <div class="header-title">Session Viewer</div>
    </template>

    <div class="page">
      <main class="content">
        <header class="page-header">
          <div>
            <div class="page-title">Session Viewer</div>
            <div class="page-subtitle">Replay an archived session through the live charts.</div>
          </div>
        </header>

        <div v-if="!playerId" class="empty">
          <p>No <code>player_id</code> in the URL.</p>
          <p>Open <code>/dashboard/session-viewer.html?player_id=&lt;uuid&gt;&amp;play_id=&lt;uuid&gt;</code></p>
        </div>

        <template v-else>
          <!-- #736 member tab rail — like testing.html's session pills, on
               its own row above the banner. Click a tab to make that grouped
               play active (its panels show, its line goes solid); the overlay
               set stays the same. -->
          <div v-if="comparePlays.length" class="session-tabs" role="tablist" aria-label="Grouped sessions">
            <button
              v-for="(m, i) in comparePlays"
              :key="m.playerId"
              type="button"
              role="tab"
              class="session-tab grouped"
              :class="{ active: i === activeIdx }"
              :aria-selected="i === activeIdx"
              :title="m.playerId"
              @click="activeIdx = i"
            >
              <span class="st-label">{{ memberLabel(m) }}<span v-if="memberTail(i)" class="st-tail"> · {{ memberTail(i) }}</span></span>
              <span v-if="groupBadge" class="group-badge">{{ groupBadge }}</span>
            </button>
          </div>
          <!-- REPLAY banner — page-specific (SessionDisplay's brush
               block joins flush below it via shared border styling).
               Single-session controls only: player/play + actions. -->
          <header class="meta-banner">
            <div class="meta-line">
              <span class="replay-badge">REPLAY</span>
              <span class="meta-label">player</span>
              <code class="id-pill" :title="playerId">{{ playerId || '(no player)' }}</code>
              <span class="meta-label">play</span>
              <code class="id-pill" :title="playId ?? '(all plays)'">{{ playId ?? '(all plays)' }}</code>
            </div>
            <div class="banner-actions">
              <button
                type="button"
                class="banner-btn"
                :class="{ active: starred }"
                @click="toggleStarred"
                :disabled="!playId"
                :title="starred ? 'Unstar — TTL applies again' : 'Star — keep this play forever'"
              >
                {{ starred ? '★ Starred' : '☆ Star' }}
              </button>
              <a class="banner-btn" :href="bundleHref" :class="{ disabled: !playId }" download>⬇ Download bundle</a>
              <a class="banner-btn" :href="backHref">← Back to sessions</a>
            </div>
          </header>

          <SessionDisplay
            :player-id="playerId"
            :play-id="playId"
            :start-ms="startMs"
            :end-ms="endMs"
            :compare-plays="comparePlays"
          />
        </template>
      </main>
    </div>

    <!-- AI chat side panel — scope reflects the chart's brush.
         When the brush covers the full play, scope is
         {kind: 'play'}; when the brush is a strict subset,
         scope is {kind: 'range', from, to} so the bot focuses
         on events inside that window. SessionViewer and
         SessionDisplay share the same useChartCoordination
         instance (keyed by player_id), so brushRange is
         reactive without any event plumbing. -->
    <Teleport to="body">
      <div class="chat-dock" v-if="playId && playerId">
        <ChatPanel
          :scope="chatScope"
          :scope-key="`viewer:${playId}`"
          variant="panel"
          :start-collapsed="true"
        />
      </div>
    </Teleport>
  </ShellLayout>
</template>

<style>
/* Unscoped — Teleport-to-body element needs the parent style applied
   directly. Pinned to the right edge, full viewport height below the
   header. Same pattern as Sessions.vue. */
.chat-dock {
  position: fixed;
  top: var(--header-height, 64px);
  right: 0;
  bottom: 0;
  z-index: 50;
  box-shadow: var(--shadow-md);
  background: #fff;
}
</style>

<style scoped>
.page { display: flex; min-width: 0; }
.content {
  padding: 14px 20px;
  margin: 0 auto;
  flex: 1;
  /* min-width: 0 — flex items default to min-width: auto, which
     lets intrinsic child widths (timeline/chart canvases sized to
     their original viewport) push the item past its flex parent.
     Setting min-width: 0 lets flex shrink .content to its parent's
     bounds when the AI panel reduces available space. */
  min-width: 0;
  /* overflow-x: hidden — safety net for any grand-child (a Chart.js
     canvas, vis-timeline) that still renders at an explicit pixel
     width bigger than .content. Without this they'd bleed past
     .content's right edge into the AI dock area. Charts that need
     to be readable at narrow widths should observe their container
     and call .resize() — this is a clip, not a layout fix for them. */
  overflow-x: hidden;
}
.header-title { font-size: 16px; font-weight: 600; }
/* In-page page title, mirroring Testing.vue's .page-header/.page-title. */
.page-header { margin-bottom: 16px; }
.page-title { font-size: 24px; font-weight: 600; color: #202124; }
.page-subtitle { font-size: 13px; color: #5f6368; margin-top: 2px; }

.meta-banner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 14px;
  background: linear-gradient(90deg, #fff7ed, #fffbeb);
  border: 1px solid #fcd34d;
  border-radius: 8px 8px 0 0;
  border-bottom: none;
  flex-wrap: wrap;
}
.meta-line {
  display: inline-flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  font-size: 13px;
  color: #374151;
}
.replay-badge {
  background: #fde68a;
  color: #92400e;
  padding: 2px 10px;
  border-radius: 6px;
  font-size: 10px;
  font-weight: 700;
  letter-spacing: 1px;
}
.meta-label { font-weight: 600; color: #4b5563; }
.id-pill {
  background: #e5e7eb;
  color: #374151;
  padding: 2px 8px;
  border-radius: 4px;
  font-family: ui-monospace, monospace;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.2px;
  max-width: 360px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* When "Showing context" is on, the play_id pill no longer reflects
 * what the SSE is filtering by. Strike + mute so the operator can
 * see at a glance "this label doesn't match the data flowing". */
.id-pill-disabled {
  background: #f3f4f6;
  color: #9ca3af;
  text-decoration: line-through;
  text-decoration-color: #9ca3af;
  text-decoration-thickness: 1px;
}

.banner-actions { display: flex; gap: 6px; flex-wrap: wrap; }
/* #736 member tab rail — matches testing.html's session pills exactly. */
.session-tabs { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; margin-bottom: 8px; }
.session-tab {
  display: inline-flex; align-items: center; gap: 8px; max-width: 380px;
  border: 1px solid #c7d2fe; background: #eef2ff; color: #1e1b4b;
  padding: 8px 14px; border-radius: 999px; font-size: 13px; font-weight: 600;
  cursor: pointer; transition: transform 0.08s ease, box-shadow 0.15s ease, background 0.15s ease;
}
.session-tab:hover { background: #e0e7ff; box-shadow: 0 4px 12px rgba(59,130,246,0.18); transform: translateY(-1px); }
.session-tab.active { background: #1e40af; color: #fff; border-color: #1e40af; box-shadow: 0 6px 14px rgba(30,64,175,0.32); }
.session-tab.grouped { border-left: 4px solid #10b981; padding-left: 10px; }
.session-tab.grouped.active { border-left-color: #34d399; }
.session-tab .st-label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.session-tab .st-tail { font-weight: 500; opacity: 0.85; }
.session-tab .group-badge {
  flex: none; background: #10b981; color: #fff; font-size: 10px; font-weight: 700;
  padding: 1px 8px; border-radius: 8px; font-family: ui-monospace, monospace;
}
.session-tab.active .group-badge { background: #34d399; }
.banner-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  background: #fff;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  padding: 5px 12px;
  font-size: 12px;
  font-weight: 500;
  color: #1f2937;
  cursor: pointer;
  text-decoration: none;
}
.banner-btn:hover { background: #f9fafb; }
.banner-btn.active {
  background: #f59e0b;
  border-color: #d97706;
  color: #fff;
}
.banner-btn.disabled { opacity: 0.4; pointer-events: none; }

.empty {
  text-align: center;
  padding: 64px 24px;
  color: #6b7280;
}
.empty code {
  background: #1f2937;
  color: #e5e7eb;
  padding: 2px 6px;
  border-radius: 4px;
  font-family: ui-monospace, monospace;
}
</style>
