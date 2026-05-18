<script setup lang="ts">
/**
 * BufferChart.vue — buffer depth on the LEFT axis; live offset and
 * true offset on the RIGHT axis (axis: 'y2' on each series). Separate
 * scales because offsets can run to minutes when the player drifts
 * while buffer depth is typically tens of seconds. Matches the
 * legacy two-axis layout.
 */
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { PlayerRecord } from '@/repo/v2-repo';

defineProps<{
  playerId: string;
  eventsStream: Stream<Record<string, unknown>>;
}>();

const series: SeriesSpec[] = [
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
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    title="Buffer & live offset"
    unit="buffer (s)"
    :series="series"
    :events-stream="eventsStream"
    :y-min="0"
    y2-title="offset (s)"
    :y2-min="0"
  />
</template>
