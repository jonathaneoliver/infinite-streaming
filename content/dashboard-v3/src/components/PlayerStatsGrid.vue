<script setup lang="ts">
/**
 * PlayerStatsGrid.vue — read-only 9-tile stats grid below the player
 * frame: State · Time · Buffered End · Buffer Depth · Live Offset ·
 * Bitrate · SSE Missed · Dropped Frames · Stalls. Mirrors the legacy
 * `.stats-grid` block.
 *
 * `sseMissed` is reported by the parent (VideoPlayerFrame holds the
 * EventSource); everything else comes from PlayerMetrics on the
 * current PlayerRecord.
 */
import { computed, toRef } from 'vue';
import { usePlayer } from '@/composables/usePlayer';

const props = defineProps<{
  playerId: string;
  sseMissed?: number;
}>();

const { player } = usePlayer(toRef(props, 'playerId'));
const pm = computed(() => player.value?.player_metrics ?? null);

function fmtSec(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  if (Math.abs(v) >= 60) {
    const m = Math.floor(v / 60);
    const s = Math.floor(v - m * 60);
    return `${m}:${String(s).padStart(2, '0')}`;
  }
  return `${v.toFixed(2)}s`;
}

function fmtMbps(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${v.toFixed(2)} Mbps`;
}

const stats = computed(() => [
  { label: 'State', value: pm.value?.state ?? 'Idle' },
  { label: 'Time', value: fmtSec(pm.value?.position_s) },
  { label: 'Buffered End', value: fmtSec(pm.value?.buffer_end_s) },
  { label: 'Buffer Depth', value: fmtSec(pm.value?.buffer_depth_s) },
  { label: 'Live Offset', value: fmtSec(pm.value?.live_offset_s) },
  { label: 'Bitrate', value: fmtMbps(pm.value?.video_bitrate_mbps) },
  { label: 'SSE Missed', value: props.sseMissed ?? 0 },
  { label: 'Dropped Frames', value: pm.value?.frames_dropped ?? 0 },
  { label: 'Stalls', value: pm.value?.stalls ?? 0 },
]);
</script>

<template>
  <div class="stats-grid">
    <div v-for="s in stats" :key="s.label" class="tile">
      <span class="label">{{ s.label }}</span>
      <span class="value">{{ s.value }}</span>
    </div>
  </div>
</template>

<style scoped>
.stats-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
  gap: 8px;
  margin-top: 12px;
}
.tile {
  background: #f8f9fa;
  border: 1px solid #e8eaed;
  border-radius: 6px;
  padding: 6px 10px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.label {
  font-size: 10px;
  color: #5f6368;
  text-transform: uppercase;
  letter-spacing: 0.5px;
}
.value {
  font-size: 14px;
  font-weight: 600;
  color: #202124;
  font-variant-numeric: tabular-nums;
}
</style>
