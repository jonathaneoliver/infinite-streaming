/**
 * useLegendVisibility(scopeKey) — persist an operator's per-legend show/hide
 * choices on a MetricsLineChart so they SURVIVE A NEW play_id.
 *
 * A new play rebuilds (or recreates) the chart's datasets, which otherwise reset
 * every legend toggle back to its `SeriesSpec.hidden` default. This store records
 * the operator's explicit choices and re-applies them whenever datasets are
 * (re)built, so toggles persist across plays.
 *
 * Backing store: module-level Map keyed by `scopeKey` (the caller passes
 * `${coordId}::${chartTitle}` — per-chart, per-player), so the choices also
 * survive a component remount — the same persistence guarantee
 * useChartCoordination gives the viewport.
 *
 * Within a chart a series is keyed by its `groupLegend` (grouped ladder lines
 * toggle as one chip) or, ungrouped, by its `label`.
 */

export interface LegendKeyed {
  groupLegend?: string | null;
  label: string;
  hidden?: boolean;
}

const stores = new Map<string, Map<string, boolean>>();

function keyFor(s: LegendKeyed): string {
  return s.groupLegend ? 'g:' + s.groupLegend : 'l:' + s.label;
}

export function useLegendVisibility(scopeKey: string) {
  let store = stores.get(scopeKey);
  if (!store) {
    store = new Map<string, boolean>();
    stores.set(scopeKey, store);
  }
  const s = store;
  return {
    /** Effective initial hidden state: the operator's persisted choice for this
     *  legend if they've toggled it, otherwise the series' own `hidden` default. */
    resolveHidden(spec: LegendKeyed): boolean {
      const k = keyFor(spec);
      return s.has(k) ? !!s.get(k) : !!spec.hidden;
    },
    /** Record the operator's explicit show/hide choice for a series' legend. */
    setHidden(spec: LegendKeyed, hidden: boolean): void {
      s.set(keyFor(spec), hidden);
    },
  };
}
