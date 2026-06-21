<script setup lang="ts">
/**
 * RTTChart.vue — TCP_INFO RTT family + ICMP path ping on the left
 * y-axis (milliseconds). The TCP retransmit timeout (RTO) is much
 * larger than the RTT samples (a few hundred ms vs. handful of ms),
 * so it gets its own right-hand y-axis to keep the RTT detail
 * readable. Matches the legacy chart layout.
 */
import { computed, toRef } from 'vue';
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { PlayerRecord } from '@/repo/v2-repo';
import { usePlayer } from '@/composables/usePlayer';
import { useCompareOverlays, useCompareSelf } from '@/composables/useCompareContext';
import { compareRttSeries } from '@/composables/compareSeries';

const props = defineProps<{
  playerId: string;
  /** Coordination scope key forwarded to MetricsLineChart (per-player, stable
   *  across plays). Falls back to playerId there when absent. */
  coordId?: string;
  eventsStream: Stream<Record<string, unknown>>;
}>();

/** Grouped-sibling RTT overlays (issue #579). Empty unless compare mode
 *  is on; shares the ms 'y' axis (and 'y2' for RTO) so the scales size
 *  across every overlaid session. */
const compareOverlays = useCompareOverlays(compareRttSeries);
const compareSelf = useCompareSelf();

// AVMetrics is iOS-only (AVPlayer ≥ iOS 18); other players will never
// populate client_rtt_avmetrics_ms, so we hide the TTFB (client) line
// entirely on those platforms rather than rendering an empty series
// in the legend.
const { player } = usePlayer(toRef(props, 'playerId'));
const isAVPlayer = computed(() => {
  const tech = player.value?.player_metrics?.player_tech;
  return tech === 'AVPlayer';
});

const series = computed<SeriesSpec[]>(() => {
  // Compare mode: active session shows the same canonical tagged RTT set
  // (solid `S<id>`) the siblings overlay.
  if (compareSelf.value) return compareRttSeries(compareSelf.value);
  return [
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
  // TTFB (client) — iOS-AVPlayer-only (AVMetrics, issue #486). Median
  // `responseStart − requestEnd` over the recent MediaResourceRequest
  // events; stream-level latency from URLSession's pipeline view, not
  // a wire-time RTT. Hidden entirely for non-iOS platforms because the
  // field is always null on Roku / ExoPlayer / external HLS players,
  // so the legend would mislead.
  ...(isAVPlayer.value
    ? [{
        label: 'TTFB (client, ms)',
        color: '#0ea5e9',
        accessor: (p: PlayerRecord) => p.server_metrics?.rtt_avmetrics_ms ?? null,
        borderDash: [4, 3],
      } satisfies SeriesSpec]
    : []),
  {
    label: 'RTO (ms)',
    color: '#a855f7',
    accessor: (p: PlayerRecord) => p.server_metrics?.rto_ms ?? null,
    axis: 'y2',
  },
  ];
});
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    :coord-id="coordId"
    title="Round-trip time"
    unit="ms"
    :series="series"
    :events-stream="eventsStream"
    :overlays="compareOverlays"
    :y-min="0"
    y2-title="RTO (ms)"
    :y2-min="0"
  />
</template>
