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
import { ref, computed, onMounted } from 'vue';
import ShellLayout from '@/components/ShellLayout.vue';
import SessionDisplay from '@/components/SessionDisplay.vue';

const qs = new URLSearchParams(window.location.search);
// v3 canonical: identify an archived play by (player_id, play_id).
// session_id was the legacy proxy-port handle — not needed here since
// the v3 timeseries endpoint and the SSE pool both key by player_id.
const playerId = ref<string>(qs.get('player_id') ?? '');
const playId = ref<string | null>(qs.get('play_id'));

// Starred state. Optimistically toggled on click, then synced from
// the server response. Initial fetch in onMounted below.
const starred = ref<boolean>(false);
async function toggleStarred() {
  if (!playerId.value || !playId.value) return;
  const next = !starred.value;
  starred.value = next; // optimistic
  try {
    const url = `/analytics/api/sessions/${encodeURIComponent(playerId.value)}/${encodeURIComponent(playId.value)}/star`;
    const resp = await fetch(url, { method: next ? 'POST' : 'DELETE' });
    if (!resp.ok) throw new Error(`star ${resp.status}`);
  } catch {
    starred.value = !next; // rollback on failure
  }
}

const bundleHref = computed(() => {
  if (!playerId.value) return '#';
  const p = new URLSearchParams();
  p.set('player_id', playerId.value);
  if (playId.value) p.set('play_id', playId.value);
  return '/analytics/api/session_bundle?' + p.toString();
});
// Back-link points at the LEGACY picker — v3 sessions picker isn't
// ready as the default yet. Flip to /dashboard/v3/sessions.html once
// it is.
const backHref = '/dashboard/sessions.html';

onMounted(async () => {
  // Look up the current starred state. The endpoint returns
  // {"starred": true|false} (or 404 if the play hasn't been touched).
  if (playerId.value && playId.value) {
    try {
      const url = `/analytics/api/sessions/${encodeURIComponent(playerId.value)}/${encodeURIComponent(playId.value)}/star`;
      const resp = await fetch(url);
      if (resp.ok) {
        const j = await resp.json();
        starred.value = !!j.starred;
      }
    } catch {
      // Star lookup is non-essential; the toggle still works.
    }
  }
});

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
            mode="archive"
          />
        </template>
      </main>
    </div>
  </ShellLayout>
</template>

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
