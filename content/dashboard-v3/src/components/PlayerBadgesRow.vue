<script setup lang="ts">
/**
 * PlayerBadgesRow.vue — chip strip above the player frame showing what
 * we know about the active play: HLS/DASH protocol, engine in use,
 * codec, segment-duration class, UHD '4K' indicator, group id.
 *
 * Derived from the current PlayerRecord — no extra wire calls.
 */
import { computed, toRef } from 'vue';
import { usePlayer } from '@/composables/usePlayer';

const props = defineProps<{
  playerId: string;
  engine: string;
}>();

const { player } = usePlayer(toRef(props, 'playerId'));

interface Badge {
  text: string;
  cls: string;
  title?: string;
}

const protoBadge = computed<Badge | null>(() => {
  const url = player.value?.current_play?.manifest?.master_url ?? '';
  if (/\.mpd(\?|$)/i.test(url)) return { text: 'DASH', cls: 'protocol dash' };
  if (/\.m3u8(\?|$)/i.test(url)) return { text: 'HLS', cls: 'protocol hls' };
  return null;
});

const engineBadge = computed<Badge | null>(() => {
  if (!props.engine || props.engine === 'auto') return null;
  return { text: props.engine, cls: `engine ${props.engine}` };
});

const codecBadge = computed<Badge | null>(() => {
  // Inferred from the master URL's content slug (legacy convention:
  // `_h264` / `_hevc` / `_av1` suffix). Falls back to "—" when the
  // slug is missing.
  const url = player.value?.current_play?.manifest?.master_url ?? '';
  const m = url.match(/_(h264|hevc|av1)/i);
  if (m) return { text: m[1].toUpperCase(), cls: 'codec' };
  return null;
});

const segmentBadge = computed<Badge | null>(() => {
  // Match legacy: 2s/4s/6s segment durations are common content slugs.
  const url = player.value?.current_play?.manifest?.master_url ?? '';
  const m = url.match(/_(2s|4s|6s|ll)\b/i);
  return m ? { text: m[1].toUpperCase(), cls: 'segment' } : null;
});

// 4K detection — match the legacy `update4kBadge` heuristic, expanded
// to also honour the sticky localStorage flag the v3 grid writes when
// it sees a 4K-capable tile (`ismKnown4k:<contentName>`). Three signals:
//   1. player.player_metrics.video_resolution (e.g. "3840x2160")
//   2. raw_session.max_height / max_resolution (server metadata)
//   3. localStorage.ismKnown4k:<contentName> (set by v3 grid or earlier
//      sessions on this content)
function parseHeight(v: any): number | null {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v === 'string') {
    const s = v.trim();
    let m = s.match(/\d{3,4}x(\d{3,4})/);
    if (m) return Number(m[1]);
    m = s.match(/^(\d{3,4})p$/i);
    if (m) return Number(m[1]);
    m = s.match(/^(\d{3,4})$/);
    if (m) return Number(m[1]);
  }
  return null;
}

const uhdBadge = computed<Badge | null>(() => {
  const UHD = 2160;
  // 1. Player-reported actual resolution.
  const res = player.value?.player_metrics?.video_resolution ?? '';
  if (/2160|3840|4K/i.test(res)) return { text: '4K', cls: 'uhd' };
  const playerH = parseHeight(res);
  if (playerH != null && playerH >= UHD) return { text: '4K', cls: 'uhd' };

  // 2. Server content metadata.
  const raw = (player.value as any)?.raw_session ?? {};
  const metaH =
    parseHeight(raw.max_height) ??
    parseHeight(raw.max_resolution) ??
    parseHeight((player.value as any)?.max_height) ??
    parseHeight((player.value as any)?.max_resolution);
  if (metaH != null && metaH >= UHD) return { text: '4K', cls: 'uhd' };

  // 3. Sticky localStorage flag, keyed by content name extracted from
  //    the master URL. v3 grid sets this when it confirms 4K once.
  try {
    const url =
      player.value?.current_play?.manifest?.master_url ??
      raw.master_manifest_url ??
      raw.manifest_url ??
      '';
    const m = String(url).match(/\/go-live\/([^/?#]+)/);
    if (m && m[1]) {
      const flag = localStorage.getItem('ismKnown4k:' + m[1]);
      if (flag === 'true') return { text: '4K', cls: 'uhd' };
    }
  } catch {
    /* localStorage can throw in private/quota — ignore */
  }
  return null;
});

const groupBadge = computed<Badge | null>(() => {
  const raw = (player.value as any)?.raw_session;
  const gid = raw?.group_id;
  if (typeof gid === 'string' && gid.length) {
    return { text: `group ${gid.slice(0, 8)}`, cls: 'group', title: gid };
  }
  return null;
});

const badges = computed<Badge[]>(() => {
  return [protoBadge.value, engineBadge.value, codecBadge.value, segmentBadge.value, uhdBadge.value, groupBadge.value]
    .filter((b): b is Badge => b != null);
});
</script>

<template>
  <div v-if="badges.length" class="panel-badges">
    <span
      v-for="(b, i) in badges"
      :key="i"
      class="badge"
      :class="b.cls"
      :title="b.title"
    >
      {{ b.text }}
    </span>
  </div>
</template>

<style scoped>
.panel-badges {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.badge {
  display: inline-flex;
  align-items: center;
  padding: 2px 10px;
  border-radius: 12px;
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  background: #f3f4f6;
  color: #374151;
}
.badge.hls { background: #fef3c7; color: #92400e; }
.badge.dash { background: #e0e7ff; color: #312e81; }
.badge.hlsjs { background: #e8f0fe; color: #1a73e8; }
.badge.shaka { background: #f3e8ff; color: #6b21a8; }
.badge.videojs { background: #fce7f3; color: #9d174d; }
.badge.native { background: #e0f2fe; color: #0369a1; }
.badge.codec { background: #d1fae5; color: #065f46; }
.badge.segment { background: #fee2e2; color: #991b1b; }
.badge.uhd { background: #1f2937; color: #fde68a; }
.badge.group { background: #fde68a; color: #92400e; }
</style>
