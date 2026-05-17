/**
 * useChartCoordination(playerId) — shared per-session chart state so
 * the bandwidth / RTT / buffer / FPS charts in one card behave as one
 * coordinated unit:
 *
 *   - One pause flag: pausing any chart pauses all of them.
 *   - One viewport: zooming/panning any chart syncs the others.
 *   - One window length: changing the rolling window length once
 *     applies everywhere.
 *
 * The state lives in a module-level Map keyed by playerId so it
 * survives component remount (e.g. when a section collapses + reopens)
 * and is shared across charts that mount independently of one another.
 *
 * Matches the legacy session-shell.js semantics: 10-minute default
 * window, viewport in ms relative to the latest sample's wall clock,
 * paused freezes the viewport.
 */
import { computed, reactive } from 'vue';

export interface ChartViewport {
  /** ms-since-epoch start of the visible range */
  min: number;
  /** ms-since-epoch end of the visible range */
  max: number;
}

export interface ChartCoordinationState {
  paused: boolean;
  expanded: boolean;
  /** explicit viewport set by the user (pan, drag-zoom); null = follow live */
  viewport: ChartViewport | null;
  /** running "latest sample timestamp" maintained by member charts */
  lastSampleMs: number;
  /** how many ms wide the rolling window is when live-following */
  windowMs: number;
  /** When set and the chart is live (not paused) AND no sticky
   *  viewport, the visible range becomes
   *  `[lastSampleMs - liveSpanMs, lastSampleMs]`. Lets Alt+wheel zoom
   *  while keeping the chart anchored at the live edge (matches the
   *  legacy `installLiveWheelAnchor` behaviour). null = no zoom, use
   *  full `windowMs`. */
  liveSpanMs: number | null;
  /** monotonic version counter so charts react to viewport changes */
  version: number;
  /** Y-axis upper bound for the bandwidth chart (undefined = auto). */
  bandwidthYMax: number | undefined;
}

const states = new Map<string, ChartCoordinationState>();

export function useChartCoordination(playerId: string) {
  let state = states.get(playerId);
  if (!state) {
    state = reactive<ChartCoordinationState>({
      paused: false,
      expanded: false,
      viewport: null,
      lastSampleMs: 0,
      windowMs: 10 * 60 * 1000,
      liveSpanMs: null,
      version: 0,
      bandwidthYMax: undefined,
    });
    states.set(playerId, state);
  }
  const s = state;

  const effectiveViewport = computed<ChartViewport>(() => {
    if (s.viewport) return s.viewport;
    const end = s.lastSampleMs || Date.now();
    // When live (no sticky viewport) and the user has chosen a tighter
    // zoom span via Alt+wheel, the visible range follows the live edge
    // with that span — same as the legacy live-anchored wheel zoom.
    const span = s.liveSpanMs != null ? s.liveSpanMs : s.windowMs;
    return { min: end - span, max: end };
  });

  /** Wall-clock anchored tick positions for the visible viewport.
   *  Returns an array of ms-since-epoch timestamps, picked at a "nice"
   *  interval (1s / 5s / 10s / 30s / 1m / 5m / 10m / 30m) so the chart
   *  shows ~6 vertical gridlines across the window. Both Chart.js
   *  (afterBuildTicks) and vis-timeline (addCustomTime) consume this
   *  same array so the gridlines line up vertically across the
   *  bandwidth / RTT / buffer / FPS / events-timeline panels in one
   *  session card. */
  const tickPositions = computed<number[]>(() => {
    const v = effectiveViewport.value;
    const span = Math.max(1, v.max - v.min);
    const target = span / 6;
    const NICE_MS = [1_000, 5_000, 10_000, 30_000, 60_000, 5 * 60_000, 10 * 60_000, 30 * 60_000];
    let interval = NICE_MS[NICE_MS.length - 1];
    for (const n of NICE_MS) {
      if (n >= target) { interval = n; break; }
    }
    // Snap each tick to wall-clock boundaries of the chosen interval
    // so e.g. 60-second ticks land on :00, :01:00, :02:00 — matches
    // how the user mentally reads clock time across the gridlines.
    const aligned = Math.floor(v.max / interval) * interval;
    const out: number[] = [];
    for (let t = aligned; t >= v.min; t -= interval) out.push(t);
    return out.reverse();
  });

  function noteSample(ts: number) {
    if (ts > s.lastSampleMs) s.lastSampleMs = ts;
  }

  function setViewport(v: ChartViewport | null) {
    s.viewport = v;
    s.version++;
  }

  /** Snapshot the current live viewport so the chart freezes here.
   *  Without this, even though `paused = true`, the effectiveViewport
   *  keeps sliding with `lastSampleMs` because each new tick advances
   *  the "live window" end. Capturing once at pause-time is what makes
   *  the line / state charts visibly stop scrolling.
   *
   *  Honours `liveSpanMs` if set, so pausing after an Alt+wheel zoom
   *  preserves the zoomed range — the user gets a frozen view of
   *  exactly what they were watching, not a snap-out to the full
   *  window. */
  function snapshotLiveViewport() {
    const end = s.lastSampleMs || Date.now();
    const span = s.liveSpanMs != null ? s.liveSpanMs : s.windowMs;
    s.viewport = { min: end - span, max: end };
  }

  function togglePause() {
    s.paused = !s.paused;
    if (s.paused) {
      snapshotLiveViewport();
    } else {
      // Returning to live drops any sticky user viewport so the next
      // tick's effectiveViewport snaps back to "now" (the live edge).
      s.viewport = null;
    }
    s.version++;
  }

  function setPaused(p: boolean) {
    if (s.paused === p) return;
    s.paused = p;
    if (p) snapshotLiveViewport();
    else s.viewport = null;
    s.version++;
  }

  /** Snap charts back to the full-window extent. Crucially, this does
   *  NOT touch `paused` — legacy session-shell.js parity: Reset Zoom
   *  is a viewport-only operation. If the user is paused and wants to
   *  resume streaming, they click the Pause / Live button (which
   *  reads "Live" while paused).
   *
   *  Live → viewport stays null AND any Alt+wheel live-span gets
   *  cleared so `effectiveViewport` follows the full live window.
   *
   *  Paused → viewport must be re-snapshotted at the current frozen
   *  time to the full window. We can't leave it null because
   *  `effectiveViewport` would otherwise fall back to
   *  `[lastSampleMs - windowMs, lastSampleMs]` which keeps sliding
   *  with each new tick — defeating the pause. */
  function resetZoom() {
    s.liveSpanMs = null;
    if (s.paused) {
      snapshotLiveViewport();
    } else {
      s.viewport = null;
    }
    s.version++;
  }

  /** Set the live-edge zoom span. Bumps the version so charts react.
   *  Pass null to clear (revert to full `windowMs`). Has no effect on
   *  the paused branch — paused mode uses sticky `viewport`. */
  function setLiveSpanMs(ms: number | null) {
    s.liveSpanMs = ms;
    s.version++;
  }

  function toggleExpanded() {
    s.expanded = !s.expanded;
  }

  function setWindowMs(ms: number) {
    s.windowMs = Math.max(5_000, ms);
    s.viewport = null;
    s.version++;
  }

  function setBandwidthYMax(v: number | undefined) {
    s.bandwidthYMax = v;
  }

  return {
    state: s,
    effectiveViewport,
    tickPositions,
    noteSample,
    setViewport,
    togglePause,
    setPaused,
    resetZoom,
    setLiveSpanMs,
    toggleExpanded,
    setWindowMs,
    setBandwidthYMax,
  };
}

/** Format a ms-since-epoch timestamp as `HH:MM:SS.mmm` (24h, zero-padded
 *  to 3-digit ms) — used by both Chart.js x-axis labels and vis-timeline
 *  custom-time labels so the gridline labels match across every chart in
 *  a session card. Exported so the components don't drift from this
 *  format. */
export function fmtTickHMSms(ms: number): string {
  const d = new Date(ms);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  const mss = String(d.getMilliseconds()).padStart(3, '0');
  return `${hh}:${mm}:${ss}.${mss}`;
}
