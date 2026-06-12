<script setup lang="ts">
/**
 * MetricsLineChart.vue — streaming line chart with the same UX the
 * legacy session-shell.js charts had:
 *
 *   - Toolbar: Live toggle · Expand · zoom hint
 *   - Alt + wheel / drag = zoom (x-axis only)
 *   - Right-click drag = pan
 *   - Left click (no drag) = toggle pause
 *   - Auto-pause when the user pans or zooms while live
 *   - Cross-chart viewport sync: all charts on the same player share
 *     one paused flag + viewport + expanded state via the shared
 *     useChartCoordination(playerId) state
 *
 * Sample lifetime: history kept in a per-instance ring buffer, trimmed
 * to `windowMs * 2` ms on the older side so a zoom-out / pan-back still
 * has data to show. The chart never holds more than ~2× window worth.
 */
import { computed, inject, onBeforeUnmount, ref, toRef, watch, type PropType } from 'vue';
import { ensureChartJs } from '@/composables/useChartJs';
import { CompareContextKey } from '@/composables/useCompareContext';
import { useChartCoordination, fmtTickHMSms, DEFAULT_FOCUS_MS, type ChartViewport } from '@/composables/useChartCoordination';
import type { Stream } from '@/composables/useSessionTimeSeries';
import { tsOfRow, chRowToPlayerRecord } from '@/composables/chRowAdapter';
import type { PlayerRecord } from '@/repo/v2-repo';

export type SeriesAccessor = (p: PlayerRecord) => number | null | undefined;

export interface SeriesSpec {
  label: string;
  color: string;
  accessor: SeriesAccessor;
  stepped?: boolean;
  /** Which y-axis this series binds to. Defaults to 'y' (left). 'y2'
   *  enables the right axis — pair with `y2Title` on the chart to label
   *  it. */
  axis?: 'y' | 'y2';
  /** Hide the series by default. Renders in the legend so the operator
   *  can click to enable it; the dataset is just initially `hidden: true`
   *  in Chart.js terms. Useful for diagnostic series that would
   *  otherwise clutter the default view. */
  hidden?: boolean;
  /** Chart.js `borderDash` — e.g. `[4, 4]` for a dashed line. */
  borderDash?: number[];
  /** Collapse this series into a group-shared legend entry. All series
   *  sharing the same `groupLegend` string appear under a single
   *  legend item whose label is the group name; clicking it toggles
   *  visibility on every member. Useful for "the variant ladder" or
   *  any other set of N lines the operator thinks of as one
   *  conceptual overlay. Issue #486. */
  groupLegend?: string;
  /** Compare mode (issue #579): the session this series belongs to (its
   *  `Sx` tag). Lets the session-legend highlight / show / hide every line
   *  for one session at once across all charts. */
  sessionTag?: string;
}

/**
 * ChartOverlaySource — one grouped-sibling's series overlaid onto this
 * chart (issue #579 compare mode). Each source carries its OWN events
 * stream (a separate per-player SSE) and its OWN tagged SeriesSpec[];
 * the chart drains each source independently and renders its datasets
 * alongside the primary `series`, on the same axes (so they share — and
 * size — the y axis with the active session's lines, the #165 fix).
 *
 * Overlay datasets are drawn with `spanGaps: false` and explicit null
 * points for missing samples, so a sibling lacking a wire metric shows
 * a clean gap instead of an interpolated bridge.
 */
export interface ChartOverlaySource {
  /** Stable identity (the sibling's player UUID) — drives dataset reuse
   *  across reconciles so a sibling's accumulated history survives a
   *  membership change elsewhere in the group. */
  key: string;
  /** The sibling's events stream (charts_minimal projection). */
  eventsStream: Stream<Record<string, unknown>>;
  /** Tagged, per-member-coloured series to overlay for this sibling. */
  series: SeriesSpec[];
}

const props = defineProps({
  playerId: { type: String, required: true },
  title: { type: String, default: '' },
  unit: { type: String, default: '' },
  series: { type: Array as PropType<SeriesSpec[]>, required: true },
  yMin: { type: Number, default: 0 },
  yMax: { type: Number, default: undefined },
  /** Title for the right-hand y-axis (only used when at least one
   *  series has `axis: 'y2'`). */
  y2Title: { type: String, default: '' },
  y2Min: { type: Number, default: 0 },
  y2Max: { type: Number, default: undefined },
  /** Samples stream from SessionDisplay's useSessionTimeSeries model.
   *  Each row is one CH session_snapshots projection (charts_minimal
   *  bundle); the chart adapts it to the synthetic PlayerRecord shape
   *  the per-series accessors expect. */
  eventsStream: { type: Object as PropType<Stream<Record<string, unknown>>>, required: true },
  /** Optional overlay markers (issue #486). One dot per entry; rendered
   *  after datasets are drawn so they sit above the line series. Used by
   *  BandwidthChart for per-AVMetric-segment throughput points; any
   *  chart can pass any stream of {x, y, color, label} the same way. */
  markers: {
    type: Array as PropType<Array<{ x: number; y: number; color?: string; label?: string }>>,
    default: () => [],
  },
  /** Synthetic legend entry text. When set, a clickable chip appears at
   *  the end of the legend that toggles all markers on/off. */
  markersLabel: { type: String, default: '' },
  /** Marker visibility (v-model). Default true. */
  markersVisible: { type: Boolean, default: true },
  /** Grouped-sibling overlays (issue #579 compare mode). Each entry is
   *  one sibling's stream + tagged series; drained independently and
   *  rendered alongside the primary `series` on shared axes. Empty in
   *  the normal single-session case — the primary path is untouched. */
  overlays: {
    type: Array as PropType<ChartOverlaySource[]>,
    default: () => [],
  },
});

const emit = defineEmits<{
  (e: 'update:markersVisible', value: boolean): void;
}>();

const canvas = ref<HTMLCanvasElement | null>(null);
const wrap = ref<HTMLDivElement | null>(null);
const canvasWrap = ref<HTMLDivElement | null>(null);
const coord = useChartCoordination(toRef(props, 'playerId'));

// Compare-mode session legend (issue #579). When mounted inside a
// SessionDisplay that's in compare mode, the S1/S2 chips drive this
// shared view; we highlight / show / hide this chart's datasets by their
// `_sessionTag` in lockstep with every other chart. Null outside compare.
const compareCtx = inject(CompareContextKey, null);

let chart: any = null;
let dataset: Array<Array<{ x: number; y: number }>> = [];

/** Hard cap on points kept per series (issue #582). A pure memory
 *  bound: the oldest points are dropped once a series exceeds this, so a
 *  tab open for hours can't grow the renderer to multiple GB. Doubled to
 *  16000 (~4.4 h at 1 Hz) to match the doubled cache cap so the charts
 *  cover the same deep history while #587 (refetch) is blocked. It's the
 *  cache's eviction window, not zoom, that bounds how far back data goes. */
const MAX_POINTS_PER_SERIES = 16000;

/** Listeners attached to `window` outlive this component's canvas (which
 *  GCs when the chart is destroyed), so they must be removed on unmount
 *  or they leak the closures — and the chart/dataset they capture —
 *  every time a chart is torn down (e.g. switching the active session).
 *  Issue #582. Canvas-bound listeners GC with the canvas, but routing
 *  them here too is harmless and keeps teardown complete. */
const teardownFns: Array<() => void> = [];
function onGlobal(
  target: EventTarget,
  type: string,
  handler: (e: any) => void,
  opts?: AddEventListenerOptions | boolean,
) {
  target.addEventListener(type, handler as EventListener, opts);
  teardownFns.push(() => target.removeEventListener(type, handler as EventListener, opts as EventListenerOptions));
}
// Watermark of the latest CH row already pushed through the chart.
// Read by the markers-stream watcher to drain only NEW rows on each
// version bump (the cache holds the full backfill + live tail).
let lastIngestedMs = -Infinity;

/** Tolerance for "right edge is at the live sample" — matches the
 *  brush-drop-at-live heuristic in SessionDisplay. */
const LIVE_EDGE_TOLERANCE_MS = 2000;

/** Pan- / zoom-complete handler: pin the new range as a sticky
 *  viewport UNLESS the right edge has reached the live sample, in
 *  which case return to live tracking — drop viewport, preserve the
 *  span via liveSpanMs, stay unpaused. Mirrors the EventsTimeline
 *  rangechanged path so chart / lane behave identically. */
function applyViewportOrSnapToLive(min: number, max: number) {
  if (coord.isAtLiveEdge(max)) {
    coord.setLiveSpan(max - min);
    coord.setRange(null);
    return;
  }
  coord.setRange({ min, max });
}


/** Pick a "nice" tick interval based on the visible span. Snaps to
 *  one of the wall-clock-friendly steps so gridlines land on real
 *  minute / 10-second / 1-second boundaries; same pattern vis-timeline
 *  uses for its own time axis. */
const NICE_TICK_MS = [
  100, 250, 500, 1_000, 2_000, 5_000, 10_000, 15_000, 30_000,
  60_000, 2 * 60_000, 5 * 60_000, 10 * 60_000, 15 * 60_000, 30 * 60_000,
  60 * 60_000,
];
function pickTickStep(scale: any) {
  const span = (scale.max ?? 0) - (scale.min ?? 0);
  if (!Number.isFinite(span) || span <= 0) return;
  // Aim for ~10 visible gridlines. With the default 10-minute window
  // that lands on the 60_000ms (1-minute) entry; zoom in halves the
  // window and we step down to 30s, then 10s, etc. Zoom out widens to
  // 2-minute / 5-minute / 10-minute steps.
  const target = span / 10;
  let step = NICE_TICK_MS[NICE_TICK_MS.length - 1];
  for (const n of NICE_TICK_MS) {
    if (n >= target) { step = n; break; }
  }
  // Anchor ticks to wall-clock boundaries of the chosen step so
  // 1-minute gridlines actually land on the minute (etc.).
  const start = Math.ceil(scale.min / step) * step;
  const ticks: { value: number }[] = [];
  for (let t = start; t <= scale.max; t += step) ticks.push({ value: t });
  scale.ticks = ticks;
}

/** Axis label format: hides hours when the visible span is short
 *  enough to read minutes/seconds at a glance, switches to millisecond
 *  precision when the user is zoomed in below ~10 seconds. */
function fmtAxisTick(v: number, chart: any): string {
  const d = new Date(v);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  const ms = String(d.getMilliseconds()).padStart(3, '0');
  const span = (chart?.scales?.x?.max ?? 0) - (chart?.scales?.x?.min ?? 0);
  if (span > 0 && span < 10_000) return `${hh}:${mm}:${ss}.${ms}`;
  if (span < 60_000) return `${hh}:${mm}:${ss}`;
  return `${hh}:${mm}:${ss}`;
}

function applyViewport(v: ChartViewport) {
  // Defensive: between the constructor returning and Chart.js fully
  // wiring up `scales`, or between `destroy()` and our `chart = null`,
  // there are tiny windows where `chart.options.scales.x` is undefined.
  // Any throw here ends up as an "Unhandled error in watcher" warning,
  // so just bail safely if the chart isn't ready.
  if (!chart || !chart.options?.scales?.x) return;
  chart.options.scales.x.min = v.min;
  chart.options.scales.x.max = v.max;
}

function applyYMax(v: number | undefined) {
  if (!chart || !chart.options?.scales?.y) return;
  chart.options.scales.y.max = v;
  // #664: push the new scale to the rendered chart immediately ('none' =
  // no animation), otherwise the Y-max buttons don't take effect until some
  // other update fires.
  try { chart.update('none'); } catch { /* ignore */ }
}

// #664: re-apply Y-max live when the prop changes (the buttons set
// coord.bandwidthYMax → :y-max). Without this it's only read at chart init.
watch(() => props.yMax, (v) => applyYMax(v));

// Serialise init so concurrent watch callbacks don't each `new Chart()`
// on the same canvas (Chart.js throws "Canvas is already in use" the
// second time). The first call kicks off the script load + chart
// construction; later calls await the same promise.
let ensurePromise: Promise<any> | null = null;
async function ensure(): Promise<any> {
  if (chart) return chart;
  if (!canvas.value) return null;
  if (ensurePromise) return ensurePromise;
  ensurePromise = (async () => {
    const Chart = await ensureChartJs();
    if (!canvas.value || chart) return chart;
    return createChartInstance(Chart);
  })();
  return ensurePromise;
}

/** Build one Chart.js dataset object from a SeriesSpec. `dsKey` is a
 *  stable identity (`primary|<label>` for the active session's lines,
 *  `<siblingPlayerId>|<label>` for a compare overlay) used to reuse the
 *  same dataset object — and thus its accumulated `data` array — across
 *  reconciles. `spanGaps` is true for the primary series (legacy
 *  behaviour) and false for overlays so a sibling's missing samples
 *  render as gaps, not interpolated bridges (issue #579). */
function makeDsObj(
  dsKey: string,
  s: SeriesSpec,
  data: Array<{ x: number; y: number | null }>,
  spanGaps: boolean,
): any {
  return {
    label: s.label,
    borderColor: s.color,
    backgroundColor: s.color + '22',
    data,
    tension: 0,
    stepped: !!s.stepped,
    pointRadius: 0,
    borderWidth: 2,
    borderDash: s.borderDash ?? [],
    spanGaps,
    yAxisID: s.axis === 'y2' ? 'y2' : 'y',
    hidden: !!s.hidden,
    _groupLegend: s.groupLegend ?? null,
    _sessionTag: s.sessionTag ?? null,
    _dsKey: dsKey,
  };
}

/** Per-overlay runtime state (issue #579). One entry per grouped
 *  sibling: its current series spec, the backing {x,y} arrays Chart.js
 *  reads from, an ingest watermark, and the last-seen stream epoch (so a
 *  sibling's play rotation / refetch wipes and re-drains cleanly). */
interface OverlayRuntime {
  datasets: Array<Array<{ x: number; y: number | null }>>;
  watermark: number;
  epoch: number;
}
const overlayRuntime = new Map<string, OverlayRuntime>();

/** Build the primary (active-session) dataset objects, reconciling
 *  against whatever is already on the chart by `_dsKey` so a stable
 *  series keeps its accumulated history when the series list changes
 *  (e.g. manifest variants arriving late, issue #486). Also keeps the
 *  backing `dataset` cache 1:1 with these objects for pushSample. */
function buildPrimaryDatasetObjs(): any[] {
  const target = props.series;
  while (dataset.length < target.length) dataset.push([]);
  while (dataset.length > target.length) dataset.pop();
  const existing = chart?.data?.datasets ?? [];
  const byKey = new Map<string, any>();
  for (const d of existing) if (d._dsKey) byKey.set(d._dsKey, d);
  const out: any[] = [];
  for (let i = 0; i < target.length; i++) {
    const s = target[i];
    const dsKey = 'primary|' + s.label;
    const prev = byKey.get(dsKey);
    if (prev) {
      prev.borderColor = s.color;
      prev.backgroundColor = s.color + '22';
      prev.stepped = !!s.stepped;
      prev.borderDash = s.borderDash ?? [];
      prev.yAxisID = s.axis === 'y2' ? 'y2' : 'y';
      prev.spanGaps = true;
      prev._groupLegend = s.groupLegend ?? null;
      prev._sessionTag = s.sessionTag ?? null;
      dataset[i] = prev.data;
      out.push(prev);
    } else {
      const data: Array<{ x: number; y: number }> = [];
      dataset[i] = data;
      out.push(makeDsObj(dsKey, s, data, true));
    }
  }
  return out;
}

/** Build the compare-overlay dataset objects from `props.overlays`,
 *  reconciling by `<key>|<label>` so a sibling's lines (and history)
 *  survive a membership change. Prunes runtime for siblings that have
 *  left the group. Issue #579. */
function buildOverlayDatasetObjs(): any[] {
  const sources = props.overlays ?? [];
  const wantKeys = new Set(sources.map((s) => s.key));
  for (const key of [...overlayRuntime.keys()]) {
    if (!wantKeys.has(key)) overlayRuntime.delete(key);
  }
  const existing = chart?.data?.datasets ?? [];
  const byKey = new Map<string, any>();
  for (const d of existing) if (d._dsKey) byKey.set(d._dsKey, d);
  const out: any[] = [];
  for (const src of sources) {
    let rt = overlayRuntime.get(src.key);
    if (!rt) {
      rt = { datasets: [], watermark: -Infinity, epoch: src.eventsStream.epoch.value };
      overlayRuntime.set(src.key, rt);
    }
    while (rt.datasets.length < src.series.length) rt.datasets.push([]);
    while (rt.datasets.length > src.series.length) rt.datasets.pop();
    for (let i = 0; i < src.series.length; i++) {
      const s = src.series[i];
      const dsKey = src.key + '|' + s.label;
      const prev = byKey.get(dsKey);
      if (prev) {
        prev.borderColor = s.color;
        prev.backgroundColor = s.color + '22';
        prev.stepped = !!s.stepped;
        prev.borderDash = s.borderDash ?? [];
        prev.yAxisID = s.axis === 'y2' ? 'y2' : 'y';
        prev.spanGaps = false;
        prev._groupLegend = s.groupLegend ?? null;
        prev._sessionTag = s.sessionTag ?? null;
        rt.datasets[i] = prev.data;
        out.push(prev);
      } else {
        const data = rt.datasets[i] ?? [];
        rt.datasets[i] = data;
        out.push(makeDsObj(dsKey, s, data, false));
      }
    }
  }
  return out;
}

/** Recompose `chart.data.datasets` = [primary…, overlay…] from the
 *  current `series` + `overlays` props. The single owner of the dataset
 *  list; both the series-prop watcher and the overlays watcher route
 *  through here so the two halves never clobber each other. */
function rebuildAllDatasets() {
  if (!chart || !chart.data) return;
  const primary = buildPrimaryDatasetObjs();
  const overlay = buildOverlayDatasetObjs();
  chart.data.datasets = [...primary, ...overlay];
  // Re-apply the session-legend hidden state so freshly (re)built
  // datasets for a hidden session start hidden. No render here — the
  // safeChartUpdate below paints it. Issue #579.
  applySessionVisibility(false);
  safeChartUpdate();
}

/** Show/hide every dataset by its session's legend state (#579). When a
 *  session is toggled off in the S1/S2 chip row, all its lines hide on
 *  every chart at once. `doUpdate` is false when called from a path that
 *  renders anyway (rebuildAllDatasets). */
function applySessionVisibility(doUpdate = true) {
  if (!chart || !compareCtx) return;
  const hidden = compareCtx.view.hidden.value;
  let changed = false;
  chart.data.datasets.forEach((ds: any, idx: number) => {
    if (!ds._sessionTag) return;
    const shouldShow = !hidden.has(ds._sessionTag);
    if (chart.isDatasetVisible(idx) !== shouldShow) {
      chart.setDatasetVisibility(idx, shouldShow);
      changed = true;
    }
  });
  if (changed && doUpdate) { try { chart.update(); } catch { /* ignore */ } }
}

/** Hovering a session chip pops that session's lines and dims the rest
 *  (#579). Mirrors the per-series legend hover but keyed on _sessionTag
 *  so the whole session lights up across every chart. */
function applySessionHover() {
  if (!chart || !compareCtx) return;
  const hov = compareCtx.view.hovered.value;
  for (const ds of chart.data.datasets as any[]) {
    if (ds._origBorderWidth == null) ds._origBorderWidth = ds.borderWidth ?? 2;
    if (ds._origBorderColor == null) ds._origBorderColor = ds.borderColor;
    if (!hov) {
      ds.borderWidth = ds._origBorderWidth;
      ds.borderColor = ds._origBorderColor;
    } else if (ds._sessionTag === hov) {
      ds.borderWidth = (ds._origBorderWidth ?? 2) + 2;
      ds.borderColor = ds._origBorderColor;
    } else {
      ds.borderWidth = Math.max(1, (ds._origBorderWidth ?? 2) - 1);
      const oc = ds._origBorderColor;
      ds.borderColor = typeof oc === 'string' && oc.startsWith('#') && oc.length === 7 ? oc + '33' : oc;
    }
  }
  try { chart.update('none'); } catch { /* ignore */ }
}

if (compareCtx) {
  watch(() => compareCtx.view.hovered.value, () => applySessionHover());
  watch(() => compareCtx.view.hidden.value, () => applySessionVisibility());
}

/** Reconcile the live chart's dataset list against props.series
 *  (issue #486) — now delegates to rebuildAllDatasets so compare
 *  overlays (#579) are preserved when the primary series list changes. */
function syncDatasetsFromSeriesProp() {
  rebuildAllDatasets();
}

function createChartInstance(Chart: any): any {
  dataset = props.series.map(() => []);
  const initialViewport = coord.effectiveRange.value;
  const usesY2 = props.series.some((s) => s.axis === 'y2');

  // Pin chart plot-area edges across every chart in the panel so the
  // X axis lines up visually. Tight 60px left gutter (matches the
  // events timeline's narrowed label column) so the actual plot area
  // gets the maximum possible width. Charts WITHOUT a y2 axis still
  // reserve the same 60px on the right via layout.padding so they
  // align with the dual-axis charts beside them.
  const Y_WIDTH = 60;
  const Y2_WIDTH = 60;
  const pinYWidth = (scale: any) => { scale.width = Y_WIDTH; };
  const pinY2Width = (scale: any) => { scale.width = Y2_WIDTH; };
  chart = new Chart(canvas.value, {
    type: 'line',
    data: {
      // Straight line segments between samples — no curve fitting.
      // Stepped series keep tension 0 too. Smoothing implies
      // measurements that weren't taken; for instrumentation data,
      // straight-line is the truthful representation. makeDsObj stamps
      // `_dsKey` (primary|<label>) so later reconciles reuse these
      // objects — and their accumulated history — by identity, and
      // `_groupLegend` for the legend group-collapse logic (issue #486).
      datasets: props.series.map((s, i) => makeDsObj('primary|' + s.label, s, dataset[i], true)),
    },
    /**
     * Inline plugin: vertical "selected event" cursor.
     * Reads `coord.state.cursorMs` (set from SessionViewer prev/next)
     * and draws a single dashed line at that x-position so the
     * operator can see exactly where the selected event lines up
     * across every chart in the card. Drawn after the datasets so it
     * sits on top of the lines, but below the tooltip/legend.
     */
    plugins: [{
      id: 'overlayMarkers',
      afterDatasetsDraw(c: any) {
        if (!props.markersVisible) return;
        const list = props.markers;
        if (!Array.isArray(list) || list.length === 0) return;
        const sx = c.scales?.x;
        const sy = c.scales?.y;
        const area = c.chartArea;
        if (!sx || !sy || !area) return;
        const ctx = c.ctx;
        ctx.save();
        for (const m of list) {
          if (!Number.isFinite(m.x) || !Number.isFinite(m.y)) continue;
          if (m.x < sx.min || m.x > sx.max) continue;
          if (m.y < sy.min || m.y > sy.max) continue;
          const px = sx.getPixelForValue(m.x);
          const py = sy.getPixelForValue(m.y);
          ctx.beginPath();
          ctx.arc(px, py, 3, 0, Math.PI * 2);
          ctx.fillStyle = m.color ?? '#1f2937';
          ctx.fill();
          // Hairline border so the dot stands out on a same-colored line.
          ctx.strokeStyle = 'rgba(255,255,255,0.85)';
          ctx.lineWidth = 1;
          ctx.stroke();
        }
        ctx.restore();
      },
    }, {
      id: 'navCursorLine',
      afterDatasetsDraw(c: any) {
        const ms = coord.state.cursorMs;
        if (ms == null || !Number.isFinite(ms)) return;
        const sx = c.scales?.x;
        const sy = c.scales?.y;
        if (!sx || !sy) return;
        if (ms < sx.min || ms > sx.max) return;
        const x = sx.getPixelForValue(ms);
        const ctx = c.ctx;
        ctx.save();
        ctx.beginPath();
        ctx.moveTo(x, sy.top);
        ctx.lineTo(x, sy.bottom);
        ctx.lineWidth = 1.5;
        ctx.strokeStyle = '#1d4ed8';
        ctx.setLineDash([4, 3]);
        ctx.stroke();
        // Tiny down-arrow at the top of the line so the eye finds
        // the cursor immediately on dense charts.
        ctx.beginPath();
        ctx.setLineDash([]);
        ctx.moveTo(x - 4, sy.top);
        ctx.lineTo(x + 4, sy.top);
        ctx.lineTo(x, sy.top + 5);
        ctx.closePath();
        ctx.fillStyle = '#1d4ed8';
        ctx.fill();
        ctx.restore();
      },
    }],
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      interaction: { mode: 'nearest', intersect: false },
      layout: {
        padding: {
          // Reserve room on the right edge for the y2 axis. When no y2
          // is used, pad the same amount via layout so the chart area
          // ends at the same x-coordinate as charts that DO have y2.
          right: usesY2 ? 0 : Y2_WIDTH,
        },
      },
      plugins: {
        legend: {
          position: 'bottom',
          labels: {
            boxWidth: 10,
            font: { size: 11 },
            padding: 6,
            // Group-collapse (issue #486): datasets sharing a
            // `_groupLegend` value appear as ONE legend entry whose
            // text is the group name and whose `hidden` reflects "is
            // every member hidden?". The default generator emits one
            // entry per dataset; we walk those, rename the first per
            // group, and drop the rest.
            generateLabels(chart: any) {
              const defaults = (window as any).Chart?.defaults?.plugins?.legend?.labels?.generateLabels;
              const items = defaults ? defaults(chart) : [];
              const seen = new Set<string>();
              const out: any[] = [];
              for (const item of items) {
                const ds = chart.data.datasets[item.datasetIndex];
                const grp = ds?._groupLegend;
                if (!grp) { out.push(item); continue; }
                if (seen.has(grp)) continue;
                seen.add(grp);
                // "hidden" for the group entry: only true when EVERY
                // member is hidden. Any one visible → group reads
                // visible (not strike-through).
                const anyVisible = chart.data.datasets.some(
                  (d: any, idx: number) =>
                    d._groupLegend === grp && chart.isDatasetVisible(idx),
                );
                item.text = grp;
                item.hidden = !anyVisible;
                item.lineDash = ds.borderDash ?? [];
                out.push(item);
              }
              // Synthetic legend chip for the markers overlay (issue
              // #486). Rendered as a filled circle (no line) so it
              // reads as "dot overlay" not "line series". onClick
              // below toggles `props.markersVisible` via emit.
              if (props.markersLabel) {
                out.push({
                  text: props.markersLabel,
                  fillStyle: '#475569',
                  strokeStyle: '#475569',
                  lineWidth: 0,
                  pointStyle: 'circle',
                  hidden: !props.markersVisible,
                  datasetIndex: -1,
                  _isMarkerToggle: true,
                });
              }
              return out;
            },
          },
          // Group-aware onClick. For ungrouped datasets, use the
          // stock toggle. For grouped datasets, flip visibility on
          // every member so one click controls the whole overlay.
          onClick(_e: any, item: any, legend: any) {
            const ci = legend.chart;
            // Marker overlay toggle — synthetic legend entry; flip
            // visibility on the parent's v-model state and force a
            // repaint. Issue #486.
            if (item?._isMarkerToggle) {
              emit('update:markersVisible', !props.markersVisible);
              ci.update();
              return;
            }
            const ds = ci.data.datasets[item.datasetIndex];
            const grp = ds?._groupLegend;
            if (!grp) {
              ci.setDatasetVisibility(item.datasetIndex, !ci.isDatasetVisible(item.datasetIndex));
              ci.update();
              return;
            }
            const anyVisible = ci.data.datasets.some(
              (d: any, idx: number) =>
                d._groupLegend === grp && ci.isDatasetVisible(idx),
            );
            ci.data.datasets.forEach((d: any, idx: number) => {
              if (d._groupLegend === grp) ci.setDatasetVisibility(idx, !anyVisible);
            });
            ci.update();
          },
          // Hover-highlight: when the cursor is over a legend label,
          // make that dataset's line POP and dim every other line so the
          // user can isolate it without clicking through visibility
          // toggles. Restores everything on leave. We stash the
          // original border width on the dataset itself so we can
          // restore precisely even after consecutive hovers across
          // different items.
          onHover(_evt: any, item: any, leg: any) {
            const c = leg?.chart;
            if (!c || typeof item?.datasetIndex !== 'number') return;
            // The synthetic markers chip carries datasetIndex -1 (a number, so
            // it slips past the guard above) and _isMarkerToggle. It maps to no
            // real dataset, so the highlight logic below would dim EVERY line
            // (highlighted = {-1} matches nothing) and leave them greyed while
            // the cursor rests on the chip. The overlay toggle is orthogonal to
            // line focus — skip hover-highlight for it entirely (issue #486/#579).
            if (item.datasetIndex < 0 || item._isMarkerToggle) return;
            const hovered = item.datasetIndex;
            // Compare mode (#579): also give a medium highlight to the
            // SAME metric on other sessions — hovering `Fetching Variant
            // (S2)` lifts `Fetching Variant (S1)` too, just less. Metric
            // identity is the label with its trailing ` (Sx)` stripped.
            const stripTag = (l: string) => l.replace(/\s*\(S[^)]*\)\s*$/, '');
            const hoveredDs0 = c.data.datasets[hovered];
            const sameMetric = new Set<number>();
            if (hoveredDs0?._sessionTag) {
              const mk = stripTag(hoveredDs0.label ?? '');
              c.data.datasets.forEach((ds: any, i: number) => {
                if (i !== hovered && ds._sessionTag && stripTag(ds.label ?? '') === mk) {
                  sameMetric.add(i);
                }
              });
            }
            // If the hovered legend entry represents a group (issue
            // #486 — `Variant avg bandwidth` / `Variant peak bandwidth`
            // each stand in for N member series), build a Set of every
            // dataset index in that group so all members read as
            // "highlighted" together. For an ungrouped entry the set
            // is just the one dataset.
            const hoveredGroup = c.data.datasets[hovered]?._groupLegend;
            const highlighted = new Set<number>();
            if (hoveredGroup) {
              c.data.datasets.forEach((ds: any, i: number) => {
                if (ds._groupLegend === hoveredGroup) highlighted.add(i);
              });
            } else {
              highlighted.add(hovered);
            }
            c.data.datasets.forEach((ds: any, i: number) => {
              if (ds._origBorderWidth == null) ds._origBorderWidth = ds.borderWidth ?? 2;
              if (ds._origBorderColor == null) ds._origBorderColor = ds.borderColor;
              if (highlighted.has(i)) {
                // Strongest: the hovered line (or its group).
                ds.borderWidth = (ds._origBorderWidth ?? 2) + 2;
                ds.borderColor = ds._origBorderColor;
              } else if (sameMetric.has(i)) {
                // Medium: same metric on another session — keep full
                // colour, a touch bolder than baseline so the eye pairs
                // it with the hovered line (#579).
                ds.borderWidth = (ds._origBorderWidth ?? 2) + 1;
                ds.borderColor = ds._origBorderColor;
              } else {
                ds.borderWidth = Math.max(1, (ds._origBorderWidth ?? 2) - 1);
                // Dim by appending an alpha suffix to a hex colour
                // (`#rrggbb` → `#rrggbb33`). Non-hex values fall through
                // unchanged — Chart accepts both formats.
                const oc = ds._origBorderColor;
                ds.borderColor =
                  typeof oc === 'string' && oc.startsWith('#') && oc.length === 7 ? oc + '33' : oc;
              }
            });
            try { c.update('none'); } catch { /* ignore */ }
            // Cursor cue so the user knows the label is interactive.
            if (c.canvas) c.canvas.style.cursor = 'pointer';
          },
          onLeave(_evt: any, _item: any, leg: any) {
            const c = leg?.chart;
            if (!c) return;
            c.data.datasets.forEach((ds: any) => {
              if (ds._origBorderWidth != null) ds.borderWidth = ds._origBorderWidth;
              if (ds._origBorderColor != null) ds.borderColor = ds._origBorderColor;
            });
            try { c.update('none'); } catch { /* ignore */ }
            if (c.canvas) c.canvas.style.cursor = '';
          },
        },
        tooltip: {
          callbacks: {
            title: (items: any[]) => fmtTickHMSms(items[0]?.parsed?.x ?? 0),
            label: (ctx: any) => {
              const v = ctx.parsed?.y;
              const u = props.unit ? ` ${props.unit}` : '';
              return `${ctx.dataset.label}: ${typeof v === 'number' ? v.toFixed(2) : v}${u}`;
            },
          },
        },
        zoom: {
          pan: {
            enabled: true,
            mode: 'x',
            threshold: 2,
            modifierKey: undefined,
            onPanStart: ({ chart: c }: any) => {
              // Pre-seed coord.viewport with the chart's CURRENT visible
              // Pin the range to the chart's current visible window so
              // effectiveRange stops following live mid-pan. setRange
              // collapses what used to be setViewport + setPaused.
              const sx = c?.scales?.x;
              if (sx && Number.isFinite(sx.min) && Number.isFinite(sx.max)) {
                coord.setRange({ min: sx.min, max: sx.max });
              }
            },
            onPanComplete: ({ chart: c }: any) => {
              const sx = c.scales?.x;
              if (!sx) return;
              applyViewportOrSnapToLive(sx.min, sx.max);
            },
          },
          zoom: {
            // Wheel disabled here — `installLiveWheelAnchor` handles
            // Alt+wheel itself with a unified mouse-anchored math
            // shared across the player state lane and focus bar so
            // direction + speed are identical across all three.
            wheel: { enabled: false },
            drag: {
              enabled: true,
              modifierKey: 'alt',
              borderColor: '#1d4ed8',
              borderWidth: 1,
              backgroundColor: 'rgba(37, 99, 235, 0.12)',
            },
            mode: 'x',
            // Legacy session-shell.js parity: zoom DOES NOT auto-pause.
            // Pan does (in onPanStart / right-drag mousedown above), but
            // Alt+wheel and Alt+drag-zoom let the user tighten the view
            // while the chart keeps tracking the live edge. Locking to
            // a specific historical moment is a deliberate gesture
            // (pan, or the explicit Pause button).
            onZoomComplete: ({ chart: c }: any) => {
              const sx = c.scales?.x;
              if (!sx) return;
              applyViewportOrSnapToLive(sx.min, sx.max);
            },
          },
        },
      },
      scales: {
        x: {
          type: 'linear',
          min: initialViewport.min,
          max: initialViewport.max,
          ticks: {
            // Pick a "nice" tick interval based on the visible span,
            // so we get ~minute marks for the default 10-min window
            // and tighter spacing (10s / 1s) when the user zooms in.
            // Same UX pattern vis-timeline gives the events panel
            // natively. Label format scales: ms when zoomed tight,
            // HH:MM otherwise.
            callback: (v: number) => fmtAxisTick(v, chart),
            stepSize: 60_000, // initial — overridden by afterBuildTicks
            font: { size: 10 },
            maxRotation: 0,
          },
          afterBuildTicks: pickTickStep,
          grid: { display: true, color: 'rgba(148, 163, 184, 0.25)' },
        },
        y: {
          min: props.yMin,
          max: props.yMax,
          ticks: { font: { size: 10 } },
          title: props.unit
            ? { display: true, text: props.unit, font: { size: 10 } }
            : undefined,
          afterFit: pinYWidth,
        },
        ...(usesY2 ? {
          y2: {
            position: 'right' as const,
            min: props.y2Min,
            max: props.y2Max,
            grid: { drawOnChartArea: false },
            ticks: { font: { size: 10 } },
            title: props.y2Title
              ? { display: true, text: props.y2Title, font: { size: 10 } }
              : undefined,
            afterFit: pinY2Width,
          },
        } : {}),
      },
    },
  });

  // Right-click pan: a quirk of chartjs-plugin-zoom is that it only
  // panes from a left-button drag. We swap the underlying pointerdown
  // event so right-drag triggers pan and suppresses contextmenu.
  installRightDragPan();
  installLiveWheelAnchor();
  installContextMenuSuppress();
  installLeftClickLiveToggle();
  installCursorHoverTooltip();
  installMarkerHoverTooltip();
  // If compare overlays were already present at mount, compose + drain
  // them now (the overlays watcher's immediate run may have fired before
  // the chart existed). Issue #579.
  if ((props.overlays ?? []).length) {
    try { rebuildAllDatasets(); } catch { /* ignore */ }
    void drainOverlays();
  }
  return chart;
}

/* Cursor hover tooltip (issue #486).
 *
 * When the selected-event cursor is pinned, hovering the mouse within
 * a few pixels of the vertical line reveals a small tooltip showing
 * the cursor's label (`coord.state.cursorLabel`). Drives three refs
 * the template binds: `cursorTooltipVisible`, `cursorTooltipX/Y`.
 *
 * Hit-test is canvas-pixel-space so the tooltip activates wherever the
 * line is actually drawn, regardless of zoom or pan. 6 px tolerance
 * matches the tap-target rule of thumb on macOS / desktop browsers.
 */
const cursorTooltipVisible = ref(false);
const cursorTooltipX = ref(0);
const cursorTooltipY = ref(0);

/* Per-marker hover tooltip (issue #486).
 *
 * Mousemove handler hit-tests against every marker's chart-pixel
 * position. When the mouse comes within ~6 px of a dot, pop a
 * multi-line tooltip with that marker's pre-built `label` (set by
 * BandwidthChart with event type / filename / mbps / bytes / etc.).
 * Hidden when markers themselves are hidden. */
const markerTooltipVisible = ref(false);
const markerTooltipX = ref(0);
const markerTooltipY = ref(0);
const markerTooltipText = ref('');

function installMarkerHoverTooltip() {
  const c = canvas.value;
  if (!c) return;
  c.addEventListener('mousemove', (e) => {
    if (!props.markersVisible) {
      if (markerTooltipVisible.value) markerTooltipVisible.value = false;
      return;
    }
    const list = props.markers;
    if (!Array.isArray(list) || list.length === 0 || !chart) return;
    const sx = chart.scales?.x;
    const sy = chart.scales?.y;
    const area = chart.chartArea;
    if (!sx || !sy || !area) return;
    const rect = c.getBoundingClientRect();
    const mx = e.clientX - rect.left;
    const my = e.clientY - rect.top;
    if (mx < area.left || mx > area.right || my < area.top || my > area.bottom) {
      if (markerTooltipVisible.value) markerTooltipVisible.value = false;
      return;
    }
    // Find the closest marker within tolerance. Use squared
    // distance to avoid the sqrt in the hot path.
    const tol = 7; // pixels
    let bestDist = tol * tol;
    let bestLabel = '';
    for (const m of list) {
      if (!Number.isFinite(m.x) || !Number.isFinite(m.y)) continue;
      if (m.x < sx.min || m.x > sx.max) continue;
      if (m.y < sy.min || m.y > sy.max) continue;
      const px = sx.getPixelForValue(m.x);
      const py = sy.getPixelForValue(m.y);
      const dx = mx - px;
      const dy = my - py;
      const d2 = dx * dx + dy * dy;
      if (d2 < bestDist) {
        bestDist = d2;
        bestLabel = m.label ?? '';
      }
    }
    if (!bestLabel) {
      if (markerTooltipVisible.value) markerTooltipVisible.value = false;
      return;
    }
    markerTooltipText.value = bestLabel;
    markerTooltipX.value = Math.min(mx + 10, c.clientWidth - 280);
    markerTooltipY.value = Math.max(4, my - 56);
    markerTooltipVisible.value = true;
  });
  c.addEventListener('mouseleave', () => {
    markerTooltipVisible.value = false;
  });
}

function installCursorHoverTooltip() {
  const c = canvas.value;
  if (!c) return;
  c.addEventListener('mousemove', (e) => {
    const ms = coord.state.cursorMs;
    const label = coord.state.cursorLabel;
    if (ms == null || !label || !chart) {
      if (cursorTooltipVisible.value) cursorTooltipVisible.value = false;
      return;
    }
    const sx = chart.scales?.x;
    const area = chart.chartArea;
    if (!sx || !area) return;
    const cursorPx = sx.getPixelForValue(ms);
    if (!Number.isFinite(cursorPx)) return;
    const rect = c.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    if (y < area.top || y > area.bottom) {
      if (cursorTooltipVisible.value) cursorTooltipVisible.value = false;
      return;
    }
    if (Math.abs(x - cursorPx) > 6) {
      if (cursorTooltipVisible.value) cursorTooltipVisible.value = false;
      return;
    }
    // Position: just above and slightly right of the cursor, clamped
    // to the visible area so a cursor near the right edge doesn't
    // push the tooltip off-screen.
    cursorTooltipX.value = Math.min(cursorPx + 6, c.clientWidth - 220);
    cursorTooltipY.value = Math.max(4, y - 28);
    cursorTooltipVisible.value = true;
  });
  c.addEventListener('mouseleave', () => {
    cursorTooltipVisible.value = false;
  });
}

/**
 * Left-click-on-plot-area = toggle live (issue #486).
 *
 * The header comment promised this behavior since day one ("Left click
 * (no drag) = toggle pause") but it was never wired. Adds a click-vs-
 * drag detector: a left mouseup within 3 px and 300 ms of mousedown
 * is a click; anything more is a drag (which is already handled by
 * other paths). Click is constrained to the chart's plot area so
 * legend toggles, axis-label clicks, and title clicks fall through
 * to Chart.js's own handlers without also toggling live state.
 *
 * Alt+click is reserved for the live-edge wheel anchor and isn't
 * treated as a toggle.
 */
function installLeftClickLiveToggle() {
  const c = canvas.value;
  if (!c) return;
  let downAt: { x: number; y: number; t: number } | null = null;
  c.addEventListener('mousedown', (e) => {
    if (e.button !== 0 || e.altKey) return;
    downAt = { x: e.clientX, y: e.clientY, t: performance.now() };
  });
  c.addEventListener('mouseup', (e) => {
    if (e.button !== 0) return;
    const start = downAt;
    downAt = null;
    if (!start) return;
    const dist = Math.hypot(e.clientX - start.x, e.clientY - start.y);
    const dur = performance.now() - start.t;
    if (dist > 3 || dur > 300) return;            // it was a drag
    const rect = c.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    const area = chart?.chartArea;
    if (!area) return;
    if (x < area.left || x > area.right || y < area.top || y > area.bottom) return;
    coord.toggleLive();
  });
  onGlobal(window, 'blur', () => { downAt = null; });
}

function installContextMenuSuppress() {
  const c = canvas.value;
  if (!c) return;
  c.addEventListener('contextmenu', (e) => e.preventDefault());
}

let dragState: { startX: number; startMin: number; startMax: number } | null = null;

function installRightDragPan() {
  const c = canvas.value;
  if (!c) return;
  c.addEventListener('mousedown', (e) => {
    if (e.button !== 2 || !chart) return;
    e.preventDefault();
    const sx = chart.scales?.x;
    if (!sx) return;
    dragState = { startX: e.clientX, startMin: sx.min, startMax: sx.max };
    // Pin from the start so effectiveRange stops sliding while the
    // user is dragging (setRange handles both range + paused mirror).
    coord.setRange({ min: sx.min, max: sx.max });
  });
  onGlobal(window, 'mousemove', (e: MouseEvent) => {
    if (!dragState || !chart) return;
    const area = chart.chartArea;
    if (!area) return;
    const widthPx = Math.max(1, area.right - area.left);
    const span = dragState.startMax - dragState.startMin;
    const dx = e.clientX - dragState.startX;
    const dv = (dx / widthPx) * span;
    coord.setRange({ min: dragState.startMin - dv, max: dragState.startMax - dv });
  });
  onGlobal(window, 'mouseup', (e: MouseEvent) => {
    if (e.button === 2) dragState = null;
  });
  onGlobal(window, 'blur', () => { dragState = null; });
}

/**
 * Live-edge wheel zoom anchor — port of legacy session-shell.js
 * `installLiveWheelAnchor`. In LIVE mode (not paused), Alt+wheel
 * zooms anchored at the right edge (`lastSampleMs`) regardless of
 * mouse position so the chart keeps tracking "now". In PAUSED mode
 * we let the chartjs-plugin-zoom handle the wheel normally
 * (mouse-anchored zoom into the frozen history).
 *
 * Capture-phase listener so we run before the plugin's own wheel
 * handler. preventDefault + stopPropagation when we handle it so the
 * plugin doesn't double-zoom.
 */
function installLiveWheelAnchor() {
  const c = canvas.value;
  if (!c) return;
  c.addEventListener(
    'wheel',
    (e: WheelEvent) => {
      // Horizontal pan: trackpad two-finger swipe (deltaX dominant) OR
      // Shift+wheel (the mouse way to scroll horizontally). Shift+wheel
      // reports its magnitude on deltaX in some browsers and deltaY in
      // others, so take whichever axis is larger. No Alt required; plain
      // vertical scroll still falls through to page scroll. See gh#461.
      const horizontalPan = !e.altKey && (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY));
      if (horizontalPan) {
        e.preventDefault();
        e.stopPropagation();
        const chartArea = chart?.chartArea;
        if (!chartArea || chartArea.right <= chartArea.left) return;
        const widthPx = chartArea.right - chartArea.left;
        const current = coord.effectiveRange.value;
        const span = current.max - current.min;
        const delta = Math.abs(e.deltaX) >= Math.abs(e.deltaY) ? e.deltaX : e.deltaY;
        const dms = (delta / widthPx) * span;
        coord.setRange({ min: current.min + dms, max: current.max + dms });
        return;
      }
      if (!e.altKey) return;
      e.preventDefault();
      e.stopPropagation();
      const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
      const MIN_SPAN_MS = 1_000;
      const MAX_SPAN_MS = 24 * 3600 * 1000;
      const vp = coord.state.range;

      // LIVE — left-edge-only via liveSpan.
      if (vp == null) {
        const currentSpan = coord.state.liveSpan;
        const nextSpan = Math.max(MIN_SPAN_MS, Math.min(DEFAULT_FOCUS_MS, currentSpan * factor));
        coord.setLiveSpan(nextSpan);
        return;
      }

      // OFF-LIVE — mouse-anchored zoom on the chartArea. Pin to
      // viewport, snap back to live if the new right edge reaches
      // the live sample.
      const currentSpan = vp.max - vp.min;
      const nextSpan = Math.max(MIN_SPAN_MS, Math.min(MAX_SPAN_MS, currentSpan * factor));
      const chartArea = chart?.chartArea;
      let frac = 0.5;
      if (chartArea && chartArea.right > chartArea.left) {
        const rect = c.getBoundingClientRect();
        const xInArea = e.clientX - rect.left - chartArea.left;
        const widthPx = chartArea.right - chartArea.left;
        frac = Math.max(0, Math.min(1, xInArea / widthPx));
      }
      const anchorTime = vp.min + frac * currentSpan;
      const newStart = anchorTime - frac * nextSpan;
      const newEnd = newStart + nextSpan;
      applyViewportOrSnapToLive(newStart, newEnd);
    },
    { capture: true, passive: false },
  );
}

/** Binary-insert a {x,y} point into `data` so the array stays sorted
 *  ascending by x. Mirrors legacy session-shell.js `insertByTsAsc` —
 *  needed because samples can arrive out of order in two scenarios:
 *
 *    1. v3 session-viewer's bulk replay fires N async watcher
 *       callbacks that resolve concurrently — without sorting the
 *       line zigzags ("scribble") even though the source data is in
 *       order
 *    2. Live mode with SSE jitter where a delayed metrics frame
 *       arrives after a fresher one
 *
 *  Duplicate x values overwrite — last write wins for that timestamp.
 *  O(log n) lookup + O(n) shift; chart pushSample is dominated by
 *  Chart.js update() cost anyway. */
function insertByX(
  data: { x: number; y: number | null }[],
  point: { x: number; y: number | null },
) {
  if (!data.length || point.x >= data[data.length - 1].x) {
    if (data.length && data[data.length - 1].x === point.x) {
      data[data.length - 1] = point;
    } else {
      data.push(point);
    }
    return;
  }
  let lo = 0; let hi = data.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (data[mid].x < point.x) lo = mid + 1; else hi = mid;
  }
  if (lo < data.length && data[lo].x === point.x) {
    data[lo] = point;
  } else {
    data.splice(lo, 0, point);
  }
}


function pushSample(p: PlayerRecord, x: number) {
  if (!chart || !chart.data?.datasets) return;
  coord.noteSample(x);
  let mutated = false;
  for (let i = 0; i < props.series.length; i++) {
    const s = props.series[i];
    let y: number | null | undefined;
    try { y = s.accessor(p); } catch { y = null; }
    if (y == null || !Number.isFinite(y)) continue;
    const data = dataset[i];
    if (!data) continue;
    insertByX(data, { x, y: Number(y) });
    mutated = true;
  }
  // Bound per-series history (issue #582). The chart used to keep every
  // point for the life of the tab ("retention is the cache's job"), but
  // the cache evicts and the chart did not — so the dataset arrays grew
  // unbounded, ballooning the renderer to multi-GB and pegging CPU as
  // each redraw re-rasterized hundreds of thousands of points. Cap each
  // series to MAX_POINTS_PER_SERIES and drop the oldest (front of the
  // ascending-by-x array). The cap is a memory bound, independent of the
  // visible window, so it does NOT reintroduce the zoom-in trimming bug
  // that the old `x - windowMs*2` trim had (that one shrank with zoom).
  if (mutated) {
    for (const d of dataset) {
      if (d.length > MAX_POINTS_PER_SERIES) {
        d.splice(0, d.length - MAX_POINTS_PER_SERIES);
      }
    }
    // Live-follow: advance the viewport ONLY for a genuine live-tail
    // sample (one at/after the running max). `coord.noteSample(x)` above
    // already raised `lastSampleMs` to max(prev, x), so a new tail row
    // has `x === lastSampleMs` and an older backfill row has
    // `x < lastSampleMs`. Guarding on that stops the recent-first
    // backfill (older, off-screen rows) from dragging the window
    // backward — the fix for both the live-edge blank AND the
    // brush-crawl-across-the-whole-backfill regressions (#590).
    if (coord.state.range === null && x >= coord.state.lastSampleMs) {
      applyViewport({ min: x - DEFAULT_FOCUS_MS, max: x });
    }
    safeChartUpdate();
  }
}

// Wrap all watcher bodies in try/catch — a single throw inside a Vue
// watcher logs a noisy "Unhandled error in watcher callback" warning
// and can leave Chart.js in a half-applied state. The chart will
// catch up on the next reactive tick anyway, so swallowing transient
// failures is safe.
//
// Updates are coalesced to a max of one per second via setTimeout.
// Two reasons setTimeout beats requestAnimationFrame here:
//   1. requestAnimationFrame is throttled / paused when the tab is
//      backgrounded — during a bulk archive replay the rAF callback
//      can hold off indefinitely, and the queued chart.update never
//      fires, so the canvas stays blank
//   2. Even at 60 Hz, the session-viewer bulk replay (17k+ snapshots
//      pushed through one watcher each) needs only "the chart catches
//      up periodically so the operator sees progress"; 60× per second
//      is wasted work, 1× per second is plenty
// Live (testing-session) mode also benefits — at 1 Hz the cadence
// matches the server's metrics emit rate, so no work is wasted re-
// rendering a chart that hasn't changed.
/** Adaptive update throttle. Chart.js's render cost grows roughly
 *  linearly with `totalPoints × series`, so a fixed 1 s cadence
 *  becomes the dominant cost once the chart has thousands of points
 *  (a 2 h archive replay can sit at ~30 k points across all series).
 *  Tier the throttle to keep redraws bounded while live mode stays
 *  responsive at the metrics emit cadence:
 *
 *     <   500 pts → 1 s   (live testing-session, fresh)
 *     <  5000 pts → 2 s   (live after ~30 minutes)
 *     <  20000 pts → 5 s  (archive replay or very long live)
 *     >= 20000 pts → 10 s (very long archive)
 *
 *  Picked from observed paint times on test-dev: a 30k-point chart
 *  takes ~80 ms per update — at 1 Hz that's 8 % CPU just for one
 *  chart, with 4 charts on the page that's already 32 %. 10 s drops
 *  that to ~3 % even for the worst case. */
let pendingUpdateTimer: number | null = null;
let lastUpdateAt = 0;
function pickThrottleMs(): number {
  const n = (chart?.data?.datasets ?? []).reduce(
    (acc: number, d: any) => acc + (Array.isArray(d?.data) ? d.data.length : 0), 0);
  if (n >= 20_000) return 10_000;
  if (n >= 5_000) return 5_000;
  if (n >= 500) return 2_000;
  return 1_000;
}
function safeChartUpdate() {
  if (!chart) return;
  if (pendingUpdateTimer != null) return;
  const now = Date.now();
  const throttleMs = pickThrottleMs();
  const dueAt = lastUpdateAt + throttleMs;
  const delay = Math.max(0, dueAt - now);
  pendingUpdateTimer = window.setTimeout(() => {
    pendingUpdateTimer = null;
    lastUpdateAt = Date.now();
    if (!chart) return;
    try { chart.update('none'); } catch (err) { console.warn('chart update skipped:', err); }
  }, delay);
}

/* ─── Samples-stream consumer ──────────────────────────────────────
 *
 * Single feed: drain new CH rows from the time-series cache on every
 * version bump. `lastIngestedMs` is the watermark — only NEW rows
 * (ts > watermark) get pushed. Backfill burst lands in one drain
 * pass; live tail is one extra row per flush.
 *
 * Pause buffer: while paused, queued rows hold in `pendingLive` so
 * the chart's trim cutoff (`pushSample` removes points older than
 * `x - windowMs * 2`) can't advance and silently drop pre-pause
 * archive history off the left edge.
 */
const pendingLive: { p: PlayerRecord; x: number }[] = [];
let drainToken = 0;
let backfillToken = 0;

/**
 * Background fill of the off-screen history that sits OLDER than the
 * live window, walked newest→oldest so panning back populates the rows
 * nearest the window first. These all have `x < lastSampleMs`, so
 * pushSample's guard leaves the viewport parked at the live edge — no
 * crawl, no blank. Snapshot is bounded to MAX_POINTS_PER_SERIES rows so
 * we don't burn the main thread inserting points the per-series cap
 * would immediately trim off the front anyway.
 */
async function backfillOlder(myToken: number, ceilMs: number) {
  if (!chart) return;
  const older = props.eventsStream.inRange(0, ceilMs - 1);
  if (!older.length) return;
  const from = Math.max(0, older.length - MAX_POINTS_PER_SERIES);
  const CHUNK = 500;
  for (let end = older.length; end > from; end -= CHUNK) {
    if (myToken !== backfillToken) return;
    const start = Math.max(from, end - CHUNK);
    for (let i = end - 1; i >= start; i--) {
      const row = older[i];
      const x = tsOfRow(row);
      if (!Number.isFinite(x)) continue;
      pushSample(chRowToPlayerRecord(row), x);
    }
    await new Promise<void>((r) => setTimeout(r, 0));
  }
}

async function drainNewRows() {
  if (!chart) {
    try { await ensure(); }
    catch (err) { console.warn('chart ensure failed:', err); return; }
  }
  if (!chart) return;

  // INITIAL live-mode backfill — fill the visible window from the newest
  // rows FIRST (synchronously, so it lands in one repaint with the
  // viewport already at the live edge), then backfill the older
  // off-screen rows behind it. Draining strictly oldest→newest instead
  // (the generic path below) either left the live edge blank until the
  // drain reached it, or crawled the brush across the whole backfill —
  // both #590 regressions. Pinned/archive mode (range !== null) keeps
  // the generic full-window fill, which never moves the viewport.
  if (lastIngestedMs === -Infinity && coord.state.range === null) {
    const all = props.eventsStream.inRange(0, Number.MAX_SAFE_INTEGER);
    if (!all.length) return;
    const cacheMax = tsOfRow(all[all.length - 1]);
    if (Number.isFinite(cacheMax)) {
      const span = coord.state.liveSpan || DEFAULT_FOCUS_MS;
      const liveStart = cacheMax - span;
      // Find the contiguous recent tail (ascending-by-x array).
      let firstRecent = all.length;
      for (let i = all.length - 1; i >= 0; i--) {
        const x = tsOfRow(all[i]);
        if (Number.isFinite(x) && x >= liveStart) firstRecent = i; else break;
      }
      for (let i = firstRecent; i < all.length; i++) {
        const row = all[i];
        const x = tsOfRow(row);
        if (!Number.isFinite(x)) continue;
        pushSample(chRowToPlayerRecord(row), x);
      }
      lastIngestedMs = cacheMax;
      void backfillOlder(++backfillToken, liveStart);
      return;
    }
  }

  const raw = props.eventsStream.inRange(
    lastIngestedMs === -Infinity ? 0 : lastIngestedMs + 1,
    Number.MAX_SAFE_INTEGER,
  );
  if (!raw.length) return;
  const myToken = ++drainToken;
  // Chunked iteration with main-thread yields keeps brush + scroll
  // responsive when the initial backfill hits (5–10 k rows).
  const CHUNK = 500;
  let highWater = lastIngestedMs;
  for (let start = 0; start < raw.length; start += CHUNK) {
    if (myToken !== drainToken) return;
    const end = Math.min(start + CHUNK, raw.length);
    for (let i = start; i < end; i++) {
      const row = raw[i];
      const x = tsOfRow(row);
      if (!Number.isFinite(x)) continue;
      if (x <= lastIngestedMs) continue; // belt-and-suspenders
      const p = chRowToPlayerRecord(row);
      // Pause buffer is only for samples arriving PAST the pinned
      // window (the live-mode case: user pinned to inspect history,
      // we don't want fresh live samples expanding the view). In
      // archive replay (URL-driven start_time/end_time), the brush
      // is pinned from before the first backfill sample lands, and
      // every sample falls inside the range — those go straight to
      // the chart so the chart actually shows the play.
      const range = coord.state.range;
      if (range !== null && x > range.max) {
        pendingLive.push({ p, x });
      } else {
        pushSample(p, x);
      }
      if (x > highWater) highWater = x;
    }
    // Advance the watermark PER CHUNK so a mid-drain interrupt (a 1 Hz
    // cache flush bumps drainToken and aborts this loop) doesn't restart
    // from the beginning. That restart-from-scratch was re-processing the
    // whole backfill on every flush — ~12 s to catch up on a long
    // session, during which the live edge stayed blank. Per-chunk
    // progress makes the drain converge in a couple seconds.
    lastIngestedMs = highWater;
    if (end < raw.length) {
      await new Promise<void>((r) => setTimeout(r, 0));
    }
  }
}

watch(
  () => props.eventsStream.version.value,
  () => { void drainNewRows(); },
  { immediate: true },
);

/* ─── Compare-overlay drain (issue #579) ───────────────────────────
 *
 * Each grouped sibling has its OWN events stream + tagged series. Drain
 * them independently of the primary path: per overlay we keep an ingest
 * watermark and append only NEW rows. Missing accessor values become an
 * explicit `{x, y:null}` point so the `spanGaps:false` overlay datasets
 * render a gap (not an interpolated bridge) where a sibling lacks a wire
 * metric. Overlays never drive the viewport — the active session owns
 * the brush; siblings just paint onto the shared x/y scales.
 */
function drainOverlays() {
  if (!chart) return;
  const sources = props.overlays ?? [];
  let mutated = false;
  for (const src of sources) {
    const rt = overlayRuntime.get(src.key);
    if (!rt) continue;
    // A sibling re-subscribe (play rotation / refetch-on-pan) bumps its
    // stream epoch — wipe and re-drain from scratch so we don't splice a
    // new window's rows onto a stale watermark.
    const ep = src.eventsStream.epoch.value;
    if (ep !== rt.epoch) {
      rt.epoch = ep;
      rt.watermark = -Infinity;
      for (const arr of rt.datasets) arr.length = 0;
    }
    const fromMs = rt.watermark === -Infinity ? 0 : rt.watermark + 1;
    const rows = src.eventsStream.inRange(fromMs, Number.MAX_SAFE_INTEGER);
    if (!rows.length) continue;
    let hw = rt.watermark;
    for (const row of rows) {
      const x = tsOfRow(row);
      if (!Number.isFinite(x)) continue;
      if (rt.watermark !== -Infinity && x <= rt.watermark) continue;
      const p = chRowToPlayerRecord(row);
      for (let i = 0; i < src.series.length; i++) {
        const arr = rt.datasets[i];
        if (!arr) continue;
        let y: number | null | undefined;
        try { y = src.series[i].accessor(p); } catch { y = null; }
        const yVal = (y == null || !Number.isFinite(y)) ? null : Number(y);
        // Don't seed a dataset with LEADING nulls. A sibling on a device that
        // never provides this field (e.g. Android/ExoPlayer has no per-segment
        // AVMetrics throughput) must contribute an EMPTY dataset — an all-null
        // one desyncs Chart.js's point-element cache and crashes the
        // nearest-mode hover (`reading 'skip'` on an undefined element). Once
        // the series has its first real value, nulls ARE pushed so spanGaps:false
        // still renders gaps for an intermittently-missing metric. This matches
        // the primary path, which simply skips null samples (pushSample).
        if (yVal === null && arr.length === 0) continue;
        insertByX(arr, { x, y: yVal });
      }
      if (x > hw) hw = x;
      mutated = true;
    }
    rt.watermark = hw;
    // Same hard per-series memory bound the primary path uses (#582).
    for (const arr of rt.datasets) {
      if (arr.length > MAX_POINTS_PER_SERIES) arr.splice(0, arr.length - MAX_POINTS_PER_SERIES);
    }
  }
  if (mutated) safeChartUpdate();
}

// Structure changes (compare toggled, a sibling joined/left, or its
// series set changed) → recompose datasets, then drain. Reading
// props.overlays in the getter tracks the prop so a new membership array
// re-fires this. immediate so an at-mount overlay set composes once the
// chart exists.
watch(
  () => (props.overlays ?? [])
    .map((o) => o.key + ':' + o.series.map((s) => s.label).join(',')).join('|'),
  () => {
    try { rebuildAllDatasets(); } catch (err) { console.warn('overlay rebuild skipped:', err); }
    void drainOverlays();
  },
  { immediate: true },
);

// Data changes — any sibling stream version/epoch bump drains new rows.
// Reading each stream's version/epoch inside the getter establishes the
// reactive deps; reading props.overlays re-tracks them on a membership
// swap. Kept separate from the structure watcher so a per-second tick
// doesn't recompose the dataset list, only appends points.
watch(
  () => {
    const ovs = props.overlays ?? [];
    let v = 0;
    for (const o of ovs) v += o.eventsStream.version.value + o.eventsStream.epoch.value;
    return ovs.length + ':' + v;
  },
  () => { void drainOverlays(); },
);

// Resume drain — flush any samples that arrived while pinned in
// chronological order, so the chart catches up to the live edge in
// one pass. insertByX dedupes by x so re-runs stay safe.
watch(
  () => coord.state.range,
  (range) => {
    if (range !== null || !pendingLive.length) return;
    const drained = pendingLive.splice(0, pendingLive.length);
    for (const { p, x } of drained) pushSample(p, x);
  },
);

// Player swap — reset all per-player state so a picker swap clears
// the old chart instead of accumulating both.
watch(
  () => props.playerId,
  () => {
    pendingLive.length = 0;
    lastIngestedMs = -Infinity;
    ++backfillToken; // abort any in-flight backfill for the old player
    for (const arr of dataset) arr.length = 0;
    // Drop compare-overlay state too (#579) so a picker swap doesn't
    // leave a previous group's sibling lines on the new session.
    overlayRuntime.clear();
    safeChartUpdate();
  },
);

// Cache reset (#587) — the events stream re-subscribed to a different
// window (refetch-on-pan, or returning to live). Our forward-only
// watermark would miss the freshly-loaded window (it may be OLDER than
// what we last drained), so reset and re-drain from scratch.
watch(
  () => props.eventsStream.epoch.value,
  () => {
    pendingLive.length = 0;
    lastIngestedMs = -Infinity;
    ++backfillToken; // abort any in-flight backfill from the prior window
    for (const arr of dataset) arr.length = 0;
    void drainNewRows();
  },
);

// React to the coordinated visible range. `effectiveRange` collapses
// the old (version, paused, lastSampleMs) tuple — it changes whenever
// the range pin shifts (chart pan, brush drag, Live toggle) AND when
// lastSampleMs advances (only while in live-tracking mode; when pinned
// the new sample doesn't move the effective range, so the chart
// correctly stays parked).
watch(
  () => coord.effectiveRange.value,
  () => {
    if (!chart) return;
    try {
      applyViewport(coord.effectiveRange.value);
    } catch (err) {
      console.warn('chart viewport apply skipped:', err);
    }
    // Interactive viewport changes (brush drag, pan, zoom — i.e. a
    // pinned range) render directly so they feel as smooth as the
    // vis-timeline events panel. But in LIVE mode (range === null) this
    // watcher fires on EVERY sample, because effectiveRange tracks
    // lastSampleMs — and a direct chart.update() per sample across N
    // charts is the dominant CPU sink on a long-lived tab (#582). For
    // the live-edge case, route through the adaptive throttle instead;
    // the right edge still advances at the metrics emit cadence.
    if (coord.state.range === null) {
      safeChartUpdate();
    } else {
      try { chart.update('none'); } catch (err) { console.warn('chart pan render skipped:', err); }
    }
  },
);

// When markers change, repaint so the dots track the latest stream
// state. Cheap: chart.update('none') skips animations.
watch(
  () => props.markers,
  () => { try { chart?.update('none'); } catch { /* ignore */ } },
  { deep: false },
);

// When yMax changes from outside (Y-axis selector), reapply.
watch(
  () => props.yMax,
  (v) => {
    try { applyYMax(v); } catch (err) { console.warn('chart yMax apply skipped:', err); }
    safeChartUpdate();
  },
);

// When the series list changes (e.g. manifest variants populate after
// the chart is up), reconcile the chart's datasets and replay the
// cached events stream so new series fill in with whatever history
// the rest of the chart has already absorbed. Issue #486.
watch(
  () => props.series.map((s) => s.label).join('|'),
  () => {
    try {
      syncDatasetsFromSeriesProp();
    } catch (err) {
      console.warn('series sync skipped:', err);
    }
    // Replay history into the (possibly new) datasets. Reset the
    // watermark so pushSample picks every cached row up again — it
    // dedupes by exact x value in insertByX so the existing series
    // don't accumulate duplicates.
    try {
      lastIngestedMs = -Infinity;
      drainNewRows();
    } catch { /* ignore */ }
  },
);


// Per-chart-instance expand toggle. The legacy let the operator double
// a single chart's vertical resolution without disturbing the others,
// so they could eyeball a specific signal closely. Local ref, not
// shared via `coord` — each of bandwidth / RTT / buffer / FPS toggles
// independently. Persisted to localStorage under a per-title key so
// the operator's chosen tall layout survives reloads.
const EXPAND_STORAGE_PREFIX = 'dashboard_v3_chart_expand_';
function expandStorageKey() {
  return EXPAND_STORAGE_PREFIX + String(props.title || 'chart').toLowerCase().replace(/\s+/g, '-');
}
function readExpandStored(): boolean {
  try { return localStorage.getItem(expandStorageKey()) === 'true'; } catch { return false; }
}
function writeExpandStored(v: boolean) {
  try { localStorage.setItem(expandStorageKey(), v ? 'true' : 'false'); } catch { /* ignore */ }
}
const expandedLocal = ref<boolean>(readExpandStored());
const expandedClass = computed(() => (expandedLocal.value ? 'expanded' : ''));
function toggleExpand() {
  expandedLocal.value = !expandedLocal.value;
  writeExpandStored(expandedLocal.value);
  // Chart.js with maintainAspectRatio:false honours the new container
  // height via its ResizeObserver. The 150ms CSS transition can race
  // the observer's settled-callback though, so call resize() after the
  // transition completes to make sure the plot fills the new box.
  setTimeout(() => { try { chart?.resize(); } catch { /* ignore */ } }, 200);
}
// Live toggle is "checked" when we're currently following live —
// i.e. no sticky viewport. Reactive across all charts because every
// MetricsLineChart instance reads from the shared `coord.state`.
const liveChecked = computed(() => coord.state.range === null);

/** Click handler — always togglePause. Both directions preserve the
 *  current `liveSpanMs` so going PINNED → LIVE doesn't reset the zoom
 *  span the user dialed in. (The earlier asymmetric "go to default
 *  span on the way back" was rejected — kept ripping the focus bar
 *  away from the zoom the user picked.) */
function onLiveToggleClick() {
  coord.toggleLive();
}

onBeforeUnmount(() => {
  // Remove window-level listeners so the destroyed chart's closures can
  // be GC'd (issue #582 — otherwise switching sessions leaks charts).
  for (const fn of teardownFns) { try { fn(); } catch { /* ignore */ } }
  teardownFns.length = 0;
  if (pendingUpdateTimer != null) { clearTimeout(pendingUpdateTimer); pendingUpdateTimer = null; }
  try { chart?.destroy(); } catch { /* ignore */ }
  chart = null;
});
</script>

<template>
  <div class="metrics-chart" ref="wrap">
    <div class="bar">
      <div class="title">{{ title }}</div>
      <div class="actions">
        <button
          type="button"
          class="btn btn-expand"
          :class="{ active: expandedLocal }"
          @click="toggleExpand"
          :title="expandedLocal ? 'Restore default chart height' : 'Double this chart\'s height for a closer look'"
        >
          <span class="chart-expand-icon">⤢</span>
          {{ expandedLocal ? 'Collapse' : 'Expand' }}
        </button>
        <button
          type="button"
          class="btn live-toggle"
          :class="{ checked: liveChecked }"
          @click="onLiveToggleClick"
          :title="liveChecked ? 'Pause at current live edge' : 'Resume following live (drops zoom and pan)'"
        >
          {{ liveChecked ? '●' : '○' }} Live
        </button>
        <span class="hint" title="Alt/Option + scroll or drag = zoom; Shift + scroll (or two-finger horizontal) = pan; right-click-drag = pan">
          Alt/⌥+scroll/drag zoom · Shift+scroll / right-drag pan
        </span>
      </div>
    </div>
    <div ref="canvasWrap" class="canvas-wrap" :class="expandedClass">
      <canvas ref="canvas" />
      <!-- Cursor hover tooltip (issue #486). Positioned absolutely
           over the canvas; visibility and position driven by the
           installCursorHoverTooltip handler. Empty by default; only
           rendered when the user hovers within a few pixels of the
           pinned cursor line. -->
      <div
        v-if="cursorTooltipVisible"
        class="cursor-tooltip"
        :style="{ left: cursorTooltipX + 'px', top: cursorTooltipY + 'px' }"
      >{{ coord.state.cursorLabel }}</div>
      <!-- Per-marker hover tooltip (issue #486) — appears when the
           mouse is within ~7 px of any overlay marker. Multi-line. -->
      <div
        v-if="markerTooltipVisible"
        class="marker-tooltip"
        :style="{ left: markerTooltipX + 'px', top: markerTooltipY + 'px' }"
      >{{ markerTooltipText }}</div>
    </div>
  </div>
</template>

<style scoped>
.metrics-chart {
  display: grid;
  gap: 6px;
}

.bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.title {
  font-size: 12px;
  font-weight: 600;
  color: #374151;
}
.actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.btn {
  background: #f3f4f6;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  padding: 2px 8px;
  font-size: 11px;
  cursor: pointer;
  color: #374151;
}
.btn:hover { background: #e5e7eb; }
.btn.active {
  background: #e0e7ff;
  border-color: #818cf8;
  color: #312e81;
}
/* Live toggle: filled green when checked (following live), muted/
 * outlined when unchecked (pinned). The hollow vs filled dot in the
 * label reinforces the state at a glance. */
.btn.live-toggle.checked {
  background: #10b981;
  border-color: #059669;
  color: white;
  font-weight: 600;
}
.btn.live-toggle.checked:hover { background: #059669; }
.btn.live-toggle:not(.checked) {
  background: #f3f4f6;
  border-color: #d1d5db;
  color: #6b7280;
}
.btn.live-toggle:not(.checked):hover { background: #e5e7eb; color: #374151; }

.hint {
  font-size: 10px;
  color: #9ca3af;
}

.canvas-wrap {
  position: relative;
  height: 200px;
  width: 100%;
  transition: height 0.15s ease;
}
.canvas-wrap.expanded {
  height: 540px;
}

/* Cursor hover tooltip (issue #486). Position is set via inline
 * style — only the appearance lives here. Above the canvas, below
 * any modal so it's never lost behind a Chart.js native tooltip. */
.cursor-tooltip {
  position: absolute;
  z-index: 4;
  background: #1e3a8a;
  color: #fff;
  font-size: 11px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  padding: 4px 8px;
  border-radius: 4px;
  pointer-events: none;
  white-space: nowrap;
  box-shadow: 0 2px 6px rgba(0, 0, 0, 0.18);
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* Per-marker hover tooltip (issue #486). Different colour from the
 * cursor tooltip so the two read distinctly when both could plausibly
 * be visible. Pre-line for embedded \n in the marker label. */
.marker-tooltip {
  position: absolute;
  z-index: 4;
  background: rgba(15, 23, 42, 0.94); /* slate-900-ish, slight transparency */
  color: #fff;
  font-size: 11px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  padding: 6px 8px;
  border-radius: 4px;
  pointer-events: none;
  white-space: pre-line;
  box-shadow: 0 2px 6px rgba(0, 0, 0, 0.18);
  max-width: 280px;
  line-height: 1.4;
}

.btn-expand { display: inline-flex; align-items: center; gap: 4px; }
.chart-expand-icon { font-size: 13px; line-height: 1; }
.btn-expand.active {
  background: #2563eb;
  border-color: #1d4ed8;
  color: #ffffff;
}
.btn-expand.active:hover { background: #1d4ed8; }
</style>
