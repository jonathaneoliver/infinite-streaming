/**
 * v2-charts.js — chart rendering driven by V2Models.
 *
 * Replaces session-shell.js (5500 LOC of v1-aware DOM + Chart.js
 * spaghetti) with a thin layer that:
 *
 *   1. Constructs the four canonical charts (bandwidth, buffer, fps,
 *      network-events timeline) for a given Player.
 *   2. Subscribes to Player + NetworkLogEntry events and pushes the
 *      newest sample onto each chart.
 *   3. Pins the X-axis to a rolling time window (default 60s) with
 *      live-edge follow + Chart.js zoom plugin enabled for inspection.
 *
 * Charts hold no domain logic: the model is authoritative; the chart
 * just visualises whatever lands.
 *
 * Depends on: Chart.js + chartjs-plugin-zoom (loaded by the host page).
 *
 * Globals: window.V2Charts = { mountAll, BandwidthChart, BufferChart,
 *                              FpsChart, EventsTimeline }.
 */
(function (global) {
  "use strict";

  const ROLLING_WINDOW_MS = 60_000;

  // ---- Base chart helpers -------------------------------------------

  function commonChartOptions(yLabel) {
    return {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      plugins: {
        legend: { display: false },
        zoom: {
          pan: { enabled: true, mode: 'x' },
          zoom: { wheel: { enabled: true, modifierKey: 'shift' }, mode: 'x' },
        },
      },
      scales: {
        x: { type: 'linear', title: { display: true, text: 'time (s)' }, ticks: { stepSize: 5 } },
        y: { beginAtZero: true, title: { display: true, text: yLabel } },
      },
    };
  }

  function rollingWindow(samples) {
    if (!samples.length) return samples;
    const last = samples[samples.length - 1].x;
    const cutoff = last - ROLLING_WINDOW_MS / 1000;
    let i = 0;
    while (i < samples.length && samples[i].x < cutoff) i++;
    return i > 0 ? samples.slice(i) : samples;
  }

  // ---- BandwidthChart -----------------------------------------------
  //
  // Plots per-segment download throughput (Mbps) over time. One sample
  // per network log entry whose request_kind is 'segment' or 'init'.
  // Y-axis includes manifest variant ladder rungs as horizontal
  // reference lines (constant per play).

  function BandwidthChart(canvas, player) {
    this.player = player;
    this.samples = [];
    this.t0 = null;
    this.chart = new Chart(canvas, {
      type: 'line',
      data: { datasets: [{ label: 'throughput', data: [], borderColor: '#3b82f6', backgroundColor: 'rgba(59,130,246,0.15)', fill: true, tension: 0.2, pointRadius: 1.5 }] },
      options: commonChartOptions('Mbps'),
    });
  }
  BandwidthChart.prototype.push = function (entry) {
    if (!entry || !entry.timestamp) return;
    // For HLS: segments flow from proxy to player; bytes_out (server →
    // client) is the per-segment downlink size, which is what the
    // player measured throughput against. bytes_in is request-body
    // size and is 0 for GETs.
    const bytes = entry.bytesOut || entry.bytesIn;
    if (!bytes || !entry.totalMs) return;
    // Accept everything that isn't a manifest fetch — segments, partial
    // segments, init segments, audio segments.
    if (entry.requestKind && /manifest/i.test(entry.requestKind)) return;
    const tMs = Date.parse(entry.timestamp);
    if (!Number.isFinite(tMs)) return;
    if (this.t0 == null) this.t0 = tMs;
    const x = (tMs - this.t0) / 1000;
    const mbps = (bytes * 8) / (entry.totalMs * 1000); // bytes*8/ms*1000 = Mbps
    this.samples.push({ x, y: mbps });
    this.samples = rollingWindow(this.samples);
    this.chart.data.datasets[0].data = this.samples;
    this.chart.update('none');
  };
  BandwidthChart.prototype.destroy = function () { this.chart.destroy(); };

  // ---- BufferChart --------------------------------------------------
  //
  // Plots PlayerMetrics.bufferDepthS over time. Subscribes to the
  // Player's `change` event and samples on each metrics absorb.

  function BufferChart(canvas, player) {
    this.player = player;
    this.samples = [];
    this.t0 = null;
    this.chart = new Chart(canvas, {
      type: 'line',
      data: { datasets: [{ label: 'buffer (s)', data: [], borderColor: '#10b981', backgroundColor: 'rgba(16,185,129,0.15)', fill: true, tension: 0.25, pointRadius: 0 }] },
      options: commonChartOptions('seconds'),
    });
    this._unsubscribe = player.on('change', () => this._sample());
  }
  BufferChart.prototype._sample = function () {
    const m = this.player.metrics;
    if (!m || m.bufferDepthS == null) return;
    const now = Date.now();
    if (this.t0 == null) this.t0 = now;
    this.samples.push({ x: (now - this.t0) / 1000, y: m.bufferDepthS });
    this.samples = rollingWindow(this.samples);
    this.chart.data.datasets[0].data = this.samples;
    this.chart.update('none');
  };
  BufferChart.prototype.destroy = function () {
    if (this._unsubscribe) this._unsubscribe();
    this.chart.destroy();
  };

  // ---- FpsChart -----------------------------------------------------
  //
  // Plots video_quality_pct + stalls. Two y-axes: percent on left,
  // stall counter on right (step line).

  function FpsChart(canvas, player) {
    this.player = player;
    this.qualitySamples = [];
    this.stallSamples = [];
    this.t0 = null;
    this.chart = new Chart(canvas, {
      type: 'line',
      data: {
        datasets: [
          { label: 'quality %', data: [], borderColor: '#8b5cf6', backgroundColor: 'rgba(139,92,246,0.15)', yAxisID: 'y', tension: 0.25, pointRadius: 0 },
          { label: 'stalls', data: [], borderColor: '#ef4444', stepped: true, yAxisID: 'y2', pointRadius: 0 },
        ],
      },
      options: Object.assign({}, commonChartOptions('quality %'), {
        scales: {
          x: { type: 'linear', title: { display: true, text: 'time (s)' }, ticks: { stepSize: 5 } },
          y: { beginAtZero: true, max: 100, title: { display: true, text: 'quality %' }, position: 'left' },
          y2: { beginAtZero: true, title: { display: true, text: 'stalls' }, position: 'right', grid: { drawOnChartArea: false } },
        },
        plugins: { legend: { display: true } },
      }),
    });
    this._unsubscribe = player.on('change', () => this._sample());
  }
  FpsChart.prototype._sample = function () {
    const m = this.player.metrics;
    if (!m) return;
    const now = Date.now();
    if (this.t0 == null) this.t0 = now;
    const x = (now - this.t0) / 1000;
    if (m.videoQualityPct != null) {
      this.qualitySamples.push({ x, y: m.videoQualityPct });
      this.qualitySamples = rollingWindow(this.qualitySamples);
      this.chart.data.datasets[0].data = this.qualitySamples;
    }
    if (m.stalls != null) {
      this.stallSamples.push({ x, y: m.stalls });
      this.stallSamples = rollingWindow(this.stallSamples);
      this.chart.data.datasets[1].data = this.stallSamples;
    }
    this.chart.update('none');
  };
  FpsChart.prototype.destroy = function () {
    if (this._unsubscribe) this._unsubscribe();
    this.chart.destroy();
  };

  // ---- EventsTimeline ----------------------------------------------
  //
  // vis-timeline strip showing fault events + lifecycle markers.
  // One row per fault category; markers click → emit event the host
  // page can use to scroll the network log table.

  function EventsTimeline(container, player) {
    this.player = player;
    this.items = new vis.DataSet([]);
    this.groups = new vis.DataSet([
      { id: 'http', content: 'HTTP faults' },
      { id: 'transport', content: 'Transport' },
      { id: 'lifecycle', content: 'Lifecycle' },
    ]);
    // Initial window = last 60s ↔ +10s. vis-timeline defaults to a
    // multi-hour view when empty, which blew the fold out earlier.
    const now = Date.now();
    this.timeline = new vis.Timeline(container, this.items, this.groups, {
      stack: false,
      showCurrentTime: true,
      zoomMin: 1000,
      zoomMax: 1000 * 60 * 60,
      orientation: 'top',
      height: '200px',          // 3 lanes × ~60px → readable.
      maxHeight: '240px',
      start: new Date(now - 60_000),
      end: new Date(now + 10_000),
      moveable: true,
      zoomable: true,
    });
    this._counter = 0;
    // Seed a single "page-loaded" marker so the rail isn't visually
    // empty before the first network entry lands.
    this.items.add({
      id: ++this._counter,
      group: 'lifecycle',
      content: 'page loaded',
      start: now,
    });
  }
  EventsTimeline.prototype.push = function (entry) {
    if (!entry || !entry.faulted) return;
    const t = Date.parse(entry.timestamp);
    if (!Number.isFinite(t)) return;
    this.items.add({
      id: ++this._counter,
      group: entry.faultCategory === 'transport' ? 'transport' : 'http',
      content: entry.faultType || 'fault',
      start: t,
      title: (entry.method || '') + ' ' + (entry.path || entry.url || ''),
    });
  };
  EventsTimeline.prototype.pushLifecycle = function (label, ts) {
    const t = ts ? Date.parse(ts) : Date.now();
    this.items.add({
      id: ++this._counter,
      group: 'lifecycle',
      content: label,
      start: t,
    });
  };
  EventsTimeline.prototype.destroy = function () {
    this.timeline.destroy();
    this.items.clear();
    this.groups.clear();
  };

  // ---- mountAll -----------------------------------------------------
  //
  // Convenience: given a Player + a target DOM with four canvases /
  // containers, instantiate every chart and wire it to a network-log
  // poll loop.
  //
  //   const charts = V2Charts.mountAll(player, {
  //     bandwidthCanvas, bufferCanvas, fpsCanvas, eventsContainer,
  //     repo,
  //   });
  //   ...
  //   charts.destroy();

  function mountAll(player, opts) {
    const charts = {};
    if (opts.bandwidthCanvas) charts.bandwidth = new BandwidthChart(opts.bandwidthCanvas, player);
    if (opts.bufferCanvas)    charts.buffer    = new BufferChart(opts.bufferCanvas, player);
    if (opts.fpsCanvas)       charts.fps       = new FpsChart(opts.fpsCanvas, player);
    if (opts.eventsContainer) charts.events    = new EventsTimeline(opts.eventsContainer, player);

    // Poll the network log every 2s and hand new entries to the
    // bandwidth chart + events timeline. SSE has play.network.entry
    // for live streams — when wired through PlayersStore we'll stop
    // polling. For Phase Q3 we polling-only since SSE wiring lands in
    // Q5 with the per-session UI.
    let cancelled = false;
    let lastTimestamp = 0;
    async function pollNetwork() {
      if (cancelled || !opts.repo) return;
      try {
        const r = await opts.repo.networkLog(player.id, 200);
        if (r && r.ok && r.body && r.body.items) {
          for (const raw of r.body.items) {
            const ts = Date.parse(raw.timestamp || '');
            if (!Number.isFinite(ts) || ts <= lastTimestamp) continue;
            lastTimestamp = ts;
            const entry = new global.V2Models.NetworkLogEntry(raw);
            if (charts.bandwidth) charts.bandwidth.push(entry);
            if (charts.events) charts.events.push(entry);
          }
        }
      } catch (_) { /* swallow — next tick will retry */ }
      if (!cancelled) setTimeout(pollNetwork, 2000);
    }
    pollNetwork();

    return {
      charts,
      destroy() {
        cancelled = true;
        for (const c of Object.values(charts)) c.destroy();
      },
    };
  }

  global.V2Charts = {
    mountAll,
    BandwidthChart,
    BufferChart,
    FpsChart,
    EventsTimeline,
  };
})(window);
