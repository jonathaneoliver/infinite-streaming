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
 * Backing store: module-level Map keyed by playerId so the state
 * survives component remount AND is shared across charts on the same
 * session card.
 *
 * Reactivity model: callers may pass either a plain string (legacy) or
 * a `Ref<string>` (TS12 — picker swap without :key remount). When a
 * ref is supplied, the `state` getter + every computed + every mutator
 * re-read `playerIdRef.value` on access, so switching the active
 * player in Testing.vue's picker propagates to every chart in the
 * same SessionDisplay instance without forcing a remount.
 *
 * Matches the legacy session-shell.js semantics: 10-minute default
 * window, viewport in ms relative to the latest sample's wall clock,
 * paused freezes the viewport.
 */
import { computed, isRef, reactive, ref, type Ref } from 'vue';

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
  /** Synchronized "selected event" cursor — ms-since-epoch.
   *  When non-null every member chart (line charts + events
   *  timeline) draws a vertical marker at this x position so the
   *  operator can see exactly where the prev/next-selected event
   *  sits across all panels. null = no cursor (default). */
  cursorMs: number | null;

  /* ─── Brush-as-source-of-truth refactor (alongside old API) ───────
   *
   * `range` collapses what `viewport` + `paused` used to express:
   *   null         → operator is following live (chart x-axis is
   *                   `[lastSampleMs - liveSpan, lastSampleMs]`)
   *   {min, max}   → operator has pinned to this range (chart x-axis
   *                   is exactly this)
   *
   * `liveSpan` collapses `liveSpanMs ?? windowMs`. Defaults to
   * DEFAULT_FOCUS_MS (10 min) and is updated ONLY by explicit user
   * gestures: chart Alt+wheel at live edge, brush-rail Alt+wheel at
   * live edge, drag-end at live edge. Never auto-derived from
   * incidental brush motion (that was the source of the focus-bar
   * auto-grow regression we fixed by removing the auto-feedback
   * watcher).
   *
   * Once all consumers have migrated, this pair entirely supersedes
   * `viewport`, `paused`, `liveSpanMs`, `windowMs`, and `version`.
   * During the migration the new setters internally also update the
   * old state so legacy reads stay coherent. */
  range: ChartViewport | null;
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

const DEFAULT_FOCUS_MS = 10 * 60 * 1000;
const LIVE_EDGE_TOLERANCE_MS = 2_000;

function freshState(): ChartCoordinationState {
  return reactive<ChartCoordinationState>({
    paused: false,
    expanded: readExpandedStored(),
    viewport: null,
    lastSampleMs: 0,
    windowMs: DEFAULT_FOCUS_MS,
    liveSpanMs: null,
    version: 0,
    bandwidthYMax: undefined,
    cursorMs: null,
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
  // Normalise input — accept both a plain string (legacy callers) and
  // a reactive ref (new picker-swap-aware callers). When a ref is
  // provided, every getter + computed + mutator below threads through
  // `playerIdRef.value` so changes propagate to the consumer's
  // reactive context without a component remount.
  const playerIdRef: Ref<string> = isRef(playerIdInput)
    ? playerIdInput
    : ref(playerIdInput);

  function cur(): ChartCoordinationState {
    return ensureState(playerIdRef.value);
  }

  const effectiveViewport = computed<ChartViewport>(() => {
    const s = cur();
    if (s.viewport) return s.viewport;
    const end = s.lastSampleMs || Date.now();
    // When live (no sticky viewport) and the user has chosen a tighter
    // zoom span via Alt+wheel, the visible range follows the live edge
    // with that span — same as the legacy live-anchored wheel zoom.
    const span = s.liveSpanMs != null ? s.liveSpanMs : s.windowMs;
    return { min: end - span, max: end };
  });

  /** New API: the single source of truth for what's currently displayed.
   *  Derives from `state.range` (when pinned) or `[lastSampleMs - liveSpan,
   *  lastSampleMs]` (when following live). Replaces `effectiveViewport`
   *  once all consumers migrate. */
  const effectiveRange = computed<ChartViewport>(() => {
    const s = cur();
    if (s.range) return s.range;
    const end = s.lastSampleMs || Date.now();
    return { min: end - s.liveSpan, max: end };
  });

  /** True when the chart's right edge is within tolerance of the latest
   *  sample — i.e. the operator's effective view is essentially "live".
   *  Used by chart pan/zoom-end paths to decide whether to snap back
   *  into live-tracking mode. */
  function isAtLiveEdge(rightEdgeMs: number): boolean {
    const s = cur();
    if (!s.lastSampleMs) return false;
    return Math.abs(s.lastSampleMs - rightEdgeMs) <= LIVE_EDGE_TOLERANCE_MS;
  }

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
    const s = cur();
    if (ts > s.lastSampleMs) s.lastSampleMs = ts;
  }

  function setViewport(v: ChartViewport | null) {
    const s = cur();
    const span = v ? Math.round((v.max - v.min) / 1000) : 'null';
    const trace = new Error().stack?.split('\n').slice(2, 5).join(' | ').slice(0, 200) ?? '';
    console.log('[CC] setViewport span=' + span + 's | ' + trace);
    s.viewport = v;
    // Mirror into the new `range` so old + new readers stay coherent
    // during the migration. After Phase E, `viewport` goes away and
    // only `range` is written.
    s.range = v;
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
    const s = cur();
    // If there's already an explicit viewport (set by a brush drag
    // or an Alt+drag-zoom on a chart), DON'T overwrite it. The whole
    // point of pausing is to freeze whatever the operator was looking
    // at; if they had a custom range it IS the visible window. The
    // legacy "snapshot to [lastSample - liveSpanMs, ...]" path runs
    // only when no sticky viewport exists — i.e. we're live-tracking
    // and need to freeze at the live edge.
    if (s.viewport) return;
    const end = s.lastSampleMs || Date.now();
    const span = s.liveSpanMs != null ? s.liveSpanMs : s.windowMs;
    s.viewport = { min: end - span, max: end };
    s.range = s.viewport;
  }

  function togglePause() {
    const s = cur();
    s.paused = !s.paused;
    if (s.paused) {
      snapshotLiveViewport();
    } else {
      // Returning to live drops any sticky user viewport so the next
      // tick's effectiveViewport snaps back to "now" (the live edge).
      s.viewport = null;
      s.range = null;
    }
    s.version++;
  }

  function setPaused(p: boolean) {
    const s = cur();
    if (s.paused === p) return;
    s.paused = p;
    if (p) {
      snapshotLiveViewport();
    } else {
      s.viewport = null;
      s.range = null;
    }
    s.version++;
  }

  /** Set the live-edge zoom span. Bumps the version so charts react.
   *  Pass null to clear (revert to full `windowMs`). Has no effect on
   *  the paused branch — paused mode uses sticky `viewport`. */
  function setLiveSpanMs(ms: number | null) {
    const s = cur();
    s.liveSpanMs = ms;
    // Mirror into the new `liveSpan`. Null clears back to default.
    s.liveSpan = ms != null ? ms : DEFAULT_FOCUS_MS;
    s.version++;
  }

  /* ─── New API: setRange / setLiveSpan / toggleLive ─────────────────
   * These are the brush-as-source-of-truth setters. They mirror into
   * the OLD state too so consumers that haven't migrated yet keep
   * seeing coherent reads. After Phase E the old state goes away. */

  /** Set the pinned range (or null = resume following live). The
   *  single setter that replaces every `setViewport`+`setPaused` pair.
   *  `range === null` means "follow live"; non-null means "pinned". */
  function setRange(r: ChartViewport | null) {
    const s = cur();
    s.range = r;
    // Mirror into the deprecated state during migration.
    s.viewport = r;
    s.paused = r !== null;
    s.version++;
  }

  /** Set the live-edge span (used when range === null). Only updated
   *  by explicit user gestures: chart Alt+wheel at live edge,
   *  brush-rail Alt+wheel at live edge, brush drag-end at live edge.
   *  Defaults to DEFAULT_FOCUS_MS; setting <= 0 reverts to default. */
  function setLiveSpan(ms: number) {
    const s = cur();
    const next = ms > 0 ? ms : DEFAULT_FOCUS_MS;
    s.liveSpan = next;
    // Mirror into the deprecated state. `liveSpanMs == null` was the
    // old "no zoom, use windowMs" signal — by always writing a value
    // we keep the old effectiveViewport math consistent.
    s.liveSpanMs = next;
    s.version++;
  }

  /** Single-handler Live toggle used by chart toolbars, lane toolbar,
   *  and the focus-window Live button. Pinned → following = clear
   *  range. Following → pinned = snapshot current effectiveRange. */
  function toggleLive() {
    const s = cur();
    if (s.range !== null) {
      setRange(null);
      return;
    }
    // Currently following live; pin to current effectiveRange.
    const snapshot = effectiveRange.value;
    setRange({ min: snapshot.min, max: snapshot.max });
  }

  function toggleExpanded() {
    const s = cur();
    s.expanded = !s.expanded;
    writeExpandedStored(s.expanded);
  }

  function setWindowMs(ms: number) {
    const s = cur();
    s.windowMs = Math.max(5_000, ms);
    s.viewport = null;
    s.version++;
  }

  function setBandwidthYMax(v: number | undefined) {
    cur().bandwidthYMax = v;
  }

  /** Move the synchronized "selected event" cursor. Pass null to
   *  hide it. Bumps version so chart watchers re-render the marker
   *  layer in lock-step. */
  function setCursorMs(ms: number | null) {
    const s = cur();
    s.cursorMs = ms;
    s.version++;
  }

  return {
    // `state` is a getter so each property read flows through cur(),
    // which reads playerIdRef.value. Vue templates / computeds that
    // touch `coord.state.X` therefore depend on playerIdRef and
    // re-evaluate when the active player switches.
    get state(): ChartCoordinationState { return cur(); },
    effectiveViewport,
    effectiveRange,
    isAtLiveEdge,
    tickPositions,
    noteSample,
    setViewport,
    togglePause,
    setPaused,
    setLiveSpanMs,
    setRange,
    setLiveSpan,
    toggleLive,
    toggleExpanded,
    setWindowMs,
    setBandwidthYMax,
    setCursorMs,
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
