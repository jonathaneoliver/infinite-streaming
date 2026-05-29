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
  eventsStream: Stream<Record<string, unknown>>;
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
    // TTFB (client) — median `responseStart − requestEnd` over the
    // recent AVMetric MediaResourceRequest events on the iOS player
    // (issue #486). Honest naming: on HTTP/2 keep-alive this is
    // *stream-level* latency from URLSession's pipeline view — not
    // a wire-time RTT. Frame coalescing and multiplexing mean it
    // typically reads far below the server-side TCP_INFO RTT.
    //
    // The gap between this and `client_rtt_ms` is the diagnostic:
    // closer together means the path is healthy, divergence means
    // something between the player's URLSession and the proxy's
    // socket (proxy buffering, HTTP/2 stream queueing, etc.) is
    // adding latency. Dashed so it reads as "derived / per-request"
    // versus the smoothed server-side traces.
    label: 'TTFB (client, ms)',
    color: '#0ea5e9',
    accessor: (p: PlayerRecord) => p.server_metrics?.rtt_avmetrics_ms ?? null,
    borderDash: [4, 3],
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
    :events-stream="eventsStream"
    :y-min="0"
    y2-title="RTO (ms)"
    :y2-min="0"
  />
</template>
