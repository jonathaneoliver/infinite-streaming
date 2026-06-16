<script setup lang="ts">
/**
 * FPSChart.vue — derives instantaneous displayed-FPS and dropped-FPS
 * from the running counters in player_metrics. Stalls show as zero.
 *
 * The frames_displayed counter is monotonically non-decreasing per
 * play; we keep the previous (count, time) pair and divide by elapsed
 * wall time. Resets to zero on a new play (counter goes down).
 *
 * Reads from the unified samples stream rather than usePlayer so the
 * chart is fed by the same backfill+live source as every other chart;
 * the per-row delta state below mirrors what the stream watcher inside
 * MetricsLineChart does, but here it's exposed via a `__derived` field
 * the synthetic PlayerRecord adapter passes through.
 */
import { computed, ref, watch } from 'vue';
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import type { Stream } from '@/composables/useSessionTimeSeries';
import { tsOfRow } from '@/composables/chRowAdapter';
import type { PlayerRecord } from '@/repo/v2-repo';
import { useCompareOverlays, useCompareSelf } from '@/composables/useCompareContext';
import { compareFpsSeries } from '@/composables/compareSeries';

const props = defineProps<{
  playerId: string;
  eventsStream: Stream<Record<string, unknown>>;
}>();

/** Grouped-sibling FPS overlays (issue #579). Each sibling derives its
 *  own displayed/dropped FPS (stateful per-sibling accessors) so the
 *  overlay shows that device's frame rate, not the active session's. */
const compareOverlays = useCompareOverlays(compareFpsSeries);
const compareSelf = useCompareSelf();

// per-instance state: previous frame counters + the derived rate to
// surface to MetricsLineChart through the synthetic accessor below.
const displayedFps = ref<number | null>(null);
const droppedFps = ref<number | null>(null);

let prevFrames: number | null = null;
let prevDropped: number | null = null;
let prevTs: number | null = null;
let prevPlayId: string | null = null;
let lastSeenMs = -Infinity;

function num(v: unknown): number | null {
  if (v == null) return null;
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v === 'string') { const n = Number(v); return Number.isFinite(n) ? n : null; }
  return null;
}

watch(
  () => props.eventsStream.version.value,
  () => {
    const raw = props.eventsStream.inRange(
      lastSeenMs === -Infinity ? 0 : lastSeenMs + 1,
      Number.MAX_SAFE_INTEGER,
    );
    if (!raw.length) return;
    for (const row of raw) {
      const t = tsOfRow(row);
      if (!Number.isFinite(t) || t <= lastSeenMs) continue;
      lastSeenMs = t;
      const fr = num(row.frames_displayed);
      const dr = num(row.frames_dropped);
      const playId = typeof row.play_id === 'string' ? row.play_id : null;
      if (playId !== prevPlayId) {
        prevPlayId = playId;
        prevFrames = fr;
        prevDropped = dr;
        prevTs = t;
        displayedFps.value = null;
        droppedFps.value = null;
        continue;
      }
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
    }
  },
  { immediate: true },
);

// Reset on player swap so the next play's counters don't get a
// spurious huge delta against the previous player's last frame count.
watch(
  () => props.playerId,
  () => {
    prevFrames = prevDropped = prevTs = null;
    prevPlayId = null;
    lastSeenMs = -Infinity;
    displayedFps.value = null;
    droppedFps.value = null;
  },
);

const series = computed<SeriesSpec[]>(() => {
  // Compare mode: active session shows the same canonical tagged FPS set
  // (solid `S<id>`) the siblings overlay; its derived FPS comes from the
  // per-session makeCounterRate accessors, not the component refs below.
  if (compareSelf.value) return compareFpsSeries(compareSelf.value);
  return [
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
    accessor: (p: PlayerRecord) => p.player_metrics?.frames_dropped ?? null,
    stepped: true,
    axis: 'y2',
  },
  ];
});

</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    title="Frame rate (derived)"
    unit="fps"
    :series="series"
    :events-stream="eventsStream"
    :overlays="compareOverlays"
    :y-min="0"
    y2-title="dropped (count)"
    :y2-min="0"
  />
</template>
