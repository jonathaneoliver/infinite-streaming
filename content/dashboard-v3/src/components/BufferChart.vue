<script setup lang="ts">
/**
 * BufferChart.vue — buffer depth on the LEFT axis; live offset and
 * true offset on the RIGHT axis (axis: 'y2' on each series). Separate
 * scales because offsets can run to minutes when the player drifts
 * while buffer depth is typically tens of seconds. Matches the
 * legacy two-axis layout.
 */
import { computed } from 'vue';
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import { useCompareOverlays, useCompareSelf } from '@/composables/useCompareContext';
import { compareBufferSeries } from '@/composables/compareSeries';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { PlayerRecord } from '@/repo/v2-repo';

defineProps<{
  playerId: string;
  /** Coordination scope key forwarded to MetricsLineChart (per-player, stable
   *  across plays). Falls back to playerId there when absent. */
  coordId?: string;
  eventsStream: Stream<Record<string, unknown>>;
}>();

/** Grouped-sibling buffer-depth overlays (issue #579). On the left ('y')
 *  axis so the buffer-depth scale sizes across every overlaid grouped
 *  session — the #165 requirement that a deeper-buffering sibling doesn't
 *  clip. Empty unless compare mode is on (resolved from CompareContext). */
const compareOverlays = useCompareOverlays(compareBufferSeries);
const compareSelf = useCompareSelf();

const baseSeries: SeriesSpec[] = [
  {
    label: 'Buffer depth (s)',
    color: '#4f46e5',
    accessor: (p: PlayerRecord) => p.player_metrics?.buffer_depth_s ?? null,
  },
  {
    label: 'Live offset (s)',
    color: '#f59e0b',
    accessor: (p: PlayerRecord) => p.player_metrics?.live_offset_s ?? null,
    axis: 'y2',
  },
  {
    label: 'True offset (s)',
    color: '#10b981',
    accessor: (p: PlayerRecord) => p.player_metrics?.true_offset_s ?? null,
    axis: 'y2',
  },
];
// Compare mode: active session shows the same canonical tagged set
// (buffer depth only, solid `S<id>`) the siblings overlay.
const series = computed<SeriesSpec[]>(() =>
  compareSelf.value ? compareBufferSeries(compareSelf.value) : baseSeries,
);
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    :coord-id="coordId"
    title="Buffer & live offset"
    unit="buffer (s)"
    :series="series"
    :events-stream="eventsStream"
    :overlays="compareOverlays"
    :y-min="0"
    y2-title="offset (s)"
    :y2-min="0"
  />
</template>
