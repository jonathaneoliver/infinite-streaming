<script setup lang="ts">
/**
 * RTTChart.vue — TCP_INFO RTT family + ICMP path ping on the left
 * y-axis (milliseconds). The TCP retransmit timeout (RTO) is much
 * larger than the RTT samples (a few hundred ms vs. handful of ms),
 * so it gets its own right-hand y-axis to keep the RTT detail
 * readable. Matches the legacy chart layout.
 */
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { PlayerRecord } from '@/repo/v2-repo';

defineProps<{
  playerId: string;
  samplesStream: Stream<Record<string, unknown>>;
}>();

const series: SeriesSpec[] = [
  {
    label: 'RTT (ms)',
    color: '#4f46e5',
    accessor: (p: PlayerRecord) => p.server_metrics?.rtt_ms ?? null,
  },
  {
    label: 'RTT min (ms)',
    color: '#10b981',
    accessor: (p: PlayerRecord) => p.server_metrics?.rtt_min_ms ?? null,
  },
  {
    label: 'RTT max (ms)',
    color: '#ef4444',
    accessor: (p: PlayerRecord) => p.server_metrics?.rtt_max_ms ?? null,
  },
  {
    label: 'Path ping (ms)',
    color: '#f59e0b',
    accessor: (p: PlayerRecord) => p.server_metrics?.path_ping_rtt_ms ?? null,
  },
  {
    label: 'RTO (ms)',
    color: '#a855f7',
    accessor: (p: PlayerRecord) => p.server_metrics?.rto_ms ?? null,
    axis: 'y2',
  },
];
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    title="Round-trip time"
    unit="ms"
    :series="series"
    :samples-stream="samplesStream"
    :y-min="0"
    y2-title="RTO (ms)"
    :y2-min="0"
  />
</template>
