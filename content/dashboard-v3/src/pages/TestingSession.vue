<script setup lang="ts">
/**
 * TestingSession.vue — per-player editing page (the big page).
 * Sections get added one at a time during Stage 2; for now this is
 * the page scaffold + the ShapeSliders proving-ground component.
 *
 * URL contract (matches the legacy v2 page so existing bookmarks work):
 *   /dashboard/v3/testing-session.html?player_id=<uuid>
 */
import { computed } from 'vue';
import { useUrlSearchParams } from '@vueuse/core';
import { usePlayer } from '@/composables/usePlayer';
import ShapeSliders from '@/components/ShapeSliders.vue';
import NetworkShapingPattern from '@/components/NetworkShapingPattern.vue';
import TransferTimeouts from '@/components/TransferTimeouts.vue';
import ContentManipulation from '@/components/ContentManipulation.vue';
import FaultRules from '@/components/FaultRules.vue';
import GroupBanner from '@/components/GroupBanner.vue';
import VideoPlayerFrame from '@/components/VideoPlayerFrame.vue';
import SessionDetails from '@/components/SessionDetails.vue';
import CollapsibleSection from '@/components/CollapsibleSection.vue';
import ShellLayout from '@/components/ShellLayout.vue';
import StatusBanners from '@/components/StatusBanners.vue';
// Display half (Session Details / Player Metrics / Player State /
// Bitrate charts / Network Log + Focus Window fold) is shared with
// SessionViewer via SessionDisplay.vue. SessionDisplay resolves
// historical via player_id directly against the forwarder — no
// client-side session lookup needed.
import SessionDisplay from '@/components/SessionDisplay.vue';

const params = useUrlSearchParams<{ player_id?: string; url?: string }>('history');
const playerId = computed(() => params.player_id ?? '');

/**
 * Defensive rescue for stale grid bundles. Pre-fix grid builds appended
 * the page's fresh player_id to a tile URL that ALREADY carried its own
 * `player_id=<tileId>`, producing `…?player_id=A&player_id=B`. The
 * shaper then registers under the first (`A`) and the page (which
 * queries `B`) 404s forever. If we see duplicates in `url=`, rewrite
 * down to a single `player_id` matching the page's id so the shaper
 * registers under what we'll actually look up.
 *
 * Once every browser is on the post-fix grid bundle this is a no-op,
 * but it costs ~nothing and shields users with cached old code.
 */
function sanitizeUrlOverride(raw: string, pid: string): string {
  if (!raw) return raw;
  try {
    const u = new URL(raw, window.location.href);
    const all = u.searchParams.getAll('player_id');
    // No player_id, or already a single matching one → nothing to do.
    if (all.length === 0) return raw;
    if (all.length === 1 && all[0] === pid) return raw;
    u.searchParams.delete('player_id');
    if (pid) u.searchParams.set('player_id', pid);
    const fixed = u.toString();
    if (fixed !== raw) {
      console.warn('[TS] sanitised urlOverride to dedupe player_id', {
        before: raw,
        after: fixed,
        duplicates: all,
      });
    }
    return fixed;
  } catch {
    return raw;
  }
}

const urlOverride = computed(() => sanitizeUrlOverride(params.url ?? '', playerId.value));

console.log('[TS] page boot', {
  rawSearch: window.location.search,
  parsedPlayerId: params.player_id,
  parsedUrl: params.url,
  resolvedPlayerId: playerId.value,
  resolvedUrlOverride: urlOverride.value,
});

const { player, isLoading, isError, error, sseState } = usePlayer(playerId);

// play_id falls out of the live PlayerRecord directly; the forwarder
// accepts player_id alongside play_id, so SessionDisplay can query
// archive data without a session_id lookup.
const playIdRef = computed<string | null>(() => player.value?.current_play?.id ?? null);

const masterUrl = computed<string>(() => player.value?.current_play?.manifest?.master_url ?? 'Loading…');
const displayId = computed<string>(() => {
  const n = player.value?.display_id;
  return typeof n === 'number' ? `#${n}` : '';
});

</script>

<template>
  <ShellLayout active-page="test-playback">
    <template #header-right>
      <span class="player-id" v-if="playerId">{{ playerId }}</span>
      <span class="sse" :data-state="sseState">{{ sseState }}</span>
    </template>

    <div class="page">
      <main class="content">
      <div v-if="!playerId" class="empty">
        <p>No <code>player_id</code> in the URL.</p>
        <p>Open <code>/dashboard/v3/testing-session.html?player_id=&lt;uuid&gt;</code></p>
      </div>

      <!-- Loading + error states only kick in when there's NO urlOverride.
           When the caller already knows the stream URL (grid right-click,
           legacy deeplink), we render the content branch immediately so
           the player frame mounts once and stays — preventing a remount
           loop while the player record's GET is still 404'ing. -->
      <div v-else-if="isLoading && !urlOverride" class="empty">Loading player…</div>

      <div v-else-if="isError && !urlOverride" class="empty error">
        Error loading player: {{ error?.message ?? 'unknown' }}
      </div>

      <template v-else-if="player || urlOverride">
        <header class="page-header">
          <div>
            <div class="page-title">Testing Playback {{ displayId }}</div>
            <div class="page-subtitle">{{ masterUrl }}</div>
          </div>
        </header>

        <StatusBanners />

        <GroupBanner :player-id="playerId" />

        <!-- Playback frame is a v3-only convenience — keep it at the
             top of the page since it's the most direct affordance for
             "watch what this device is currently playing" without
             reaching for the device itself.

             Every section below gets a stable `persist-key`. The
             CollapsibleSection component reads/writes
             `testing_session_collapse_<key>` to localStorage (matches
             legacy) and honours `?open_folds=<key>,<key>` deep links. -->
        <!-- persist-key bumped to playback-v2 — older "playback" key
             may have been stored collapsed; reset to default-open so
             returning operators see the video frame on landing. New
             toggles still persist under the v2 key normally. -->
        <CollapsibleSection title="Playback" :open="true" persist-key="playback-v2">
          <VideoPlayerFrame :player-id="playerId" :url-override="urlOverride" />
        </CollapsibleSection>

        <!-- Session Details sits above the control panels on live
             pages: it's the "what am I looking at" summary the
             operator reads before adjusting fault rules / shaping.
             SessionDisplay below omits its internal duplicate via
             :hide-session-details. -->
        <CollapsibleSection title="Session Details" persist-key="session-details">
          <SessionDetails :player-id="playerId" />
        </CollapsibleSection>

        <h3 class="session-controls-heading">Session Controls</h3>

        <!-- Control panels — order mirrors legacy testing-session.html.
             These mutate server state and so are LIVE-only; the
             archive session-viewer omits them. -->
        <CollapsibleSection title="Fault Injection" :open="true" persist-key="fault-injection">
          <FaultRules :player-id="playerId" />
        </CollapsibleSection>

        <CollapsibleSection title="Content Manipulation" persist-key="content-manipulation">
          <ContentManipulation :player-id="playerId" />
        </CollapsibleSection>

        <CollapsibleSection title="Server Timeouts" persist-key="server-timeouts">
          <TransferTimeouts :player-id="playerId" />
        </CollapsibleSection>

        <CollapsibleSection title="Network Shaping" :open="true" persist-key="network-shaping">
          <ShapeSliders :player-id="playerId" />
          <h3 class="subhead">Pattern</h3>
          <NetworkShapingPattern :player-id="playerId" />
        </CollapsibleSection>

        <h3 class="session-controls-heading">Session Display</h3>

        <!-- Display half — same component the archive session-viewer
             mounts. The brush + accordion + nav-bar live inside the
             Focus Window fold (collapse via its chevron to hide).
             SessionDisplay queries archive history with player_id
             (no session lookup); play_id comes straight from the
             live PlayerRecord. Empty until forwarder ingest catches
             up — display panels still render via the live SSE path
             during that window. -->
        <SessionDisplay
          :player-id="playerId"
          :play-id="playIdRef"
          mode="live"
          hide-session-details
        />

      </template>
      </main>
    </div>
  </ShellLayout>
</template>

<style scoped>
.page {
  font-family: system-ui, -apple-system, sans-serif;
  color: #111;
  background: #fff;
  min-height: 100vh;
  line-height: 1.5;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 24px;
  border-bottom: 1px solid #e5e7eb;
  background: #f9fafb;
}

.topbar h1 {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  color: #111827;
}

.meta {
  display: flex;
  gap: 12px;
  align-items: center;
  font-size: 12px;
  color: #6b7280;
}

.player-id {
  font-family: ui-monospace, monospace;
  background: #1f2937;
  color: #e5e7eb;
  padding: 4px 8px;
  border-radius: 4px;
}

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

.content {
  padding: 24px;
  margin: 0 auto;
}

.empty {
  text-align: center;
  padding: 64px 24px;
  color: #6b7280;
}

.empty.error {
  color: #991b1b;
}

.empty code {
  background: #1f2937;
  color: #e5e7eb;
  padding: 2px 6px;
  border-radius: 4px;
  font-family: ui-monospace, monospace;
}

.session-controls-heading {
  margin: 20px 0 12px;
  font-size: 16px;
  font-weight: 600;
  color: #202124;
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
  font-size: 12px;
  color: #5f6368;
  word-break: break-all;
  margin-top: 4px;
}

/* Legacy stacked the four metrics charts vertically inside one
 * collapsible. Same here — keeps the X-axis lined up so a spike in
 * bandwidth and the matching buffer dip read at the same time slice. */
.chart-stack {
  display: grid;
  gap: 24px;
}

.subhead {
  margin: 20px 0 12px 0;
  font-size: 12px;
  font-weight: 600;
  color: #6b7280;
  text-transform: uppercase;
  letter-spacing: 0.5px;
}
</style>
