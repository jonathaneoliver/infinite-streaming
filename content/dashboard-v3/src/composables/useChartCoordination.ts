/**
 * useChartCoordination(playerId) — shared per-session chart state.
 *
 * The brush IS the source of truth for what's currently displayed.
 * Charts, the brush rail, the Live toggle's checked state, and the
 * brush-end-row panel projection all derive from a single computed
 * `effectiveRange`. There are no separate "viewport" / "paused" /
 * "userMovedBrush" / "live span" flags scattered across watchers —
 * every state transition goes through `setRange` / `setLiveSpan` /
 * `toggleLive` and propagates via Vue's reactive graph in one
 * direction. No cycles, no defensive `if (a === b) return` guards,
 * no version counter.
 *
 * Backing store: module-level Map keyed by playerId so state
 * survives component remount AND is shared across charts on the
 * same session card. Pass either a plain string (legacy) or a
 * `Ref<string>` (picker-swap-aware).
 */
import { computed, isRef, reactive, ref, type Ref } from 'vue';

export interface ChartViewport {
  /** ms-since-epoch start of the visible range */
  min: number;
  /** ms-since-epoch end of the visible range */
  max: number;
}

/** Default "rolling window" span — what the brush sits at on a
 *  fresh session before any zoom gesture, and the cap for Alt+wheel
 *  zoom-out. Exported so chart components clamping their own
 *  zoom-out paths use the same value. */
export const DEFAULT_FOCUS_MS = 10 * 60 * 1000;

/** Tolerance for "is the right edge at the live sample?" — used by
 *  isAtLiveEdge to decide whether a drag/zoom should snap back to
 *  live-tracking mode. */
const LIVE_EDGE_TOLERANCE_MS = 2_000;

export interface ChartCoordinationState {
  /** UI: whether the events-timeline panel is in expanded-height mode. */
  expanded: boolean;

  /** Latest sample timestamp, maintained by chart ingest paths via
   *  `noteSample`. The right edge of "live tracking" mode. */
  lastSampleMs: number;

  /** Y-axis upper bound for the bandwidth chart (undefined = auto). */
  bandwidthYMax: number | undefined;

  /** Synchronized "selected event" cursor — ms-since-epoch. When
   *  non-null every member chart draws a vertical marker at this x
   *  position so the operator can see exactly where the prev/next
   *  selected event sits across all panels. null = no cursor. */
  cursorMs: number | null;
  /** Operator-facing label for the cursor — set alongside `cursorMs`
   *  by `setCursor`. Surfaces in the hover tooltip on every chart's
   *  vertical marker so the operator never has to guess what the
   *  pinned event was. Issue #486. */
  cursorLabel: string | null;

  /** The single source of truth for "what range is currently
   *  displayed". null → operator is following live (chart x-axis is
   *  `[lastSampleMs - liveSpan, lastSampleMs]`). {min, max} →
   *  operator has pinned to this range. */
  range: ChartViewport | null;

  /** The span the operator wants while range === null. Updated ONLY
   *  by explicit user gestures: chart Alt+wheel at live edge,
   *  brush-rail Alt+wheel at live edge, brush drag-end at live edge.
   *  Never auto-derived from incidental brush motion. Defaults to
   *  DEFAULT_FOCUS_MS (10 min). */
  liveSpan: number;
}

const states = new Map<string, ChartCoordinationState>();

const EXPANDED_STORAGE_KEY = 'dashboard_v3_events_timeline_expanded';
function readExpandedStored(): boolean {
  try { return localStorage.getItem(EXPANDED_STORAGE_KEY) === 'true'; } catch { return false; }
}
function writeExpandedStored(v: boolean) {
  try { localStorage.setItem(EXPANDED_STORAGE_KEY, v ? 'true' : 'false'); } catch { /* ignore */ }
}

function freshState(): ChartCoordinationState {
  return reactive<ChartCoordinationState>({
    expanded: readExpandedStored(),
    lastSampleMs: 0,
    bandwidthYMax: undefined,
    cursorMs: null,
    cursorLabel: null,
    range: null,
    liveSpan: DEFAULT_FOCUS_MS,
  });
}

function ensureState(pid: string): ChartCoordinationState {
  let s = states.get(pid);
  if (!s) {
    s = freshState();
    states.set(pid, s);
  }
  return s;
}

export function useChartCoordination(playerIdInput: string | Ref<string>) {
  // Normalise input — accept a plain string or a reactive ref. When
  // a ref is provided, every getter and mutator threads through
  // playerIdRef.value so a picker-swap propagates without remount.
  const playerIdRef: Ref<string> = isRef(playerIdInput)
    ? playerIdInput
    : ref(playerIdInput);

  function cur(): ChartCoordinationState {
    return ensureState(playerIdRef.value);
  }

  /** The single source of truth for what's currently displayed.
   *  Derives from `state.range` (pinned) or
   *  `[lastSampleMs - liveSpan, lastSampleMs]` (following live).
   *
   *  Every chart x-axis, the brush window, the Live-toggle's checked
   *  state, the network-log brush, the brush-end-row projection
   *  watcher — all bind to this. No further sync needed. */
  const effectiveRange = computed<ChartViewport>(() => {
    const s = cur();
    if (s.range) return s.range;
    const end = s.lastSampleMs || Date.now();
    return { min: end - s.liveSpan, max: end };
  });

  /** Wall-clock anchored tick positions for the visible range.
   *  Returns ms-since-epoch timestamps at a "nice" interval so the
   *  chart shows ~6 vertical gridlines. Both Chart.js
   *  (afterBuildTicks) and vis-timeline (addCustomTime) consume the
   *  same array so gridlines line up across all panels in one
   *  session card. */
  const tickPositions = computed<number[]>(() => {
    const v = effectiveRange.value;
    const span = Math.max(1, v.max - v.min);
    const target = span / 6;
    const NICE_MS = [1_000, 5_000, 10_000, 30_000, 60_000, 5 * 60_000, 10 * 60_000, 30 * 60_000];
    let interval = NICE_MS[NICE_MS.length - 1];
    for (const n of NICE_MS) {
      if (n >= target) { interval = n; break; }
    }
    // Snap each tick to wall-clock boundaries of the chosen interval
    // so e.g. 60-second ticks land on :00, :01:00, :02:00.
    const aligned = Math.floor(v.max / interval) * interval;
    const out: number[] = [];
    for (let t = aligned; t >= v.min; t -= interval) out.push(t);
    return out.reverse();
  });

  /** True when the chart's right edge is within tolerance of the
   *  latest sample — used by chart pan/zoom-end paths to decide
   *  whether to snap back into live-tracking mode. */
  function isAtLiveEdge(rightEdgeMs: number): boolean {
    const s = cur();
    if (!s.lastSampleMs) return false;
    return Math.abs(s.lastSampleMs - rightEdgeMs) <= LIVE_EDGE_TOLERANCE_MS;
  }

  function noteSample(ts: number) {
    const s = cur();
    if (ts > s.lastSampleMs) s.lastSampleMs = ts;
  }

  /** Set the pinned range (or null = resume following live). The
   *  single setter that handles every pan/drag/zoom/Live-toggle
   *  state transition. */
  function setRange(r: ChartViewport | null) {
    cur().range = r;
  }

  /** Set the live-edge span (used when range === null). Only updated
   *  by explicit user gestures. Pass any positive number; <= 0
   *  reverts to DEFAULT_FOCUS_MS. */
  function setLiveSpan(ms: number) {
    cur().liveSpan = ms > 0 ? ms : DEFAULT_FOCUS_MS;
  }

  /** Single Live-toggle handler used by chart toolbars, the lane
   *  toolbar, and the focus-window Live button. Pinned → following =
   *  clear range. Following → pinned = snapshot current
   *  effectiveRange. */
  function toggleLive() {
    const s = cur();
    if (s.range !== null) {
      s.range = null;
      return;
    }
    const snap = effectiveRange.value;
    s.range = { min: snap.min, max: snap.max };
  }

  function toggleExpanded() {
    const s = cur();
    s.expanded = !s.expanded;
    writeExpandedStored(s.expanded);
  }

  function setBandwidthYMax(v: number | undefined) {
    cur().bandwidthYMax = v;
  }

  /** Move the synchronized "selected event" cursor. Pass null to
   *  hide it. */
  function setCursorMs(ms: number | null) {
    cur().cursorMs = ms;
    if (ms == null) cur().cursorLabel = null;
  }
  /** Set position AND label in one call. Use this from callers that
   *  know which event the cursor represents (SessionDisplay's
   *  prev/next navigator); use `setCursorMs` from callers that only
   *  know the timestamp. Issue #486. */
  function setCursor(ms: number | null, label: string | null) {
    // Set the highlighted-event cursor only. Does NOT touch the live/pinned
    // range: user event-navigation pins the window via recenterOnNav()
    // (setRange) — that is what keeps a *user-selected* row on screen (#662).
    // Previously (#663) setCursor itself dropped out of Live whenever ms was
    // set while live; but the navigator AUTO-advances to each new event on a
    // live view, so that fired on every incoming event and made the page
    // unable to stay in Live (testing.html + in-progress session-viewer both
    // snapped to a 10-min window around the oldest event). Auto-advance must
    // not drop live; only explicit navigation (recenterOnNav) pins.
    const s = cur();
    s.cursorMs = ms;
    s.cursorLabel = ms == null ? null : label;
  }

  return {
    // `state` is a getter so each property read flows through cur(),
    // which reads playerIdRef.value. Vue templates / computeds that
    // touch `coord.state.X` therefore depend on playerIdRef and
    // re-evaluate when the active player switches.
    get state(): ChartCoordinationState { return cur(); },
    effectiveRange,
    tickPositions,
    isAtLiveEdge,
    noteSample,
    setRange,
    setLiveSpan,
    toggleLive,
    toggleExpanded,
    setBandwidthYMax,
    setCursorMs,
    setCursor,
  };
}

/** Format a ms-since-epoch timestamp as `HH:MM:SS.mmm` (24h,
 *  zero-padded to 3-digit ms) — used by both Chart.js x-axis labels
 *  and vis-timeline custom-time labels so the gridline labels match
 *  across every chart in a session card. */
export function fmtTickHMSms(ms: number): string {
  const d = new Date(ms);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  const ss = String(d.getSeconds()).padStart(2, '0');
  const mss = String(d.getMilliseconds()).padStart(3, '0');
  return `${hh}:${mm}:${ss}.${mss}`;
}
