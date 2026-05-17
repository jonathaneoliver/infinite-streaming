<script setup lang="ts">
/**
 * MetricsLineChart.vue — streaming line chart with the same UX the
 * legacy session-shell.js charts had:
 *
 *   - Toolbar: Reset Zoom · Pause/Live · Expand · zoom hint
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
import { usePlayer } from '@/composables/usePlayer';
import { ensureChartJs } from '@/composables/useChartJs';
import { useChartCoordination, fmtTickHMSms, type ChartViewport } from '@/composables/useChartCoordination';
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
});

const canvas = ref<HTMLCanvasElement | null>(null);
const wrap = ref<HTMLDivElement | null>(null);
const canvasWrap = ref<HTMLDivElement | null>(null);
const { player } = usePlayer(toRef(props, 'playerId'));
const coord = useChartCoordination(props.playerId);

let chart: any = null;
let lastSampleKey: string | null = null;
let dataset: Array<Array<{ x: number; y: number }>> = [];
let clickStartX = 0;
let clickStartY = 0;
let clickDragged = false;

function tsFor(p: PlayerRecord): number {
  if (p.player_metrics?.event_time) {
    const v = Date.parse(p.player_metrics.event_time);
    if (Number.isFinite(v)) return v;
  }
  if (p.last_seen_at) {
    const v = Date.parse(p.last_seen_at);
    if (Number.isFinite(v)) return v;
  }
  return Date.now();
}

function sampleKey(p: PlayerRecord): string {
  return `${p.control_revision}|${p.last_seen_at ?? ''}|${p.player_metrics?.event_time ?? ''}`;
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
  const initialViewport = coord.effectiveViewport.value;
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
        tension: s.stepped ? 0 : 0.25,
        stepped: !!s.stepped,
        pointRadius: 0,
        borderWidth: 2,
        spanGaps: true,
        yAxisID: s.axis === 'y2' ? 'y2' : 'y',
      })),
    },
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
            onPanStart: () => {
              if (!coord.state.paused) coord.setPaused(true);
            },
            onPanComplete: ({ chart: c }: any) => {
              const sx = c.scales?.x;
              if (!sx) return;
              coord.setViewport({ min: sx.min, max: sx.max });
            },
          },
          zoom: {
            wheel: { enabled: true, modifierKey: 'alt' },
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
              coord.setViewport({ min: sx.min, max: sx.max });
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
  installClickPause();
  installLiveWheelAnchor();
  installContextMenuSuppress();
  return chart;
}

function installContextMenuSuppress() {
  const c = canvas.value;
  if (!c) return;
  c.addEventListener('contextmenu', (e) => e.preventDefault());
}

function installClickPause() {
  // Bind on the WRAPPER, not the canvas. The legend area is drawn on
  // the canvas by Chart.js, but the zoom plugin attaches its own
  // listeners directly on the canvas element and can swallow the click
  // before our handler runs. The wrapper sits one level up so the
  // event always bubbles to us.
  const el = canvasWrap.value;
  if (!el) return;

  /** Is `(clientX,clientY)` inside the Chart.js legend hit-box? Chart
   *  v4 exposes `chart.legend.{top,bottom,left,right}` in canvas-local
   *  pixel coordinates. We translate the click into the same space and
   *  test the rectangle. Clicking legend items toggles series
   *  visibility (Chart.js handles that internally); we must NOT also
   *  toggle pause when the user is just hiding a line. */
  function pointInLegend(clientX: number, clientY: number): boolean {
    const c = canvas.value;
    if (!c || !chart?.legend) return false;
    const r = c.getBoundingClientRect();
    // Translate to CSS-pixel canvas coords. Chart.js uses devicePixelRatio
    // internally; legend.{top,…} is reported in CSS pixels matching the
    // bounding rect, no extra scaling needed.
    const x = clientX - r.left;
    const y = clientY - r.top;
    const l = chart.legend;
    return (
      typeof l.top === 'number' &&
      typeof l.bottom === 'number' &&
      typeof l.left === 'number' &&
      typeof l.right === 'number' &&
      x >= l.left && x <= l.right && y >= l.top && y <= l.bottom
    );
  }

  // Same heuristic for the chart title / panel toolbar widgets that
  // sit ABOVE the plot area. Currently we only have the legend below
  // the chart, so just the legend check is needed.

  el.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    clickStartX = e.clientX;
    clickStartY = e.clientY;
    clickDragged = false;
  });
  el.addEventListener('mousemove', (e) => {
    if (e.buttons === 0) return;
    if (Math.abs(e.clientX - clickStartX) > 3 || Math.abs(e.clientY - clickStartY) > 3) {
      clickDragged = true;
    }
  });
  el.addEventListener('click', (e) => {
    if (e.altKey) return;
    if (clickDragged) return;
    // Don't toggle pause when the user is interacting with the Chart.js
    // legend (show/hide a series) or the chart's toolbar/title area.
    if (pointInLegend(e.clientX, e.clientY)) return;
    coord.togglePause();
  });
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
    if (!coord.state.paused) coord.setPaused(true);
  });
  window.addEventListener('mousemove', (e) => {
    if (!dragState || !chart) return;
    const area = chart.chartArea;
    if (!area) return;
    const widthPx = Math.max(1, area.right - area.left);
    const span = dragState.startMax - dragState.startMin;
    const dx = e.clientX - dragState.startX;
    const dv = (dx / widthPx) * span;
    coord.setViewport({ min: dragState.startMin - dv, max: dragState.startMax - dv });
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
      if (!e.altKey) return;
      if (coord.state.paused) return; // plugin handles mouse-anchored zoom
      e.preventDefault();
      e.stopPropagation();
      const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
      const windowMs = coord.state.windowMs;
      const currentSpan =
        coord.state.liveSpanMs != null ? coord.state.liveSpanMs : windowMs;
      const MIN_SPAN_MS = 1_000;
      const nextSpan = Math.max(
        MIN_SPAN_MS,
        Math.min(windowMs, currentSpan * factor),
      );
      // Clearing the override (back to full window) when we'd otherwise
      // pin at windowMs lets future "no override" callers (e.g. reset)
      // observe a clean state.
      coord.setLiveSpanMs(nextSpan >= windowMs ? null : nextSpan);
    },
    { capture: true, passive: false },
  );
}

function pushSample(p: PlayerRecord) {
  if (!chart || !chart.data?.datasets) return;
  const x = tsFor(p);
  coord.noteSample(x);
  const cutoff = x - coord.state.windowMs * 2;
  let mutated = false;
  for (let i = 0; i < props.series.length; i++) {
    const s = props.series[i];
    let y: number | null | undefined;
    try { y = s.accessor(p); } catch { y = null; }
    if (y == null || !Number.isFinite(y)) continue;
    const data = dataset[i];
    if (!data) continue;
    data.push({ x, y: Number(y) });
    while (data.length && data[0].x < cutoff) data.shift();
    mutated = true;
  }
  if (mutated) {
    if (!coord.state.paused && !coord.state.viewport) {
      applyViewport({ min: x - coord.state.windowMs, max: x });
    }
    safeChartUpdate();
  }
}

// Wrap all watcher bodies in try/catch — a single throw inside a Vue
// watcher logs a noisy "Unhandled error in watcher callback" warning
// and can leave Chart.js in a half-applied state. The chart will
// catch up on the next reactive tick anyway, so swallowing transient
// failures is safe.
function safeChartUpdate() {
  if (!chart) return;
  try { chart.update('none'); } catch (err) { console.warn('chart update skipped:', err); }
}

watch(
  () => player.value,
  async (p) => {
    if (!p) return;
    const k = sampleKey(p);
    if (k === lastSampleKey) return;
    lastSampleKey = k;
    try {
      await ensure();
      pushSample(p);
    } catch (err) {
      console.warn('chart sample push failed:', err);
    }
  },
  { immediate: true, deep: false },
);

// React to coordinated state changes (other charts in the same session
// zooming / panning / pausing).
watch(
  () => [coord.state.version, coord.state.paused, coord.state.lastSampleMs] as const,
  () => {
    if (!chart) return;
    try {
      applyViewport(coord.effectiveViewport.value);
    } catch (err) {
      console.warn('chart viewport apply skipped:', err);
    }
    safeChartUpdate();
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
// independently.
const expandedLocal = ref(false);
const expandedClass = computed(() => (expandedLocal.value ? 'expanded' : ''));
function toggleExpand() {
  expandedLocal.value = !expandedLocal.value;
  // Chart.js with maintainAspectRatio:false honours the new container
  // height via its ResizeObserver. The 150ms CSS transition can race
  // the observer's settled-callback though, so call resize() after the
  // transition completes to make sure the plot fills the new box.
  setTimeout(() => { try { chart?.resize(); } catch { /* ignore */ } }, 200);
}
const pauseLabel = computed(() => (coord.state.paused ? '▶ Live' : '⏸ Pause'));
const zoomActive = computed(() => coord.state.viewport !== null);

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
          class="btn"
          :class="{ active: zoomActive }"
          @click="coord.resetZoom()"
          title="Snap back to live edge and clear any zoom"
        >
          Reset Zoom
        </button>
        <button
          type="button"
          class="btn"
          :class="{ live: coord.state.paused }"
          @click="coord.togglePause()"
          title="Freeze the chart at the current view"
        >
          {{ pauseLabel }}
        </button>
        <span class="hint" title="Hold Alt (Option on Mac) while scrolling or dragging to zoom; right-click-drag to pan">
          Alt/⌥+scroll/drag · right-drag pan · click pause
        </span>
      </div>
    </div>
    <div ref="canvasWrap" class="canvas-wrap" :class="expandedClass">
      <canvas ref="canvas" />
      <div v-if="coord.state.paused" class="paused-badge">PAUSED</div>
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
.btn.live {
  background: #10b981;
  border-color: #059669;
  color: white;
}
.btn.live:hover { background: #059669; }

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

.paused-badge {
  position: absolute;
  top: 8px;
  right: 8px;
  background: rgba(31, 41, 55, 0.85);
  color: #fde68a;
  padding: 2px 8px;
  border-radius: 4px;
  font-size: 10px;
  font-weight: 700;
  letter-spacing: 1px;
  pointer-events: none;
}
</style>
