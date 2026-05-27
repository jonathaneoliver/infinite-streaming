<script setup lang="ts">
/**
 * BandwidthChart.vue — bandwidth panel main chart. Mirrors the legacy
 * `mbps_*` family of series rendered above the bitrate chart:
 *
 *   Server-side measurements (from server_metrics):
 *     - mbps_shaper_rate     — kernel shaper rate ceiling
 *     - mbps_shaper_avg      — EWMA shaper throughput
 *     - mbps_transfer_rate   — instantaneous per-segment write rate
 *     - mbps_transfer_complete — average over last completed transfer
 *     - measured_mbps        — server-measured throughput on the wire
 *     - Limit (rate_mbps)    — currently-enforced shaper target
 *
 *   Player-side measurements (from player_metrics):
 *     - Player avg_network_bitrate — player-side EWMA
 *     - Player network_bitrate     — player-side instantaneous
 *     - Player Variant             — active video bitrate
 *
 *   Server-side rendition (from server_metrics):
 *     - Server Variant — rendition_mbps the server thinks the client picked
 *
 * Y-max is controlled by the panel-level BitrateChartPanelToolbar via
 * the shared chart-coordination state.
 */
import { computed, toRef } from 'vue';
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import { useChartCoordination } from '@/composables/useChartCoordination';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { PlayerRecord } from '@/repo/v2-repo';

const props = defineProps<{
  playerId: string;
  eventsStream: Stream<Record<string, unknown>>;
}>();
const coord = useChartCoordination(toRef(props, 'playerId'));
const yMax = computed(() => coord.state.bandwidthYMax);

const series: SeriesSpec[] = [
  {
    label: 'mbps_shaper_rate',
    color: '#0f766e',
    accessor: (p: PlayerRecord) => p.server_metrics?.mbps_shaper_rate ?? null,
    stepped: true,
  },
  {
    label: 'mbps_shaper_avg',
    color: '#0d9488',
    accessor: (p: PlayerRecord) => p.server_metrics?.mbps_shaper_avg ?? null,
  },
  {
    label: 'mbps_transfer_rate',
    color: '#f97316',
    accessor: (p: PlayerRecord) => p.server_metrics?.mbps_transfer_rate ?? null,
  },
  {
    label: 'mbps_transfer_complete',
    color: '#dc2626',
    accessor: (p: PlayerRecord) => p.server_metrics?.mbps_transfer_complete ?? null,
    stepped: true,
  },
  // `server_metrics.measured_mbps` was a vestigial pre-v2 alias that
  // the proxy never actually populated (the rename of the throughput
  // payload key from `"mbps"` to `"mbps_transfer_rate"` orphaned the
  // assignment in main.go applySessionThroughput). Other series in
  // this chart cover the same ground — drop instead of patching the
  // dead path. If the operator wants this measurement back, the new
  // canonical source is `mbps_transfer_rate` already plotted below.
  {
    label: 'Limit (rate_mbps)',
    color: '#f59e0b',
    // Mirror legacy session-shell.js:2397-2408 — the displayed "Limit"
    // is the *effective* shaper ceiling at this moment, which depends
    // on whether a pattern is running:
    //   1. Pattern enabled & runtime rate set → that's what the kernel
    //      is actually enforcing right now (`pattern_rate_runtime_mbps`)
    //   2. Otherwise, if a pattern step is active → that step's rate
    //   3. Otherwise → the static `shape.rate_mbps`
    //   4. Otherwise → 0 (no shaping configured; still draw the line
    //      so the operator can see "no ceiling enforced" rather than a
    //      missing series)
    accessor: (p: PlayerRecord) => {
      const sh = p.shape;
      if (!sh) return 0;
      const runtime = sh.pattern_rate_runtime_mbps;
      if (sh.pattern && Number.isFinite(runtime as number) && (runtime as number) >= 0) {
        return runtime as number;
      }
      const stepIdx = Number(sh.pattern_step_runtime ?? sh.pattern_step ?? 0);
      const steps = sh.pattern?.steps ?? [];
      if (stepIdx > 0 && stepIdx <= steps.length) {
        const r = Number(steps[stepIdx - 1]?.rate_mbps);
        if (Number.isFinite(r) && r >= 0) return r;
      }
      if (Number.isFinite(sh.rate_mbps as number)) return sh.rate_mbps as number;
      return 0;
    },
    stepped: true,
  },
  {
    label: 'Player avg_network_bitrate',
    color: '#6366f1',
    accessor: (p: PlayerRecord) => p.player_metrics?.avg_network_bitrate_mbps ?? null,
  },
  {
    label: 'Player network_bitrate',
    color: '#059669',
    accessor: (p: PlayerRecord) => p.player_metrics?.network_bitrate_mbps ?? null,
  },
  {
    label: 'Player Variant',
    color: '#ef4444',
    accessor: (p: PlayerRecord) => p.player_metrics?.video_bitrate_mbps ?? null,
    stepped: true,
  },
  {
    label: 'Server Variant',
    color: '#b45309',
    accessor: (p: PlayerRecord) => p.server_metrics?.rendition_mbps ?? null,
    stepped: true,
  },
];
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    title="Bandwidth"
    unit="Mbps"
    :series="series"
    :events-stream="eventsStream"
    :y-min="0"
    :y-max="yMax"
  />
</template>
