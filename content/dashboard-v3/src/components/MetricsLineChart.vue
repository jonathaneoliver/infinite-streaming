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
import { computed, onBeforeUnmount, ref, toRef, watch, type PropType } from 'vue';
import { ensureChartJs } from '@/composables/useChartJs';
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
});

const canvas = ref<HTMLCanvasElement | null>(null);
const wrap = ref<HTMLDivElement | null>(null);
const canvasWrap = ref<HTMLDivElement | null>(null);
const coord = useChartCoordination(toRef(props, 'playerId'));

let chart: any = null;
let dataset: Array<Array<{ x: number; y: number }>> = [];
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
}

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
      datasets: props.series.map((s, i) => ({
        label: s.label,
        borderColor: s.color,
        backgroundColor: s.color + '22',
        data: dataset[i],
        // Straight line segments between samples — no curve fitting.
        // Stepped series (player state) keep tension 0 too. Smoothing
        // implies measurements that weren't taken; for instrumentation
        // data, straight-line is the truthful representation.
        tension: 0,
        stepped: !!s.stepped,
        pointRadius: 0,
        borderWidth: 2,
        spanGaps: true,
        yAxisID: s.axis === 'y2' ? 'y2' : 'y',
        hidden: !!s.hidden,
      })),
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
          labels: { boxWidth: 10, font: { size: 11 }, padding: 6 },
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
            const hovered = item.datasetIndex;
            c.data.datasets.forEach((ds: any, i: number) => {
              if (ds._origBorderWidth == null) ds._origBorderWidth = ds.borderWidth ?? 2;
              if (ds._origBorderColor == null) ds._origBorderColor = ds.borderColor;
              if (i === hovered) {
                ds.borderWidth = (ds._origBorderWidth ?? 2) + 2;
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
  return chart;
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
  window.addEventListener('mousemove', (e) => {
    if (!dragState || !chart) return;
    const area = chart.chartArea;
    if (!area) return;
    const widthPx = Math.max(1, area.right - area.left);
    const span = dragState.startMax - dragState.startMin;
    const dx = e.clientX - dragState.startX;
    const dv = (dx / widthPx) * span;
    coord.setRange({ min: dragState.startMin - dv, max: dragState.startMax - dv });
  });
  window.addEventListener('mouseup', (e) => {
    if (e.button === 2) dragState = null;
  });
  window.addEventListener('blur', () => { dragState = null; });
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
      // Horizontal scroll (trackpad two-finger swipe left/right or
      // mouse horizontal scroll) → pan the chart by deltaX scaled
      // against the chart's plot-area width. No Alt required; plain
      // vertical scroll still falls through to page scroll. See
      // gh#461.
      if (!e.altKey && Math.abs(e.deltaX) > Math.abs(e.deltaY)) {
        e.preventDefault();
        e.stopPropagation();
        const chartArea = chart?.chartArea;
        if (!chartArea || chartArea.right <= chartArea.left) return;
        const widthPx = chartArea.right - chartArea.left;
        const current = coord.effectiveRange.value;
        const span = current.max - current.min;
        const dms = (e.deltaX / widthPx) * span;
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
function insertByX(data: { x: number; y: number }[], point: { x: number; y: number }) {
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
  // Retention is the time-series cache's job (SOFT_CAP_SAMPLES) —
  // the chart used to trim at `x - windowMs * 2` to bound memory,
  // but `windowMs` is the *visible* window, not retention. When the
  // operator zooms in (focusSpan shrinks → setWindowMs shrinks) the
  // trim was killing older points the PLAYERSTATE lane still had,
  // producing a chart-vs-lane time-axis mismatch. Chart.js with
  // `animation: false` handles tens of thousands of points fine.
  if (mutated) {
    if (coord.state.range === null) {
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

async function drainNewRows() {
  if (!chart) {
    try { await ensure(); }
    catch (err) { console.warn('chart ensure failed:', err); return; }
  }
  if (!chart) return;
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
    if (end < raw.length) {
      await new Promise<void>((r) => setTimeout(r, 0));
    }
  }
  lastIngestedMs = highWater;
}

watch(
  () => props.eventsStream.version.value,
  () => { void drainNewRows(); },
  { immediate: true },
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
    for (const arr of dataset) arr.length = 0;
    safeChartUpdate();
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
    // Viewport / pause changes are axis-only updates — render
    // directly so brush drag feels as smooth as the vis-timeline
    // events panel. safeChartUpdate's adaptive throttle exists for
    // data-arrival churn (pushSample / drainNewRows insert many
    // points per second); applying it here would delay pan response
    // up to several seconds when a dataset has thousands of points.
    try { chart.update('none'); } catch (err) { console.warn('chart pan render skipped:', err); }
  },
);

// When yMax changes from outside (Y-axis selector), reapply.
watch(
  () => props.yMax,
  (v) => {
    try { applyYMax(v); } catch (err) { console.warn('chart yMax apply skipped:', err); }
    safeChartUpdate();
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
        <span class="hint" title="Hold Alt (Option on Mac) while scrolling or dragging to zoom; right-click-drag to pan">
          Alt/⌥+scroll/drag · right-drag pan
        </span>
      </div>
    </div>
    <div ref="canvasWrap" class="canvas-wrap" :class="expandedClass">
      <canvas ref="canvas" />
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

.btn-expand { display: inline-flex; align-items: center; gap: 4px; }
.chart-expand-icon { font-size: 13px; line-height: 1; }
.btn-expand.active {
  background: #2563eb;
  border-color: #1d4ed8;
  color: #ffffff;
}
.btn-expand.active:hover { background: #1d4ed8; }
</style>
