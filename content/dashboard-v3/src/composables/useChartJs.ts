/**
 * useChartJs() — lazy loader for Chart.js + zoom plugin. Loaded from
 * CDN so it doesn't bloat the page bundle for users who never expand
 * the metrics sections.
 */

let chartJsPromise: Promise<any> | null = null;

declare global {
  interface Window {
    Chart?: any;
    ChartZoom?: any;
  }
}

function loadScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const existing = document.querySelector<HTMLScriptElement>(`script[src="${src}"]`);
    if (existing) {
      if (existing.dataset.loaded === '1') return resolve();
      existing.addEventListener('load', () => resolve());
      existing.addEventListener('error', () => reject(new Error(`load failed: ${src}`)));
      return;
    }
    const s = document.createElement('script');
    s.src = src;
    s.onload = () => {
      s.dataset.loaded = '1';
      resolve();
    };
    s.onerror = () => reject(new Error(`load failed: ${src}`));
    document.head.appendChild(s);
  });
}

export function ensureChartJs(): Promise<any> {
  if (window.Chart) return Promise.resolve(window.Chart);
  if (chartJsPromise) return chartJsPromise;
  chartJsPromise = (async () => {
    await loadScript('https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js');
    await loadScript('https://cdn.jsdelivr.net/npm/chartjs-plugin-zoom@2.0.1/dist/chartjs-plugin-zoom.min.js');
    const Chart = window.Chart;
    const zoom = window.ChartZoom ?? (window as any).chartjsPluginZoom;
    if (Chart && zoom && !Chart.__zoomRegistered) {
      Chart.register(zoom);
      Chart.__zoomRegistered = true;
    }
    // Custom interaction mode: 'singleNearest'. Chart.js's built-in 'nearest'
    // hit-test throws "Cannot read properties of undefined (reading 'skip')" on
    // the metrics chart's band-FILL (fillToValue) + spanGaps overlay datasets,
    // whose elements arrays are intentionally sparse/undefined (compare mode is
    // the worst case). The 'x' mode tolerates those gaps; we delegate to it and
    // then return only the ONE non-fill line nearest the cursor's Y — so hover +
    // tooltip read out a single line (what the operator wants) without crashing.
    if (Chart?.Interaction?.modes?.x && !Chart.__singleNearestRegistered) {
      const xMode = Chart.Interaction.modes.x;
      Chart.Interaction.modes.singleNearest = (chart: any, e: any, options: any, useFinalPosition: any) => {
        const items = xMode(chart, e, { ...options, intersect: false }, useFinalPosition);
        if (!items || !items.length) return [];
        const cy = e && typeof e.y === 'number' ? e.y : null;
        let best: any = null;
        let bestD = Infinity;
        for (const it of items) {
          const ds = chart.data?.datasets?.[it.datasetIndex];
          if (ds && ds.fill) continue; // skip borderless avg↔peak band fills
          const ey = it.element?.y;
          if (typeof ey !== 'number') continue;
          const d = cy == null ? 0 : Math.abs(ey - cy);
          if (d < bestD) {
            bestD = d;
            best = it;
          }
        }
        return best ? [best] : [];
      };
      Chart.__singleNearestRegistered = true;
    }
    return Chart;
  })();
  return chartJsPromise;
}

let visNetworkPromise: Promise<any> | null = null;

// vis-network (the graph sibling of vis-timeline) — loaded from CDN, exposes
// window.vis.Network + window.vis.DataSet. Used by the Fault Sweep lineage graph.
export function ensureVisNetwork(): Promise<any> {
  if ((window as any).vis?.Network) return Promise.resolve((window as any).vis);
  if (visNetworkPromise) return visNetworkPromise;
  visNetworkPromise = (async () => {
    const css = document.createElement('link');
    css.rel = 'stylesheet';
    css.href = 'https://cdn.jsdelivr.net/npm/vis-network@9.1.9/styles/vis-network.min.css';
    document.head.appendChild(css);
    await loadScript('https://cdn.jsdelivr.net/npm/vis-network@9.1.9/standalone/umd/vis-network.min.js');
    return (window as any).vis;
  })();
  return visNetworkPromise;
}

let visTimelinePromise: Promise<any> | null = null;

export function ensureVisTimeline(): Promise<any> {
  if ((window as any).vis?.Timeline) return Promise.resolve((window as any).vis);
  if (visTimelinePromise) return visTimelinePromise;
  visTimelinePromise = (async () => {
    const css = document.createElement('link');
    css.rel = 'stylesheet';
    css.href = 'https://cdn.jsdelivr.net/npm/vis-timeline@7.7.3/styles/vis-timeline-graph2d.min.css';
    document.head.appendChild(css);
    await loadScript('https://cdn.jsdelivr.net/npm/vis-timeline@7.7.3/standalone/umd/vis-timeline-graph2d.min.js');
    return (window as any).vis;
  })();
  return visTimelinePromise;
}
