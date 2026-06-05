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
 *     - Serving Variant — rendition_mbps the server thinks the client picked
 *
 * Y-max is controlled by the panel-level BitrateChartPanelToolbar via
 * the shared chart-coordination state.
 */
import { computed, ref, toRef } from 'vue';
import MetricsLineChart, { type SeriesSpec } from './MetricsLineChart.vue';
import { useChartCoordination } from '@/composables/useChartCoordination';
import { useManifestVariants, nearestVariantByBitrate } from '@/composables/useManifestVariants';
import { usePlayer } from '@/composables/usePlayer';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { PlayerRecord } from '@/repo/v2-repo';

const props = defineProps<{
  playerId: string;
  eventsStream: Stream<Record<string, unknown>>;
  /** AVMetrics stream — used to overlay per-segment throughput dots
   *  on the bandwidth chart (issue #486). Optional so existing
   *  callers without the avmetrics scope (live testing.html
   *  pre-spike) can still mount the chart. */
  avmetricsStream?: Stream<Record<string, unknown>>;
}>();
const coord = useChartCoordination(toRef(props, 'playerId'));
const yMax = computed(() => coord.state.bandwidthYMax);
const { variants: usePlayerVariants } = useManifestVariants(toRef(props, 'playerId'));
const { player } = usePlayer(toRef(props, 'playerId'));

/** Per-segment markers — OFF by default. Operator opts in via the
 *  synthetic legend chip in MetricsLineChart (issue #486). The chip
 *  always shows when avmetrics data is present so it's discoverable. */
const segmentMarkersVisible = ref(false);

interface ManifestVariantLite {
  url?: string;
  bandwidth?: number;
  average_bandwidth?: number;
  resolution?: string;
}

/** Most-recent non-empty `manifest_variants` value from the events
 *  stream. SessionDisplay passes `archivePlayerId` to BandwidthChart;
 *  for live testing.html that synthesises a separate scope from the
 *  raw `player_id`, and `usePlayer(archivePlayerId)` doesn't surface
 *  the manifest because the archive record is built without it. The
 *  events stream rows DO carry `manifest_variants` (it's part of the
 *  charts_minimal projection), so read from there. Issue #486. */
const eventsStreamVariants = computed<ManifestVariantLite[]>(() => {
  void props.eventsStream.version.value;
  const rows = props.eventsStream.inRange(0, Number.MAX_SAFE_INTEGER);
  for (let i = rows.length - 1; i >= 0; i--) {
    const mv = (rows[i] as Record<string, unknown>).manifest_variants;
    if (Array.isArray(mv) && mv.length) return mv as ManifestVariantLite[];
    if (typeof mv === 'string' && mv.length > 0 && mv !== 'null') {
      try {
        const parsed = JSON.parse(mv);
        if (Array.isArray(parsed) && parsed.length) return parsed as ManifestVariantLite[];
      } catch { /* ignore */ }
    }
  }
  return [];
});

const variants = computed<ManifestVariantLite[]>(() => {
  const fromPlayer = usePlayerVariants.value;
  if (Array.isArray(fromPlayer) && fromPlayer.length) return fromPlayer as ManifestVariantLite[];
  return eventsStreamVariants.value;
});

/** Per-segment throughput overlay (issue #486). For every completed
 *  HLS media-segment fetch the iOS player publishes via AVMetrics, we
 *  computed `derived_mbps` on the client side. Drop a dot at
 *  (event_ts_ms, derived_mbps) so the operator can see real per-
 *  request throughput overlaid on the heartbeat-averaged line. Colors
 *  by event type — segment dots are slate, playlist dots blue, key
 *  fetches orange — so the role of each request is visible at a glance. */
// AVMetrics is iOS-AVPlayer-only. Gate both the data AND the legend
// label so non-iOS players don't see "Per-segment throughput (AVMetrics)"
// at all — MetricsLineChart renders the legend entry whenever the label
// prop is non-empty, regardless of whether there's data.
const isAVPlayerForMarkers = computed(() => player.value?.player_metrics?.player_tech === 'AVPlayer');
const segmentMarkersLabel = computed(() => isAVPlayerForMarkers.value ? 'Per-segment throughput (AVMetrics)' : '');

const segmentMarkers = computed(() => {
  if (!isAVPlayerForMarkers.value) return [];
  const stream = props.avmetricsStream;
  if (!stream) return [];
  void stream.version.value;
  const rows = stream.inRange(0, Number.MAX_SAFE_INTEGER);
  const out: Array<{ x: number; y: number; color?: string; label?: string }> = [];
  for (const row of rows) {
    const type = String(row.event_type ?? '');
    // Video segments only (issue #486). Skip playlists, DRM keys,
    // and audio/subtitle segments so the operator sees per-video-
    // segment throughput without mixing in playlist refreshes (1-2 ms
    // each) or audio fetches that aren't bandwidth-relevant.
    if (!type.includes('HLSMediaSegment')) continue;
    const ts = Number(row.event_ts_ms ?? 0);
    if (!Number.isFinite(ts) || ts <= 0) continue;
    let mbps: number | null = null;
    let label = '';
    const rawJson = row.raw_json;
    if (typeof rawJson === 'string' && rawJson.length > 2) {
      try {
        const parsed = JSON.parse(rawJson) as Record<string, unknown>;
        // AVMediaType is an NSString-typed const; Apple's `vide`
        // 4cc is what backs `AVMediaTypeVideo`. Reflective dump
        // returns the underlying string. Match defensively against
        // the 4cc and the descriptive name in case Apple ever
        // changes the surface.
        const mediaType = String(parsed.mediaType ?? '');
        if (!/vide|video/i.test(mediaType)) continue;
        const v = Number(parsed.derived_mbps);
        if (Number.isFinite(v)) mbps = v;
        const cached = String(parsed.derived_from_cache ?? '0');
        if (cached === '1') continue; // skip cache-served requests
        // Tooltip body — short event-type label, the path
        // (filename only — full URL is too long for the floating
        // tooltip), and the derived bandwidth / transfer details.
        const shortType = type
          .replace(/^AVMetricHLS/, '')
          .replace(/^AVMetric/, '')
          .replace(/RequestEvent$/, '');
        const url = String(parsed.url ?? '');
        const filename = url ? (url.split('?')[0].split('/').pop() ?? '') : '';
        const bytes = Number(parsed.derived_bytes);
        const transferMs = Number(parsed.derived_transfer_ms);
        const ttfbMs = Number(parsed.derived_ttfb_ms);
        const d = new Date(ts);
        const hms = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}.${String(d.getMilliseconds()).padStart(3, '0')}`;
        const lines = [
          `${shortType} · ${hms}`,
          filename ? filename : '',
          mbps != null ? `${mbps.toFixed(2)} Mbps` : '',
          Number.isFinite(bytes) && bytes > 0 ? `${bytes.toLocaleString()} bytes` : '',
          Number.isFinite(transferMs) && transferMs > 0 ? `${transferMs.toFixed(0)} ms body transfer` : '',
          Number.isFinite(ttfbMs) && ttfbMs > 0 ? `${ttfbMs.toFixed(1)} ms TTFB` : '',
        ].filter(Boolean);
        label = lines.join('\n');
      } catch { /* fall through */ }
    }
    if (mbps == null) continue;
    out.push({ x: ts, y: mbps, color: colorForRequestType(type), label });
  }
  return out;
});

function colorForRequestType(type: string): string {
  if (type.includes('HLSMediaSegment')) return '#475569'; // slate — bulk segments
  if (type.includes('HLSPlaylist'))     return '#0ea5e9'; // sky   — playlists
  if (type.includes('ContentKey'))      return '#ea580c'; // orange — DRM keys
  return '#94a3b8';                                       // mute fallback
}

const baseSeries: SeriesSpec[] = [
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
    //
    // NB: this series reflects OPERATOR OVERRIDE only — when the slider
    // is at 0 ("no override") the line drops to 0 because there is no
    // operator-imposed limit. The deployment baseline is plotted by
    // the separate "Effective Limit" series (hidden by default; toggle
    // via legend). Issue #480.
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
    // Effective Limit — kernel-enforced cap at this instant. Resolves
    // in priority order:
    //   1. Pattern step runtime (when a pattern is enabled and running).
    //   2. Operator slider (when set >0).
    //   3. Deployment baseline.
    // 0 only when truly uncapped (all three sources at 0). Tracks both
    // slider AND active pattern step — distinct from "Limit (rate_mbps)"
    // above which reflects operator INTENT only (slider when set, else
    // the pattern's runtime, else static). Off by default — the operator
    // enables it when investigating "what is the kernel actually
    // enforcing right now?"
    //
    // First-class CH column (issue #480; pattern fold-in added in
    // follow-up): proxy stamps effective_rate_limit_mbps on every
    // normalize, forwarder writes it to session_events, charts_minimal
    // exposes it, chRowAdapter projects it onto raw_session.
    // Historically accurate — reflects the cap AT THE TIME of the
    // archive sample, not today's.
    label: 'Effective Limit',
    color: '#dc2626',
    hidden: true,
    accessor: (p: PlayerRecord) => {
      const eff = (p as any).raw_session?.effective_rate_limit_mbps;
      if (Number.isFinite(eff) && (eff as number) > 0) return eff as number;
      return null;
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
    label: 'Serving Variant',
    color: '#b45309',
    accessor: (p: PlayerRecord) => p.server_metrics?.rendition_mbps ?? null,
    stepped: true,
  },
];

/** Restored from the legacy chart (issue #486): one dashed horizontal
 *  line per variant in the manifest's ladder. AVG group uses
 *  `average_bandwidth` (the EXT-X-STREAM-INF AVERAGE-BANDWIDTH attr,
 *  representing typical body bitrate); PEAK group uses `bandwidth`
 *  (the EXT-X-STREAM-INF BANDWIDTH attr, the encoder's worst-case
 *  peak). Each variant becomes its own series so the line spans the
 *  whole X range; sharing a `groupLegend` collapses them all to a
 *  single legend chip that toggles every line at once.
 *
 *  Defaults: peak ON (the rate ABR actually keys on — see abr-ladder
 *  standard), avg OFF (toggle on for the typical-body-bitrate reference). */
const series = computed<SeriesSpec[]>(() => {
  const out = [...baseSeries];
  const ladder = variants.value;
  if (!ladder.length) return out;
  // "Fetching Variant": video_bitrate_mbps == AVPlayer indicatedBitrate ==
  // the rung it SELECTED to fetch (leads the screen by the buffer). It's a
  // jittery EWMA, so plot the nearest published peak instead of the raw
  // value — a clean stepped rung line, not a 29.6/29.9 wobble (#619).
  out.push({
    label: 'Fetching Variant',
    color: '#ef4444',
    accessor: (p: PlayerRecord) => {
      const vb = p.player_metrics?.video_bitrate_mbps;
      if (vb == null || vb <= 0) return null;
      return nearestVariantByBitrate(ladder, vb)?.peakMbps ?? vb;
    },
    stepped: true,
  });
  // "Displayed Variant": the same value that drives the player-state
  // "Video Res" line (player_metrics.video_resolution = the DECODED frame
  // size, presentationSize on iOS / videoWidth×videoHeight on web), plotted
  // as that variant's published peak BANDWIDTH per sample.
  //
  // Matched by NEAREST frame HEIGHT, not exact "WxH": the decoded size
  // legitimately differs from the manifest RESOLUTION attribute (coded vs
  // display, e.g. 1080 encoded as 1088 for mod-16; PAR / clean aperture;
  // packager quirks), so an exact string match would blank the line.
  // (We deliberately do NOT disambiguate with video_bitrate_mbps — that's
  // indicatedBitrate, i.e. the variant being FETCHED/selected, which leads
  // the displayed rung by the buffer; using it here would mislabel during
  // switches. Same-height variants therefore can't be told apart for the
  // displayed rung — this project's ladders have one rung per height.)
  const heightOf = (res?: string | null): number | null => {
    if (!res) return null;
    const m = /(\d+)\s*[x×]\s*(\d+)/i.exec(res);
    return m ? Number(m[2]) : null;
  };
  const rungs = ladder
    .map((v) => ({ h: heightOf(v.resolution), peak: Number(v.bandwidth) / 1_000_000 }))
    .filter((r) => r.h != null && Number.isFinite(r.peak) && r.peak > 0) as { h: number; peak: number }[];
  out.push({
    label: 'Displayed Variant',
    color: '#a855f7',
    accessor: (p: PlayerRecord) => {
      const h = heightOf(p.player_metrics?.video_resolution);
      if (h == null || !rungs.length) return null;
      let best = rungs[0];
      for (const r of rungs) {
        if (Math.abs(r.h - h) < Math.abs(best.h - h)) best = r;
      }
      return best.peak;
    },
    stepped: true,
  });
  // Mute the variant-line color so it doesn't out-shout the live
  // traces. Slate-400 reads at a glance but stays in the background.
  const AVG_COLOR = '#94a3b8';
  const PEAK_COLOR = '#cbd5e1';
  for (const v of ladder) {
    const avgBw = Number((v as any).average_bandwidth ?? v.bandwidth);
    if (Number.isFinite(avgBw) && avgBw > 0) {
      const mbps = avgBw / 1_000_000;
      const resLabel = v.resolution ?? '?';
      out.push({
        label: `Variant avg ${resLabel} (${mbps.toFixed(2)} Mbps)`,
        color: AVG_COLOR,
        accessor: () => mbps,
        stepped: false,
        borderDash: [2, 4],
        groupLegend: 'Variant avg bandwidth',
        hidden: true,
      });
    }
    const peakBw = Number(v.bandwidth);
    if (Number.isFinite(peakBw) && peakBw > 0) {
      const mbps = peakBw / 1_000_000;
      const resLabel = v.resolution ?? '?';
      out.push({
        label: `Variant peak ${resLabel} (${mbps.toFixed(2)} Mbps)`,
        color: PEAK_COLOR,
        accessor: () => mbps,
        stepped: false,
        borderDash: [6, 4],
        groupLegend: 'Variant peak bandwidth',
      });
    }
  }
  return out;
});
</script>

<template>
  <MetricsLineChart
    :player-id="playerId"
    title="Bandwidth"
    unit="Mbps"
    :series="series"
    :events-stream="eventsStream"
    :markers="segmentMarkers"
    :markers-label="segmentMarkersLabel"
    v-model:markers-visible="segmentMarkersVisible"
    :y-min="0"
    :y-max="yMax"
  />
</template>
