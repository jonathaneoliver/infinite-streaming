/**
 * compareSeries — per-chart SeriesSpec builders for the compare-charts
 * overlay (issue #579). Each builder takes one grouped sibling and
 * returns its overlay series, reusing the SAME PlayerRecord accessors as
 * the primary single-session series so the overlaid values mean exactly
 * what the active session's lines mean.
 *
 * Visual encoding (operator's spec):
 *   - COLOUR encodes the METRIC, matching the primary chart's colour for
 *     that series — so "Player network_bitrate" is the same green on the
 *     active session and on every sibling, and the eye pairs them at a
 *     glance.
 *   - DASH encodes the SESSION — the active session is solid, each
 *     sibling gets a distinct dash pattern by its stable index — so two
 *     devices' same-colour lines stay tellable apart.
 *   - LABEL is the primary series' exact name + ` (Sx)`, so the legend
 *     reads identically across sessions bar the tag.
 *
 * The full set ships default-visible, mirroring the legacy testing.html
 * compare overlay. (Some players don't emit every field — e.g.
 * network_bitrate needs the client LocalProxy — so that line may be all
 * gaps; the others still render.)
 */
import type { SeriesSpec } from '@/components/MetricsLineChart.vue';
import type { CompareSeriesIdentity } from '@/composables/useCompareContext';
import type { PlayerRecord } from '@/repo/v2-repo';
import { displayedVariantPeakMbps, parseManifestVariants } from '@/composables/useManifestVariants';

/** Metric colours — kept in lockstep with the primary chart series so a
 *  metric reads the same hue across the active session and every overlay.
 *  Update alongside the matching `color:` in the chart components. */
const C = {
  // BandwidthChart
  shaperAvg: '#0d9488',
  playerAvgRate: '#6366f1',
  playerNetRate: '#059669',
  servingVariant: '#b45309',
  fetchingVariant: '#ef4444',
  displayedVariant: '#a855f7',
  limit: '#f59e0b',
  // BufferChart
  bufferDepth: '#4f46e5',
  liveOffset: '#f59e0b',
  trueOffset: '#10b981',
  // RTTChart
  rtt: '#4f46e5',
  rttMin: '#10b981',
  rttMax: '#ef4444',
  pathPing: '#f59e0b',
  rto: '#a855f7',
  // FPSChart
  displayedFps: '#10b981',
  droppedFps: '#ef4444',
  droppedTotal: '#7c2d12',
} as const;

/** Append the session tag to a primary series label, matching it across
 *  sessions: `Player network_bitrate` → `Player network_bitrate (S2)`. */
function tagged(label: string, id: CompareSeriesIdentity): string {
  return `${label} (${id.tag})`;
}

/** Stamp the session tag onto every series so MetricsLineChart can group
 *  them for the S1/S2 session legend (hover-highlight + show/hide a whole
 *  session). Without this the legend can't match any line to a session. */
function withTag(id: CompareSeriesIdentity, series: SeriesSpec[]): SeriesSpec[] {
  return series.map((s) => ({ ...s, sessionTag: id.tag }));
}

/** Effective enforced shaper ceiling at a sample — mirrors
 *  BandwidthChart's "Limit (rate_mbps)" accessor so each session's limit
 *  overlays with the same meaning: pattern runtime rate when a pattern is
 *  active, else the active step's rate, else the static rate, else 0. The
 *  sibling's charts_minimal projection carries the nftables_* fields the
 *  chRow adapter folds into p.shape. */
function limitAccessor(p: PlayerRecord): number | null {
  const sh = p.shape as {
    rate_mbps?: number | null;
    pattern?: { steps?: { rate_mbps?: number }[] } | null;
    pattern_rate_runtime_mbps?: number | null;
    pattern_step?: number | null;
    pattern_step_runtime?: number | null;
  } | undefined;
  if (!sh) return 0;
  const runtime = sh.pattern_rate_runtime_mbps;
  // Use the kernel's enforced runtime rate whenever it's set — not only when THIS
  // session owns a pattern. A group-driven slave (single-owner shaping) has no
  // local pattern but its port is fanned the master's per-tick rate via
  // pattern_rate_runtime_mbps, so this lets the slave's Limit line track the
  // master's pyramid. Safe for normal no-pattern sessions: the proxy defaults
  // runtime to bandwidth_mbps (== rate_mbps), so the line is unchanged. Mirrors
  // the same fix in BandwidthChart.vue's single-session accessor.
  if (Number.isFinite(runtime as number) && (runtime as number) >= 0) {
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
}

/**
 * Bandwidth (rate) chart overlay — the primary comparison target. Only
 * the series the active chart shows BY DEFAULT are overlaid; lines that
 * are hidden-by-default there (Player network_bitrate, Serving Variant)
 * are omitted entirely rather than shipped as legend clutter. All on the
 * left ('y') axis so they auto-size alongside the active session's own
 * lines (#165 union).
 */
export function compareBandwidthSeries(id: CompareSeriesIdentity): SeriesSpec[] {
  const dash = id.dash;
  return withTag(id, [
    {
      label: tagged('Player avg_network_bitrate', id),
      color: C.playerAvgRate,
      accessor: (p: PlayerRecord) => p.player_metrics?.avg_network_bitrate_mbps ?? null,
      borderDash: dash,
    },
    {
      label: tagged('Fetching Variant', id),
      color: C.fetchingVariant,
      accessor: (p: PlayerRecord) => p.player_metrics?.video_bitrate_mbps ?? null,
      stepped: true,
      borderDash: dash,
    },
    {
      // Decoded rung's published peak bitrate, matched by frame height —
      // mirrors the single-session "Displayed Variant" (BandwidthChart). The
      // sibling's ladder rides on raw_session.manifest_variants (#747).
      label: tagged('Displayed Variant', id),
      color: C.displayedVariant,
      accessor: (p: PlayerRecord) =>
        displayedVariantPeakMbps(
          parseManifestVariants((p as { raw_session?: { manifest_variants?: unknown } }).raw_session?.manifest_variants),
          p.player_metrics?.video_resolution,
        ),
      stepped: true,
      borderDash: dash,
    },
    {
      label: tagged('Limit (rate_mbps)', id),
      color: C.limit,
      accessor: limitAccessor,
      stepped: true,
      borderDash: dash,
    },
    {
      label: tagged('mbps_shaper_avg', id),
      color: C.shaperAvg,
      accessor: (p: PlayerRecord) => p.server_metrics?.mbps_shaper_avg ?? null,
      borderDash: dash,
    },
  ]);
}

/**
 * Buffer chart overlay — buffer depth per session on the left ('y') axis
 * (sized across all sessions, #165), plus live + true offset on the right
 * ('y2') axis. Several sessions' offset lines share the y2 scale, which
 * auto-sizes across them, so `Live offset (S1)` / `Live offset (S2)` /
 * `True offset (…)` all read on the right axis at once.
 */
export function compareBufferSeries(id: CompareSeriesIdentity): SeriesSpec[] {
  const dash = id.dash;
  return withTag(id, [
    {
      label: tagged('Buffer depth (s)', id),
      color: C.bufferDepth,
      accessor: (p: PlayerRecord) => p.player_metrics?.buffer_depth_s ?? null,
      borderDash: dash,
    },
    {
      label: tagged('Live offset (s)', id),
      color: C.liveOffset,
      accessor: (p: PlayerRecord) => p.player_metrics?.live_offset_s ?? null,
      borderDash: dash,
      axis: 'y2',
    },
    {
      label: tagged('True offset (s)', id),
      color: C.trueOffset,
      accessor: (p: PlayerRecord) => p.player_metrics?.true_offset_s ?? null,
      borderDash: dash,
      axis: 'y2',
    },
  ]);
}

/**
 * RTT chart overlay — the TCP_INFO RTT family per sibling (left 'y'
 * axis, ms) plus RTO on the right ('y2') axis, mirroring the primary
 * layout. TTFB (client) is omitted: it's iOS-AVPlayer-only (AVMetrics)
 * and we don't carry the sibling's player_tech here, so it would just
 * add an always-empty line for non-iOS peers.
 */
export function compareRttSeries(id: CompareSeriesIdentity): SeriesSpec[] {
  const dash = id.dash;
  return withTag(id, [
    {
      label: tagged('RTT (ms)', id),
      color: C.rtt,
      accessor: (p: PlayerRecord) => p.server_metrics?.rtt_ms ?? null,
      borderDash: dash,
    },
    {
      label: tagged('RTT min (ms)', id),
      color: C.rttMin,
      accessor: (p: PlayerRecord) => p.server_metrics?.rtt_min_ms ?? null,
      borderDash: dash,
    },
    {
      label: tagged('RTT max (ms)', id),
      color: C.rttMax,
      accessor: (p: PlayerRecord) => p.server_metrics?.rtt_max_ms ?? null,
      borderDash: dash,
    },
    {
      label: tagged('Path ping (ms)', id),
      color: C.pathPing,
      accessor: (p: PlayerRecord) => p.server_metrics?.path_ping_rtt_ms ?? null,
      borderDash: dash,
    },
    {
      label: tagged('RTO (ms)', id),
      color: C.rto,
      accessor: (p: PlayerRecord) => p.server_metrics?.rto_ms ?? null,
      borderDash: dash,
      axis: 'y2',
    },
  ]);
}

/** Parse a synthetic PlayerRecord's sample timestamp (ms) from the
 *  event_time the chRow adapter projects onto player_metrics. Accepts
 *  the CH "YYYY-MM-DD HH:MM:SS.fff" form (UTC, no Z) and ISO-8601. */
function tsOfRecord(p: PlayerRecord): number {
  const et = p.player_metrics?.event_time;
  if (typeof et !== 'string' || !et) return NaN;
  if (et.length > 10 && et.charAt(10) === ' ') return Date.parse(et.replace(' ', 'T') + 'Z');
  return Date.parse(et);
}

/**
 * Build a stateful per-sibling rate accessor: instantaneous Δcount/Δt of
 * a monotonic counter (frames displayed / dropped). FPSChart derives
 * these per-component from the primary stream, so a stateless overlay
 * accessor would mis-plot the active session's FPS against the sibling's
 * samples. This closure keeps its OWN prev (count, ts, play) — resetting
 * on the sibling's play rotation, exactly like the primary chart — so
 * each overlaid device shows its own frame rate. One closure per sibling
 * (the SeriesSpec set is rebuilt only when the sibling list changes).
 */
function makeCounterRate(read: (p: PlayerRecord) => number | null): (p: PlayerRecord) => number | null {
  let prevVal: number | null = null;
  let prevTs: number | null = null;
  let prevPlay: string | null = null;
  return (p: PlayerRecord) => {
    const t = tsOfRecord(p);
    const v = read(p);
    const play = p.current_play?.id ?? null;
    // New play → reseat the baseline, emit no rate for the first sample
    // (avoids a spurious huge delta against the prior play's counter).
    if (play !== prevPlay) {
      prevPlay = play;
      prevVal = v;
      prevTs = Number.isFinite(t) ? t : null;
      return null;
    }
    let out: number | null = null;
    if (prevTs != null && prevVal != null && v != null && Number.isFinite(t) && t > prevTs) {
      const dt = (t - prevTs) / 1000;
      out = dt > 0 ? Math.max(0, (v - prevVal) / dt) : null;
    }
    if (Number.isFinite(t)) { prevVal = v; prevTs = t; }
    return out;
  };
}

/**
 * FPS chart overlay — derived displayed/dropped FPS per sibling (left
 * 'y' axis) + the raw dropped-frames counter (right 'y2' axis). The two
 * derived series each own their delta state via makeCounterRate.
 */
export function compareFpsSeries(id: CompareSeriesIdentity): SeriesSpec[] {
  const dash = id.dash;
  return withTag(id, [
    {
      label: tagged('Displayed FPS', id),
      color: C.displayedFps,
      accessor: makeCounterRate((p) => p.player_metrics?.frames_displayed ?? null),
      borderDash: dash,
    },
    {
      label: tagged('Dropped FPS', id),
      color: C.droppedFps,
      accessor: makeCounterRate((p) => p.player_metrics?.frames_dropped ?? null),
      borderDash: dash,
    },
    {
      label: tagged('Dropped frames (total)', id),
      color: C.droppedTotal,
      accessor: (p: PlayerRecord) => p.player_metrics?.frames_dropped ?? null,
      stepped: true,
      borderDash: dash,
      axis: 'y2',
    },
  ]);
}
