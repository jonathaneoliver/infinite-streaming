<script setup lang="ts">
/**
 * FPSChart.vue — derives instantaneous displayed-FPS and dropped-FPS
 * from the running counters in player_metrics. Stalls show as zero.
 *
 * The frames_displayed counter is monotonically non-decreasing per
 * play; we keep the previous (count, time) pair and divide by elapsed
 * wall time. Resets to zero on a new play (counter goes down).
 */
import { computed, ref, toRef, watch } from 'vue';
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import { usePlayer } from '@/composables/usePlayer';
import type { PlayerRecord } from '@/repo/v2-repo';

const props = defineProps<{ playerId: string }>();
const { player } = usePlayer(toRef(props, 'playerId'));

// per-instance state: previous frame counters + the derived rate to
// surface to MetricsLineChart through the synthetic accessor below.
const displayedFps = ref<number | null>(null);
const droppedFps = ref<number | null>(null);

let prevFrames: number | null = null;
let prevDropped: number | null = null;
let prevTs: number | null = null;
let prevPlayId: string | null = null;

watch(
  () => player.value,
  (p) => {
    if (!p) return;
    const pm = p.player_metrics;
    if (!pm) return;
    const t = (() => {
      if (pm.event_time) {
        const v = Date.parse(pm.event_time);
        if (Number.isFinite(v)) return v;
      }
      if (p.last_seen_at) {
        const v = Date.parse(p.last_seen_at);
        if (Number.isFinite(v)) return v;
      }
      return Date.now();
    })();
    const playId = p.current_play?.id ?? null;
    if (playId !== prevPlayId) {
      prevPlayId = playId;
      prevFrames = pm.frames_displayed ?? null;
      prevDropped = pm.dropped_frames ?? null;
      prevTs = t;
      displayedFps.value = null;
      droppedFps.value = null;
      return;
    }
    const fr = pm.frames_displayed ?? null;
    const dr = pm.dropped_frames ?? null;
    if (prevTs != null && prevFrames != null && fr != null && t > prevTs) {
      const dt = (t - prevTs) / 1000;
      displayedFps.value = dt > 0 ? Math.max(0, (fr - prevFrames) / dt) : null;
    }
    if (prevTs != null && prevDropped != null && dr != null && t > prevTs) {
      const dt = (t - prevTs) / 1000;
      droppedFps.value = dt > 0 ? Math.max(0, (dr - prevDropped) / dt) : null;
    }
    prevFrames = fr;
    prevDropped = dr;
    prevTs = t;
  },
  { immediate: true },
);

const series = computed<SeriesSpec[]>(() => [
  {
    label: 'Displayed FPS',
    color: '#10b981',
    accessor: (_p: PlayerRecord) => displayedFps.value,
  },
  {
    label: 'Dropped FPS',
    color: '#ef4444',
    accessor: (_p: PlayerRecord) => droppedFps.value,
  },
  {
    label: 'Dropped frames (total)',
    color: '#7c2d12',
    accessor: (p: PlayerRecord) => p.player_metrics?.dropped_frames ?? null,
    stepped: true,
    axis: 'y2',
  },
]);
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    title="Frame rate (derived)"
    unit="fps"
    :series="series"
    :window-seconds="180"
    :y-min="0"
    y2-title="dropped (count)"
    :y2-min="0"
  />
</template>
