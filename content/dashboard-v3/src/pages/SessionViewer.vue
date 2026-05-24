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
import { useQuery, useMutation, useQueryClient } from '@tanstack/vue-query';
import ShellLayout from '@/components/ShellLayout.vue';
import SessionDisplay from '@/components/SessionDisplay.vue';
import ChatPanel from '@/components/chat/ChatPanel.vue';
import { parseTimeAny, canonicalUUID } from '@/composables/urlTimeFormat';
import { getPlay, patchPlayClassification, type PlaySummary } from '@/repo/v2-repo';

const qs = new URLSearchParams(window.location.search);
// v3 canonical: identify an archived play by (player_id, play_id).
// session_id was the legacy proxy-port handle — not needed here since
// the v3 timeseries endpoint and the SSE pool both key by player_id.
// UUIDs are lowercased — CH stores them lowercase and iOS sometimes
// emits uppercase (case_sensitivity_ids memory).
const playerId = ref<string>(canonicalUUID(qs.get('player_id') ?? ''));
const playId = ref<string | null>(qs.get('play_id') ? canonicalUUID(qs.get('play_id')!) : null);

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

/** "Show before/after" toggle. When ON, SessionDisplay drops the
 *  play_id filter on its SSE subscription and widens the time
 *  window to play_bounds ± 5 min, so rows from neighbouring plays
 *  for the same player become visible in the same panel layout.
 *  Default OFF — the page is locked to this play, matching the
 *  "click a session row, see that session" mental model from
 *  sessions.html. */
const showContext = ref<boolean>(false);
function toggleShowContext() { showContext.value = !showContext.value; }

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
const backHref = '/dashboard/v3/sessions.html';

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
        <div v-if="!playerId" class="empty">
          <p>No <code>player_id</code> in the URL.</p>
          <p>Open <code>/dashboard/v3/session-viewer.html?player_id=&lt;uuid&gt;&amp;play_id=&lt;uuid&gt;</code></p>
        </div>

        <template v-else>
          <!-- REPLAY banner — page-specific (SessionDisplay's brush
               block joins flush below it via shared border styling). -->
          <header class="meta-banner">
            <div class="meta-line">
              <span class="replay-badge">REPLAY</span>
              <span class="meta-label">player</span>
              <code class="id-pill" :title="playerId">{{ playerId || '(no player)' }}</code>
              <span class="meta-label">play</span>
              <!-- play_id gets a "disabled" style when showContext is on:
                   the SSE has dropped the play_id filter so the id shown
                   here is no longer what's actually being filtered. The
                   strike + muted colour signals "this label doesn't
                   reflect what you're currently looking at". -->
              <code
                class="id-pill"
                :class="{ 'id-pill-disabled': showContext }"
                :title="showContext
                  ? `${playId ?? '(all plays)'} — filter disabled while showing context`
                  : (playId ?? '(all plays)')"
              >{{ playId ?? '(all plays)' }}</code>
            </div>
            <div class="banner-actions">
              <button
                type="button"
                class="banner-btn"
                :class="{ active: showContext }"
                @click="toggleShowContext"
                :disabled="!playId"
                :title="showContext
                  ? 'Snap back to this play only'
                  : 'Show rows from before and after this play (same player, ±5 min)'"
              >
                {{ showContext ? '🔓 Showing context' : '🔒 This play only' }}
              </button>
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
            :show-context="showContext"
            :start-ms="startMs"
            :end-ms="endMs"
          />
        </template>
      </main>
    </div>

    <!-- AI chat side panel scoped to this play (#497). Default
         collapsed so the existing chart layout is undisturbed.
         Brush-range integration (scope.kind='range' + from/to
         reactive to the brush) is a follow-up; for now scope
         is play-only. -->
    <Teleport to="body">
      <div class="chat-dock" v-if="playId && playerId">
        <ChatPanel
          :scope="{ kind: 'play', play_id: playId, player_id: playerId }"
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
.page { display: flex; }
.content {
  padding: 14px 20px;
  margin: 0 auto;
  flex: 1;
}
.header-title { font-size: 16px; font-weight: 600; }

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
