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
