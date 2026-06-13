/**
 * Compare-charts context (issue #579) — the bridge between SessionDisplay
 * (which owns the grouped-session identities + sibling time-series
 * subscriptions) and the individual chart components.
 *
 * In compare mode every session — the active one included — renders the
 * SAME canonical line set, each series tagged with its session's `(Sx)`
 * and told apart by dash pattern (the active session is solid, each
 * sibling gets a stable dash). SessionDisplay publishes:
 *   - `self`     — the active session's identity (tag + solid dash); the
 *                  charts build their primary `series` from it.
 *   - `siblings` — each grouped peer's identity + registered events
 *                  stream; the charts build their `overlays` from these.
 *
 * Colours are assigned per METRIC (in compareSeries), not per session, so
 * a metric reads the same hue on every session and the eye pairs them.
 */
import { computed, inject, type ComputedRef, type InjectionKey, type Ref } from 'vue';
import type { Stream } from '@/composables/useSessionTimeSeries';
import type { ChartOverlaySource, SeriesSpec } from '@/components/MetricsLineChart.vue';

/** The minimum a compareSeries builder needs to tag + style one
 *  session's lines: a short legend tag and the dash that identifies the
 *  session ([] = solid, used for the active session). */
export interface CompareSeriesIdentity {
  /** Short legend tag, e.g. `S3`. */
  tag: string;
  /** Dash pattern that identifies this session across all its lines.
   *  Empty array = solid (the active session). */
  dash: number[];
}

export interface CompareSibling extends CompareSeriesIdentity {
  /** Canonical player UUID of the sibling. */
  playerId: string;
  /** Human label, e.g. `#3 iPhone`. */
  label: string;
  /** Stable index among siblings — drives dash assignment. */
  index: number;
  /** This sibling's events stream (charts_minimal projection). */
  stream: Stream<Record<string, unknown>>;
  /** This sibling's AVMetrics stream (iOS-only on the wire — empty for
   *  non-iOS peers). Feeds the per-segment throughput dots so the
   *  bandwidth chart can merge every device's markers, not just the
   *  active session's (issue #486 compare-mode). */
  avmetricsStream?: Stream<Record<string, unknown>>;
}

/** Shared session-legend view state (issue #579). The S1/S2 chip row in
 *  SessionDisplay writes these; every chart reads them and highlights /
 *  shows / hides all lines for a session by its `Sx` tag, in lockstep. */
export interface CompareView {
  /** Tag of the session currently hovered in the legend (null = none).
   *  Charts pop that session's lines and dim the rest. */
  hovered: Ref<string | null>;
  /** Tags of sessions toggled off in the legend; charts hide all lines
   *  for those sessions. */
  hidden: Ref<Set<string>>;
}

export interface CompareContext {
  /** Is compare mode on for the active session? */
  enabled: Ref<boolean>;
  /** The active session's own identity (tag + solid dash), or null when
   *  compare mode is off. Charts build their primary `series` from this
   *  so the active session's lines get tagged + slimmed to match. */
  self: Ref<CompareSeriesIdentity | null>;
  /** Grouped siblings with registered streams (empty when off). */
  siblings: Ref<CompareSibling[]>;
  /** Session-legend hover/hide state, applied across every chart. */
  view: CompareView;
}

export const CompareContextKey: InjectionKey<CompareContext> = Symbol('compareContext');

/** Per-session dash patterns. The active session is solid (`[]`); each
 *  sibling takes one of these by its stable index so a device keeps its
 *  dash as peers come and go. */
export const SESSION_DASH: ReadonlyArray<number[]> = [
  [6, 4],          // dashed
  [2, 3],          // dotted
  [10, 4, 2, 4],   // dash-dot
  [4, 4],          // even dash
  [1, 3],          // fine dot
  [9, 3, 3, 3],    // long dash-dot
];
export function sessionDash(index: number): number[] {
  const n = SESSION_DASH.length;
  return SESSION_DASH[((index % n) + n) % n];
}

/** Per-session colours for the per-segment AVMetrics dots in compare mode.
 *  Lines tell sessions apart by DASH, but a dot can't carry a dash — so
 *  each session's per-segment markers take a distinct hue instead. The
 *  active session is slate (`SELF_MARKER_COLOR`, matching the single-
 *  session segment dot); each sibling takes the next hue by its stable
 *  index so a device keeps its colour as peers come and go. Issue #486. */
export const SELF_MARKER_COLOR = '#475569'; // slate — the single-session segment dot
export const SESSION_MARKER_COLORS: ReadonlyArray<string> = [
  '#0ea5e9', // sky
  '#ea580c', // orange
  '#16a34a', // green
  '#db2777', // pink
  '#7c3aed', // violet
  '#ca8a04', // amber
];
export function sessionMarkerColor(index: number): string {
  const n = SESSION_MARKER_COLORS.length;
  return SESSION_MARKER_COLORS[((index % n) + n) % n];
}

/**
 * useCompareOverlays — turn the provided compare context into a chart's
 * `overlays` prop. `specsFor` maps one session identity to the
 * SeriesSpec[] that chart wants (its own accessors, tagged + styled per
 * session). Returns [] when compare mode is off or no context is
 * provided (so a chart mounted outside SessionDisplay is unaffected).
 */
export function useCompareOverlays(
  specsFor: (id: CompareSeriesIdentity) => SeriesSpec[],
): ComputedRef<ChartOverlaySource[]> {
  const ctx = inject(CompareContextKey, null);
  return computed<ChartOverlaySource[]>(() => {
    if (!ctx || !ctx.enabled.value) return [];
    return ctx.siblings.value.map((sib) => ({
      key: sib.playerId,
      eventsStream: sib.stream,
      series: specsFor(sib),
    }));
  });
}

/** Inject the active session's compare identity (null when compare mode
 *  is off / outside a SessionDisplay). Charts use it to retag + slim
 *  their primary `series` so the active session matches the overlays. */
export function useCompareSelf(): ComputedRef<CompareSeriesIdentity | null> {
  const ctx = inject(CompareContextKey, null);
  return computed(() => (ctx && ctx.enabled.value ? ctx.self.value : null));
}

/** Inject the grouped siblings directly (empty when compare mode is off /
 *  outside a SessionDisplay). Unlike useCompareOverlays — which maps each
 *  sibling to chart SERIES — this hands back the raw sibling list so a
 *  chart can read per-sibling streams it overlays by hand (e.g. the
 *  bandwidth chart merging every sibling's per-segment AVMetrics dots,
 *  issue #486). */
export function useCompareSiblings(): ComputedRef<CompareSibling[]> {
  const ctx = inject(CompareContextKey, null);
  return computed(() => (ctx && ctx.enabled.value ? ctx.siblings.value : []));
}
