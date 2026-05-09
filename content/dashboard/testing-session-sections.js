/**
 * testing-session-sections.js — section render functions for the
 * v2-native testing-session.html. Each section is a pure render
 * function; mountAll wires every one to a Player and re-renders on
 * `change` events.
 *
 * Phase Q5 sub-phases lay down sections incrementally:
 *   Q5c (this file, in progress) — Session Details + Player Metrics
 *     (read-only telemetry views).
 *   Q5d-Q5j extend this same file with Fault Injection / Server
 *     Timeouts / Network Shaping / Charts / Network Log / Group.
 *
 * Hard rule: never `innerHTML` a model string. Use the t() helper
 * (textNode). Static markup may inline HTML.
 *
 * Globals: window.TestingSessionSections = { mountAll }.
 */
(() => {
  "use strict";

  function t(s) { return document.createTextNode(String(s == null ? '' : s)); }

  // ---- Collapsible helper ------------------------------------------
  // Mirrors the v1 .collapsible-section / .collapsible-header / .collapsible-content
  // markup so the same testing-session-refactored.css applies. Local
  // storage key per section persists open/closed across reloads.

  function collapsible(parent, name, title, defaultOpen, onMount) {
    const lsKey = 'ismCollapse:' + name;
    const stored = localStorage.getItem(lsKey);
    const open = stored ? stored === '1' : !!defaultOpen;

    const section = document.createElement('div');
    section.className = 'collapsible-section';
    section.dataset.section = name;
    section.dataset.defaultOpen = String(open);

    const header = document.createElement('div');
    header.className = 'collapsible-header';
    header.dataset.toggle = name;

    const icon = document.createElement('span');
    icon.className = 'collapsible-icon';
    icon.textContent = open ? '▼' : '▶';
    header.appendChild(icon);

    const titleEl = document.createElement('span');
    titleEl.className = 'collapsible-title';
    titleEl.appendChild(t(title));
    header.appendChild(titleEl);

    const badge = document.createElement('span');
    badge.className = 'collapsible-badge';
    badge.style.display = 'none';
    header.appendChild(badge);

    const content = document.createElement('div');
    content.className = 'collapsible-content';
    content.dataset.content = name;
    content.style.display = open ? 'block' : 'none';

    header.style.cursor = 'pointer';
    header.addEventListener('click', () => {
      const isOpen = content.style.display !== 'none';
      content.style.display = isOpen ? 'none' : 'block';
      icon.textContent = isOpen ? '▶' : '▼';
      localStorage.setItem(lsKey, isOpen ? '0' : '1');
    });

    section.appendChild(header);
    section.appendChild(content);
    parent.appendChild(section);

    return { section, header, content, badge, setBadge: (text) => {
      if (text == null || text === '') { badge.style.display = 'none'; return; }
      badge.textContent = text;
      badge.style.display = '';
    }};
  }

  // ---- session-grid render helper ----------------------------------
  // Build a `<div class="session-grid">` with N `<div class="session-item">`
  // children. Items skipped when value is null/empty AND `skipEmpty` true.
  function renderSessionGrid(parent, items, opts) {
    opts = opts || {};
    const grid = document.createElement('div');
    grid.className = 'session-grid';
    for (const it of items) {
      const empty = it.value == null || it.value === '';
      if (empty && opts.skipEmpty) continue;
      const item = document.createElement('div');
      item.className = 'session-item';
      const lbl = document.createElement('span');
      lbl.className = 'label';
      lbl.appendChild(t(it.label));
      item.appendChild(lbl);
      const val = document.createElement('span');
      val.className = 'value';
      val.appendChild(t(empty ? '—' : it.value));
      if (it.title) val.title = it.title;
      item.appendChild(val);
      grid.appendChild(item);
    }
    parent.appendChild(grid);
    return grid;
  }

  // ---- Field formatters --------------------------------------------
  function fmtDate(v) {
    if (!v) return null;
    try { return new Date(v).toLocaleString(); } catch (_) { return String(v); }
  }
  function fmtSeconds(v, digits) {
    if (v == null || !Number.isFinite(Number(v))) return null;
    return Number(v).toFixed(digits == null ? 2 : digits) + ' s';
  }
  function fmtMbps(v, digits) {
    if (v == null || !Number.isFinite(Number(v))) return null;
    return Number(v).toFixed(digits == null ? 2 : digits) + ' Mbps';
  }
  function fmtMs(v, digits) {
    if (v == null || !Number.isFinite(Number(v))) return null;
    return Number(v).toFixed(digits == null ? 1 : digits) + ' ms';
  }
  function fmtPct(v, digits) {
    if (v == null || !Number.isFinite(Number(v))) return null;
    return Number(v).toFixed(digits == null ? 1 : digits) + '%';
  }
  function fmtBytes(v) {
    if (v == null || !Number.isFinite(Number(v))) return null;
    const n = Number(v);
    if (n >= 1e9) return (n / 1e9).toFixed(2) + ' GB';
    if (n >= 1e6) return (n / 1e6).toFixed(2) + ' MB';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + ' KB';
    return n.toLocaleString() + ' B';
  }
  function fmtDuration(startISO, endISOOrNow) {
    if (!startISO) return null;
    const start = Date.parse(startISO);
    if (!Number.isFinite(start)) return null;
    const end = endISOOrNow ? Date.parse(endISOOrNow) : Date.now();
    const sec = Math.max(0, Math.round((end - start) / 1000));
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    if (h > 0) return h + 'h ' + m + 'm ' + s + 's';
    if (m > 0) return m + 'm ' + s + 's';
    return s + 's';
  }

  // ==== Q5c: Session Details =======================================
  // The v1 page exposed ~18 fields here, half of them deep v1
  // instrumentation (mbps_shaper_*, measurement_window_*, etc.) that
  // doesn't have a v2 equivalent. Everything that v2 currently
  // surfaces is rendered; legacy-only fields are intentionally pruned.
  // Adding more fields means extending v2 spec → translate.go → this
  // function.

  function renderSessionDetails(content, player) {
    content.innerHTML = '';
    const cp = player.currentPlay || {};
    const sm = player.serverMetrics || {};
    const items = [
      { label: 'Player ID', value: player.id, title: 'v2 player UUID' },
      { label: 'Display ID', value: '#' + player.displayId },
      { label: 'Play ID', value: cp.id || null,
        title: 'Server-issued UUIDv7 of the active play. Fresh on each new play.' },
      { label: 'User Agent', value: player.userAgent },
      { label: 'Player IP', value: player.playerIp,
        title: "Player's self-reported IP. Differs from origination_ip when behind NAT." },
      { label: 'Origination IP', value: player.originationIp },
      { label: 'Origination Time', value: fmtDate(player.firstSeenAt) },
      { label: 'Last Request', value: fmtDate(player.lastSeenAt) },
      { label: 'First Request', value: fmtDate(player.firstSeenAt) },
      { label: 'Session Duration', value: fmtDuration(player.firstSeenAt, player.lastSeenAt) },
      { label: 'Manifest URL', value: cp.manifest && cp.manifest.master_url || null },
      { label: 'Master Manifest URL', value: cp.manifest && cp.manifest.master_url || null },
      { label: 'Last Request URL', value: null },  // not surfaced in v2 yet
      { label: 'Loop Count (server)',
        value: player.loopCountServer != null ? String(player.loopCountServer) : null,
        title: 'Server-counted loop boundaries observed on the manifest timeline.' },
      { label: 'Shaper Avg', value: fmtMbps(sm.mbpsShaperAvg) },
      { label: 'Shaper Rate', value: fmtMbps(sm.mbpsShaperRate) },
      { label: 'Transfer Rate', value: fmtMbps(sm.mbpsTransferRate) },
      { label: 'Transfer Complete', value: fmtMbps(sm.mbpsTransferComplete) },
      { label: 'Mbps In', value: fmtMbps(sm.mbpsIn) },
      { label: 'Mbps Out', value: fmtMbps(sm.mbpsOut) },
      { label: 'Mbps In Avg', value: fmtMbps(sm.mbpsInAvg) },
      { label: 'Mbps In Active', value: fmtMbps(sm.mbpsInActive) },
      { label: 'Bytes In (total)', value: fmtBytes(sm.bytesInTotal) },
      { label: 'Bytes Out (total)', value: fmtBytes(sm.bytesOutTotal) },
      { label: 'Bytes In (last)', value: fmtBytes(sm.bytesInLast) },
      { label: 'Bytes Out (last)', value: fmtBytes(sm.bytesOutLast) },
      { label: 'Measured Mbps', value: fmtMbps(sm.measuredMbps) },
      { label: 'Measurement Window I/O',
        value: sm.measurementWindowIo != null ? sm.measurementWindowIo.toFixed(2) + ' s' : null },
      { label: 'Measurement Window Active',
        value: sm.measurementWindowActive != null ? sm.measurementWindowActive.toFixed(2) + ' s' : null },
      { label: 'Control Revision', value: player.controlRevision,
        title: 'ETag/If-Match concurrency token.' },
    ];
    renderSessionGrid(content, items);
  }

  // ==== Q5c: Player Metrics ========================================
  // PlayerMetrics + ServerMetrics combined here for parity with v1.
  // Each row only renders when the value exists (skipEmpty=true) so a
  // fresh / silent player doesn't show a wall of dashes.

  function renderPlayerMetrics(content, player) {
    content.innerHTML = '';
    const pm = player.metrics || {};
    const sm = player.serverMetrics || {};
    const fc = player.faultCounters;

    const items = [
      // ---- Lifecycle ----
      { label: 'Last Event', value: pm.lastEvent },
      { label: 'Trigger Type', value: pm.triggerType },
      { label: 'Event Time', value: fmtDate(pm.eventTime) },
      { label: 'State', value: pm.state },
      { label: 'Source', value: pm.source },
      { label: 'Last Error', value: pm.error },

      // ---- Position / playback ----
      { label: 'Position', value: fmtSeconds(pm.positionS) },
      { label: 'Playback Rate',
        value: pm.playbackRate != null ? pm.playbackRate.toFixed(2) + 'x' : null },

      // ---- Buffer / live ----
      { label: 'Buffer Depth', value: fmtSeconds(pm.bufferDepthS) },
      { label: 'Buffer End', value: fmtSeconds(pm.bufferEndS) },
      { label: 'Seekable End', value: fmtSeconds(pm.seekableEndS) },
      { label: 'Live Edge', value: fmtSeconds(pm.liveEdgeS) },
      { label: 'Live Offset', value: fmtSeconds(pm.liveOffsetS) },
      { label: 'Wall-Clock Offset', value: fmtSeconds(pm.trueOffsetS) },

      // ---- Resolution / bitrate ----
      { label: 'Display Resolution', value: pm.displayResolution },
      { label: 'Video Resolution', value: pm.videoResolution },
      { label: 'First Frame Time', value: fmtSeconds(pm.firstFrameTimeS) },
      { label: 'Video Start Time', value: fmtSeconds(pm.videoStartTimeS) },
      { label: 'Video Bitrate', value: fmtMbps(pm.videoBitrateMbps) },
      { label: 'Server Rendition', value: sm.serverRendition },
      { label: 'Server Rendition Mbps', value: fmtMbps(sm.renditionMbps) },
      { label: 'Video Quality', value: fmtPct(pm.videoQualityPct) },
      { label: 'avgNetworkBitrate', value: fmtMbps(pm.avgNetworkBitrateMbps) },
      { label: 'networkBitrate', value: fmtMbps(pm.networkBitrateMbps) },

      // ---- Counters ----
      { label: 'Loop Count (player)',
        value: pm.loopCountPlayer != null ? String(pm.loopCountPlayer) : null,
        title: 'Player-reported loop count. May be 0 on platforms that do not count loops.' },
      { label: 'Loop Increment',
        value: pm.loopCountIncrement != null ? String(pm.loopCountIncrement) : null,
        title: 'Server-derived increment from the previous report.' },
      { label: 'Profile Shifts',
        value: pm.profileShiftCount != null ? String(pm.profileShiftCount) : null },
      { label: 'Frames Displayed',
        value: pm.framesDisplayed != null ? String(pm.framesDisplayed) : null },
      { label: 'Dropped Frames',
        value: pm.droppedFrames != null ? String(pm.droppedFrames) : null },
      { label: 'Stalls', value: pm.stalls != null ? String(pm.stalls) : null },
      { label: 'Player Restarts',
        value: pm.playerRestarts != null ? String(pm.playerRestarts) : null },
      { label: 'Stall Time', value: fmtSeconds(pm.stallTimeS) },
      { label: 'Last Stall Time', value: fmtSeconds(pm.lastStallTimeS) },

      // ---- RTT / transport (ServerMetrics) ----
      { label: 'RTT', value: fmtMs(sm.rttMs) },
      { label: 'RTT min', value: fmtMs(sm.rttMinMs) },
      { label: 'RTT max', value: fmtMs(sm.rttMaxMs) },
      { label: 'RTT min lifetime', value: fmtMs(sm.rttMinLifetimeMs) },
      { label: 'RTT variance', value: fmtMs(sm.rttVarMs) },
      { label: 'RTO', value: fmtMs(sm.rtoMs) },
      { label: 'Path ping RTT', value: fmtMs(sm.pathPingRttMs) },
      { label: 'RTT stale',
        value: sm.rttStale == null ? null : (sm.rttStale ? 'yes' : 'no') },

      // ---- Fault counters ----
      { label: 'Faults total',
        value: fc ? String(fc.total()) : null,
        title: 'Sum of every fault_count_* family on the device.' },
    ];
    // Render every field — show "—" for empty values so the user sees
    // what's tracked rather than a phantom-empty panel on a fresh
    // synthetic player.
    renderSessionGrid(content, items);

    // If there are per-kind fault counters, surface them as a second
    // grid so the totals row reads cleanly above.
    if (fc) {
      const kinds = Object.keys(fc.byKind).filter(k => k !== 'total' && fc.byKind[k] > 0);
      if (kinds.length > 0) {
        const sep = document.createElement('div');
        sep.style.cssText = 'font-size:11px;color:#6b7280;margin:8px 0 4px;font-weight:600;';
        sep.appendChild(t('Fault breakdown:'));
        content.appendChild(sep);
        renderSessionGrid(content, kinds.map(k => ({
          label: k, value: String(fc.byKind[k]),
        })));
      }
    }
  }

  // ==== Q5d: Fault Injection (6 tabs) ==============================
  //
  // Tab → v2 mapping:
  //   All        → fault_rules[id="tab-all"]        no filter (default-all)
  //   Segment    → fault_rules[id="tab-segment"]    filter.request_kind=[segment, partial, init]
  //   Manifest   → fault_rules[id="tab-manifest"]   filter.request_kind=[manifest, audio_manifest]
  //   Master     → fault_rules[id="tab-master"]     filter.request_kind=[master_manifest]
  //   Transport  → player.shape.transportFault                         (v2 lives on Shape)
  //   Content    → player.content.{strip*, overstate, allowed, offset} (v2 lives on Player)
  //
  // Stable rule.id by tab lets us upsert/remove without an array
  // walk; the v2 spec allows arbitrary rule IDs.

  const HTTP_FAULT_TYPES = [
    'none', '404', '500', '503', 'timeout', 'corrupted',
    'request_connect_hang', 'request_connect_reset', 'request_connect_delayed',
    'request_first_byte_hang', 'request_first_byte_reset', 'request_first_byte_delayed',
    'request_body_hang', 'request_body_reset',
  ];
  const SEGMENT_FAULT_TYPES = HTTP_FAULT_TYPES; // segment supports the same set
  const MANIFEST_FAULT_TYPES = HTTP_FAULT_TYPES.filter(t => t !== 'corrupted');
  const FAULT_MODES = [
    { value: 'failures_per_seconds', label: 'Failures / Sec' },
    { value: 'requests',             label: 'Requests' },
  ];

  const TAB_DEFS = [
    { id: 'tab-all',      tab: 'all-failures',      label: 'All',
      types: HTTP_FAULT_TYPES,     filter: null },
    { id: 'tab-segment',  tab: 'segment-failures',  label: 'Segment',
      types: SEGMENT_FAULT_TYPES,  filter: { request_kind: ['segment', 'partial', 'init'] } },
    { id: 'tab-manifest', tab: 'manifest-failures', label: 'Manifest',
      types: MANIFEST_FAULT_TYPES, filter: { request_kind: ['manifest', 'audio_manifest'] } },
    { id: 'tab-master',   tab: 'master-failures',   label: 'Master',
      types: MANIFEST_FAULT_TYPES, filter: { request_kind: ['master_manifest'] } },
    { id: 'tab-transport',tab: 'transport-faults',  label: 'Transport' },
    { id: 'tab-content',  tab: 'content-manipulation', label: 'Content' },
  ];

  function ruleByID(player, id) {
    return player.faultRules.rules.find(r => r.id === id) || null;
  }

  function renderFaultInjection(content, player) {
    content.innerHTML = '';

    const wrap = document.createElement('div');
    wrap.className = 'fault-injection-section';
    const tabsContainer = document.createElement('div');
    tabsContainer.className = 'tabs-container';
    const header = document.createElement('div');
    header.className = 'tabs-header';
    const tabsContent = document.createElement('div');
    tabsContent.className = 'tabs-content';

    // Per-tab buttons + panels
    const panels = {};
    for (let i = 0; i < TAB_DEFS.length; i++) {
      const def = TAB_DEFS[i];
      const btn = document.createElement('button');
      btn.className = 'tab-button' + (i === 0 ? ' active' : '');
      btn.dataset.tab = def.tab;
      btn.appendChild(t(def.label));
      header.appendChild(btn);

      const panel = document.createElement('div');
      panel.className = 'tab-panel' + (i === 0 ? ' active' : '');
      panel.dataset.panel = def.tab;
      tabsContent.appendChild(panel);
      panels[def.tab] = panel;
    }

    tabsContainer.appendChild(header);
    tabsContainer.appendChild(tabsContent);
    wrap.appendChild(tabsContainer);
    content.appendChild(wrap);

    // Tab switching — match v1 .tab-button.active / .tab-panel.active
    header.addEventListener('click', (e) => {
      const target = e.target.closest('.tab-button');
      if (!target) return;
      const tabName = target.dataset.tab;
      header.querySelectorAll('.tab-button').forEach(b => b.classList.toggle('active', b === target));
      tabsContent.querySelectorAll('.tab-panel').forEach(p => p.classList.toggle('active', p.dataset.panel === tabName));
    });

    // ---- Fill each panel ----
    renderHttpFaultPanel(panels['all-failures'],      player, TAB_DEFS[0]);
    renderHttpFaultPanel(panels['segment-failures'],  player, TAB_DEFS[1]);
    renderHttpFaultPanel(panels['manifest-failures'], player, TAB_DEFS[2]);
    renderHttpFaultPanel(panels['master-failures'],   player, TAB_DEFS[3]);
    renderTransportPanel(panels['transport-faults'],  player);
    renderContentPanel(panels['content-manipulation'], player);
  }

  function renderHttpFaultPanel(panel, player, def) {
    panel.innerHTML = '';
    const existing = ruleByID(player, def.id);
    // Defaults: 0/0 until the user explicitly turns the rule on. Matches
    // v1 behaviour — sliders sit at zero on a fresh session.
    const cur = existing || { type: 'none', frequency: 0, consecutive: 0, mode: 'failures_per_seconds', filter: null };

    const setRow = (label, child) => {
      const row = document.createElement('div');
      row.className = 'fault-control-row';
      const lbl = document.createElement('label');
      lbl.appendChild(t(label));
      row.appendChild(lbl);
      row.appendChild(child);
      panel.appendChild(row);
      return row;
    };

    // Failure Type radio group
    const typeGroup = document.createElement('div');
    typeGroup.className = 'radio-group';
    for (const ty of def.types) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = def.id + '_type';
      inp.value = ty;
      inp.checked = cur.type === ty;
      inp.addEventListener('change', () => upsertHttpRule(player, def, { type: ty }));
      lbl.appendChild(inp);
      lbl.appendChild(t(' ' + ty));
      typeGroup.appendChild(lbl);
    }
    setRow('Failure Type', typeGroup);

    // Scope: variant URL checkboxes — picked from the active play's
    // manifest variants. v1 calls this "Targeted URLs". v2 stores the
    // selection on rule.filter.url_substring so the proxy can match
    // any segment whose URL contains the chosen variant tag.
    const scopeGroup = renderScopeCheckboxes(player, def, cur);
    setRow('Scope', scopeGroup);

    // Mode dropdown
    const modeSel = document.createElement('select');
    for (const m of FAULT_MODES) {
      const opt = document.createElement('option');
      opt.value = m.value;
      opt.appendChild(t(m.label));
      if (cur.mode === m.value) opt.selected = true;
      modeSel.appendChild(opt);
    }
    modeSel.addEventListener('change', () => upsertHttpRule(player, def, { mode: modeSel.value }));
    setRow('Mode', modeSel);

    // Consecutive slider
    setRow('Consecutive', sliderRow(cur.consecutive, 0, 10, 1, (v) => {
      upsertHttpRule(player, def, { consecutive: v });
    }));

    // Frequency slider
    setRow('Frequency', sliderRow(cur.frequency, 0, 30, 1, (v) => {
      upsertHttpRule(player, def, { frequency: v });
    }));
  }

  // Render variant-scope checkboxes from the active play's manifest.
  // Each checked variant pins the rule via filter.variant.resolutions
  // (the v2-native variant predicate — survives encoding pipeline
  // renames since it reads from the manifest, not URL conventions).
  function renderScopeCheckboxes(player, def, rule) {
    const grp = document.createElement('div');
    grp.className = 'checkbox-group';
    const variants = player.currentPlay && player.currentPlay.manifest
      && player.currentPlay.manifest.variants || [];
    if (!variants.length) {
      const note = document.createElement('span');
      note.style.cssText = 'font-size:11px;color:#9ca3af;';
      note.appendChild(t('(variants populate after first manifest fetch)'));
      grp.appendChild(note);
      return grp;
    }
    const sel = (rule.filter && rule.filter.variant && rule.filter.variant.resolutions) || [];
    for (const v of variants) {
      if (!v.resolution) continue;
      const wrap = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'checkbox';
      inp.checked = sel.indexOf(v.resolution) >= 0;
      inp.addEventListener('change', () => {
        const next = inp.checked
          ? sel.concat([v.resolution]).filter((x, i, a) => a.indexOf(x) === i)
          : sel.filter(s => s !== v.resolution);
        const filter = Object.assign({}, def.filter || {});
        if (next.length > 0) {
          filter.variant = { resolutions: next };
        } else {
          delete filter.variant;
        }
        upsertHttpRule(player, def, {
          filter: Object.keys(filter).length > 0 ? filter : null,
        });
      });
      wrap.appendChild(inp);
      const bw = v.bandwidth ? ' (' + Math.round(v.bandwidth / 1000) + 'k)' : '';
      wrap.appendChild(t(' ' + v.resolution + bw));
      grp.appendChild(wrap);
    }
    return grp;
  }

  function upsertHttpRule(player, def, patch) {
    const existing = ruleByID(player, def.id);
    if (!existing) {
      // Create the rule for this tab.
      const rule = Object.assign({
        id: def.id, type: 'none', frequency: 0, consecutive: 0, mode: 'failures_per_seconds',
      }, patch);
      if (def.filter) rule.filter = def.filter;
      // Filter on type=none means inactive — we still create so the
      // settings survive even when no fault is being injected.
      player.faultRules.append(rule);
      return;
    }
    // Patch only changed field(s) — repo coalesces in 50ms.
    player.faultRules.update(def.id, patch);
  }

  function sliderRow(value, min, max, step, onChange) {
    const wrap = document.createElement('div');
    wrap.className = 'range-row';
    const inp = document.createElement('input');
    inp.type = 'range';
    inp.min = String(min); inp.max = String(max); inp.step = String(step);
    inp.value = String(value == null ? min : value);
    const display = document.createElement('span');
    display.className = 'range-value';
    display.textContent = String(value == null ? min : value);
    inp.addEventListener('input', () => {
      display.textContent = inp.value;
      onChange(parseFloat(inp.value));
    });
    wrap.appendChild(inp);
    wrap.appendChild(display);
    return wrap;
  }

  function renderTransportPanel(panel, player) {
    panel.innerHTML = '';
    const tf = player.shape.transportFault || {};

    const setRow = (label, child) => {
      const row = document.createElement('div');
      row.className = 'fault-control-row';
      const lbl = document.createElement('label');
      lbl.appendChild(t(label));
      row.appendChild(lbl);
      row.appendChild(child);
      panel.appendChild(row);
    };

    // Fault type radio
    const typeGroup = document.createElement('div');
    typeGroup.className = 'radio-group';
    for (const ty of ['none', 'drop', 'reject']) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'transport_fault_type';
      inp.value = ty;
      inp.checked = (tf.type || 'none') === ty;
      inp.addEventListener('change', () => {
        const next = ty === 'none' ? null
          : Object.assign({}, tf, { type: ty });
        player.shape.setTransportFault(next);
      });
      lbl.appendChild(inp);
      lbl.appendChild(t(' ' + ty));
      typeGroup.appendChild(lbl);
    }
    setRow('Fault Type', typeGroup);

    // Mode radio
    const modeGroup = document.createElement('div');
    modeGroup.className = 'radio-group';
    for (const m of [
      { v: 'failures_per_seconds', l: 'Pkts / Sec' },
      { v: 'requests', l: 'Seconds' },
    ]) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'transport_fault_mode';
      inp.value = m.v;
      inp.checked = (tf.mode || 'failures_per_seconds') === m.v;
      inp.addEventListener('change', () => {
        player.shape.setTransportFault(Object.assign({}, tf, { mode: m.v }));
      });
      lbl.appendChild(inp);
      lbl.appendChild(t(' ' + m.l));
      modeGroup.appendChild(lbl);
    }
    setRow('Mode', modeGroup);

    // Consecutive slider
    setRow('Consecutive', sliderRow(tf.consecutive == null ? 1 : tf.consecutive, 0, 100, 1, (v) => {
      player.shape.setTransportFault(Object.assign({}, tf, { consecutive: v }));
    }));
    // Frequency slider
    setRow('Frequency (s)', sliderRow(tf.frequency == null ? 0 : tf.frequency, 0, 60, 1, (v) => {
      player.shape.setTransportFault(Object.assign({}, tf, { frequency: v }));
    }));

    // Read-only counters
    const fc = player.faultCounters && player.faultCounters.byKind;
    if (fc) {
      const dropPkts = fc.transport_drop || fc.transport_fault_drop || 0;
      const rejPkts = fc.transport_reject || fc.transport_fault_reject || 0;
      const counters = document.createElement('div');
      counters.className = 'session-item';
      counters.innerHTML = '<span class="label">Counters</span>';
      const v = document.createElement('span');
      v.className = 'value';
      v.appendChild(t('Drop ' + dropPkts + ' pkts · Reject ' + rejPkts + ' pkts'));
      counters.appendChild(v);
      panel.appendChild(counters);
    }
  }

  function renderContentPanel(panel, player) {
    panel.innerHTML = '';
    const c = player.content;

    function checkboxRow(label, hint, getter, setter) {
      const row = document.createElement('div');
      row.className = 'fault-control-row';
      const lbl = document.createElement('label');
      lbl.appendChild(t(label));
      row.appendChild(lbl);
      const grp = document.createElement('div');
      grp.className = 'checkbox-group';
      const wrap = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'checkbox';
      inp.checked = !!getter();
      inp.addEventListener('change', () => setter(inp.checked));
      wrap.appendChild(inp);
      wrap.appendChild(t(' ' + hint));
      grp.appendChild(wrap);
      row.appendChild(grp);
      panel.appendChild(row);
    }
    checkboxRow('Strip CODECS', 'Remove CODEC attributes from master playlist',
      () => c && c.stripCodecs, (b) => c && c.setStripCodecs(b));
    checkboxRow('Strip AVG-BANDWIDTH', 'Remove AVERAGE-BANDWIDTH from master playlist',
      () => c && c.stripAverageBandwidth, (b) => c && c.setStripAverageBandwidth(b));
    checkboxRow('Overstate Bandwidth', 'Inflate BANDWIDTH by 10%',
      () => c && c.overstateBandwidth, (b) => c && c.setOverstateBandwidth(b));

    // Live Offset radio
    const row = document.createElement('div');
    row.className = 'fault-control-row';
    const lbl = document.createElement('label');
    lbl.appendChild(t('Live Offset'));
    row.appendChild(lbl);
    const grp = document.createElement('div');
    grp.className = 'radio-group';
    for (const v of [0, 6, 18, 24]) {
      const wrap = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'content_live_offset';
      inp.value = String(v);
      inp.checked = (c && c.liveOffset || 0) === v;
      inp.addEventListener('change', () => c && c.setLiveOffset(v));
      wrap.appendChild(inp);
      wrap.appendChild(t(' ' + (v === 0 ? 'None' : v + 's')));
      grp.appendChild(wrap);
    }
    row.appendChild(grp);
    panel.appendChild(row);

    const note = document.createElement('div');
    note.className = 'content-tab-note';
    note.style.cssText = 'margin-top:8px;font-size:11px;color:#6b7280;';
    note.appendChild(t('Content modifications apply to master playlist requests. For HLS, replay after configuring to apply changes.'));
    panel.appendChild(note);
  }

  // ==== Q5e: Server Timeouts =======================================
  // Apply-To checkboxes (segments / manifests / master), active +
  // idle timeout sliders, fault counters readout. Patches go to
  // player.transferTimeouts (Q5a model layer).

  function renderServerTimeouts(content, player) {
    content.innerHTML = '';
    const tt = player.transferTimeouts;

    function checkbox(label, getter, setter) {
      const wrap = document.createElement('label');
      wrap.style.cssText = 'font-size:12px;display:flex;align-items:center;gap:6px;margin-right:12px;';
      const inp = document.createElement('input');
      inp.type = 'checkbox';
      inp.checked = !!getter();
      inp.addEventListener('change', () => setter(inp.checked));
      wrap.appendChild(inp);
      wrap.appendChild(t(label));
      return wrap;
    }

    const appliesRow = document.createElement('div');
    appliesRow.className = 'fault-control-row';
    const appliesLbl = document.createElement('label');
    appliesLbl.appendChild(t('Apply To'));
    appliesRow.appendChild(appliesLbl);
    const appliesGroup = document.createElement('div');
    appliesGroup.className = 'checkbox-group';
    appliesGroup.appendChild(checkbox('Segments',
      () => tt.appliesSegments, (b) => tt.setAppliesSegments(b)));
    appliesGroup.appendChild(checkbox('Media manifests',
      () => tt.appliesManifests, (b) => tt.setAppliesManifests(b)));
    appliesGroup.appendChild(checkbox('Master manifest',
      () => tt.appliesMaster, (b) => tt.setAppliesMaster(b)));
    appliesRow.appendChild(appliesGroup);
    content.appendChild(appliesRow);

    const activeRow = document.createElement('div');
    activeRow.className = 'fault-control-row';
    const activeLbl = document.createElement('label');
    activeLbl.appendChild(t('Active timeout (s)'));
    activeRow.appendChild(activeLbl);
    activeRow.appendChild(sliderRow(tt.activeTimeoutSeconds, 0, 30, 1, (v) => tt.setActive(v)));
    content.appendChild(activeRow);

    const idleRow = document.createElement('div');
    idleRow.className = 'fault-control-row';
    const idleLbl = document.createElement('label');
    idleLbl.appendChild(t('Idle timeout (s)'));
    idleRow.appendChild(idleLbl);
    idleRow.appendChild(sliderRow(tt.idleTimeoutSeconds, 0, 30, 1, (v) => tt.setIdle(v)));
    content.appendChild(idleRow);

    // Counters
    const fc = player.faultCounters && player.faultCounters.byKind;
    const counterEl = document.createElement('div');
    counterEl.className = 'session-item';
    const cl = document.createElement('span'); cl.className = 'label'; cl.appendChild(t('Counters'));
    const cv = document.createElement('span'); cv.className = 'value';
    const active = fc && fc.transfer_active_timeout || 0;
    const idle = fc && fc.transfer_idle_timeout || 0;
    cv.appendChild(t('Active ' + active + ' · Idle ' + idle));
    counterEl.appendChild(cl); counterEl.appendChild(cv);
    content.appendChild(counterEl);
  }

  // ==== Q5f: Network Shaping =======================================
  // Basic sliders + Pattern editor. Patches go to player.shape.
  // Pattern templates: sliders / square / ramp-up / ramp-down /
  // pyramid (drives the step list); step rows have preset / mbps /
  // duration / enabled.

  const PATTERN_TEMPLATES = [
    { v: 'sliders',     l: '🎚 Sliders' },
    { v: 'square',      l: '▁▔ Square' },
    { v: 'ramp_up',     l: '↗ Ramp Up' },
    { v: 'ramp_down',   l: '↘ Ramp Down' },
    { v: 'pyramid',     l: '⛰ Pyramid' },
  ];

  function renderNetworkShaping(content, player) {
    content.innerHTML = '';
    const s = player.shape;

    function row(label, child) {
      const r = document.createElement('div');
      r.className = 'fault-control-row';
      const lbl = document.createElement('label');
      lbl.appendChild(t(label));
      r.appendChild(lbl);
      r.appendChild(child);
      content.appendChild(r);
      return r;
    }

    row('Delay (ms)', sliderRow(s.delayMs || 0, 0, 250, 5, (v) => s.setDelay(v)));
    row('Loss (%)',   sliderRow(s.lossPct || 0, 0, 10,  0.5, (v) => s.setLoss(v)));
    const patternActive = s.pattern && s.pattern.steps && s.pattern.steps.length > 0;
    const rateRow = sliderRow(s.rateMbps || 0, 0, 50, 0.1, (v) => s.setRate(v));
    if (patternActive) rateRow.classList.add('range-row-disabled');
    row('Throughput (Mbps)', rateRow);

    // ---- Pattern editor ----
    const patternBlock = document.createElement('div');
    patternBlock.className = 'shape-pattern-block';
    content.appendChild(patternBlock);

    // Template mode radios
    const templateRow = document.createElement('div');
    templateRow.className = 'fault-control-row';
    const templateLbl = document.createElement('label');
    templateLbl.appendChild(t('Pattern Mode'));
    templateRow.appendChild(templateLbl);
    const templateGroup = document.createElement('div');
    templateGroup.className = 'radio-group';
    const curTemplate = (s.pattern && s.pattern.template) || 'sliders';
    const curStepSeconds = (s.pattern && s.pattern.default_step_seconds) || 6;
    const curMarginPct = (s.pattern && s.pattern.margin_pct) || 0;
    for (const tpl of PATTERN_TEMPLATES) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'shape_template';
      inp.value = tpl.v;
      inp.checked = curTemplate === tpl.v;
      inp.addEventListener('change', () => applyTemplate(player, tpl.v, curStepSeconds, curMarginPct));
      lbl.appendChild(inp);
      lbl.appendChild(t(' ' + tpl.l));
      templateGroup.appendChild(lbl);
    }
    templateRow.appendChild(templateGroup);
    patternBlock.appendChild(templateRow);

    // Step Duration radios — match v1 (6/12/18/24).
    const stepDurationRow = document.createElement('div');
    stepDurationRow.className = 'fault-control-row';
    const sdLbl = document.createElement('label');
    sdLbl.appendChild(t('Step Duration'));
    stepDurationRow.appendChild(sdLbl);
    const sdGroup = document.createElement('div');
    sdGroup.className = 'radio-group';
    for (const sec of [6, 12, 18, 24]) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'shape_step_seconds';
      inp.value = String(sec);
      inp.checked = curStepSeconds === sec;
      inp.addEventListener('change', () => {
        // Re-apply the template at the new default step duration.
        applyTemplate(player, curTemplate, sec, curMarginPct);
      });
      lbl.appendChild(inp);
      lbl.appendChild(t(' ' + sec + 's'));
      sdGroup.appendChild(lbl);
    }
    stepDurationRow.appendChild(sdGroup);
    patternBlock.appendChild(stepDurationRow);

    // Margin radios — Exact / +10% / +25% / +50%. Drives the cap rate
    // for square/ramp/pyramid templates (header room above the top
    // variant).
    const marginRow = document.createElement('div');
    marginRow.className = 'fault-control-row';
    const mLbl = document.createElement('label');
    mLbl.appendChild(t('Margin'));
    marginRow.appendChild(mLbl);
    const mGroup = document.createElement('div');
    mGroup.className = 'radio-group';
    for (const pct of [0, 10, 25, 50]) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'shape_margin';
      inp.value = String(pct);
      inp.checked = curMarginPct === pct;
      inp.addEventListener('change', () => {
        applyTemplate(player, curTemplate, curStepSeconds, pct);
      });
      lbl.appendChild(inp);
      lbl.appendChild(t(pct === 0 ? ' Exact' : ' +' + pct + '%'));
      mGroup.appendChild(lbl);
    }
    marginRow.appendChild(mGroup);
    patternBlock.appendChild(marginRow);

    if (curTemplate !== 'sliders') {
      // Step list with editable rows
      const stepListLabel = document.createElement('div');
      stepListLabel.style.cssText = 'font-size:11px;color:#6b7280;margin:6px 0 4px;font-weight:600;';
      stepListLabel.appendChild(t('Steps'));
      patternBlock.appendChild(stepListLabel);

      const stepList = document.createElement('div');
      stepList.className = 'shape-step-list';
      patternBlock.appendChild(stepList);
      const steps = (s.pattern && s.pattern.steps) || [];
      steps.forEach((step, i) => {
        stepList.appendChild(renderStepRow(player, i, step));
      });

      const actions = document.createElement('div');
      actions.className = 'shape-step-actions';
      const addBtn = document.createElement('button');
      addBtn.className = 'btn btn-secondary btn-mini';
      addBtn.appendChild(t('+ Add Step'));
      addBtn.addEventListener('click', () => {
        const next = steps.slice();
        next.push({ rate_mbps: 5, duration_seconds: 6, enabled: true });
        s.setPattern({ template: curTemplate, steps: next });
      });
      actions.appendChild(addBtn);
      const clearBtn = document.createElement('button');
      clearBtn.className = 'btn btn-secondary btn-mini';
      clearBtn.appendChild(t('Clear'));
      clearBtn.addEventListener('click', () => s.clearPattern());
      actions.appendChild(clearBtn);
      patternBlock.appendChild(actions);
    }
  }

  function renderStepRow(player, idx, step) {
    const row = document.createElement('div');
    row.className = 'shape-step-row';
    const idxLbl = document.createElement('label');
    idxLbl.appendChild(t('#' + (idx + 1)));
    row.appendChild(idxLbl);

    const mbpsInput = document.createElement('input');
    mbpsInput.type = 'number';
    mbpsInput.step = '0.1'; mbpsInput.min = '0';
    mbpsInput.value = String(step.rate_mbps);
    mbpsInput.addEventListener('change', () => {
      patchStep(player, idx, { rate_mbps: parseFloat(mbpsInput.value) });
    });
    row.appendChild(spanLabel('Mbps')); row.appendChild(mbpsInput);

    const dur = document.createElement('input');
    dur.type = 'number'; dur.step = '1'; dur.min = '1';
    dur.value = String(step.duration_seconds);
    dur.addEventListener('change', () => {
      patchStep(player, idx, { duration_seconds: parseInt(dur.value, 10) || 1 });
    });
    row.appendChild(spanLabel('s')); row.appendChild(dur);

    const en = document.createElement('label');
    en.className = 'shape-step-enabled';
    const enInp = document.createElement('input');
    enInp.type = 'checkbox';
    enInp.checked = step.enabled !== false;
    enInp.addEventListener('change', () => patchStep(player, idx, { enabled: enInp.checked }));
    en.appendChild(enInp);
    en.appendChild(t(' Enabled'));
    row.appendChild(en);
    return row;
  }
  function spanLabel(s) { const el = document.createElement('label'); el.appendChild(t(s)); return el; }
  function patchStep(player, idx, patch) {
    const cur = player.shape.pattern || {};
    const steps = (cur.steps || []).slice();
    if (!steps[idx]) return;
    steps[idx] = Object.assign({}, steps[idx], patch);
    player.shape.setPattern({ template: cur.template || 'sliders', steps });
  }
  function applyTemplate(player, template, stepSeconds, marginPct) {
    if (template === 'sliders') { player.shape.clearPattern(); return; }
    const dur = stepSeconds || 6;
    const steps = templateSteps(template, dur, marginPct || 0, player);
    player.shape.setPattern({
      template, steps,
      default_step_seconds: dur,
      margin_pct: marginPct || 0,
    });
  }
  function templateSteps(template, dur, marginPct, player) {
    // Cap pulled from the top manifest variant when available; +margin
    // adds headroom for a buffer test.
    const variants = player && player.currentPlay && player.currentPlay.manifest
      && player.currentPlay.manifest.variants || [];
    const topMbps = variants.length
      ? Math.max.apply(null, variants.map(v => (v.bandwidth || 0) / 1_000_000))
      : 12;
    const cap = topMbps * (1 + (marginPct || 0) / 100);
    const lo = Math.max(0.5, cap * 0.1);
    const mid = Math.max(1, cap * 0.5);
    if (template === 'square') return [
      { rate_mbps: lo, duration_seconds: dur, enabled: true },
      { rate_mbps: cap, duration_seconds: dur, enabled: true },
    ];
    if (template === 'ramp_up') return [
      { rate_mbps: lo, duration_seconds: dur, enabled: true },
      { rate_mbps: mid * 0.5, duration_seconds: dur, enabled: true },
      { rate_mbps: mid, duration_seconds: dur, enabled: true },
      { rate_mbps: cap, duration_seconds: dur, enabled: true },
    ];
    if (template === 'ramp_down') return [
      { rate_mbps: cap, duration_seconds: dur, enabled: true },
      { rate_mbps: mid, duration_seconds: dur, enabled: true },
      { rate_mbps: mid * 0.5, duration_seconds: dur, enabled: true },
      { rate_mbps: lo, duration_seconds: dur, enabled: true },
    ];
    if (template === 'pyramid') return [
      { rate_mbps: lo, duration_seconds: dur, enabled: true },
      { rate_mbps: mid, duration_seconds: dur, enabled: true },
      { rate_mbps: cap, duration_seconds: dur, enabled: true },
      { rate_mbps: mid, duration_seconds: dur, enabled: true },
      { rate_mbps: lo, duration_seconds: dur, enabled: true },
    ];
    return [];
  }

  // ==== Q5g: Charts (Bandwidth + Buffer + FPS + Events Timeline) ===
  // Mounts the v2-charts module against this Player. The Bitrate
  // Y-axis-max selector + reset/pause toolbar buttons are part of
  // the chart panel's chrome.

  function renderCharts(content, player) {
    content.innerHTML = '';

    // Toolbar
    const tools = document.createElement('div');
    tools.className = 'chart-axis-row';
    const yLbl = document.createElement('label');
    yLbl.appendChild(t('Bitrate Y-Max'));
    tools.appendChild(yLbl);
    const ymax = document.createElement('div');
    ymax.className = 'radio-group';
    for (const v of ['auto', '5', '10', '20', '30', '40', '50', '100']) {
      const lbl = document.createElement('label');
      const inp = document.createElement('input');
      inp.type = 'radio';
      inp.name = 'bitrate_ymax';
      inp.value = v;
      const cur = localStorage.getItem('ismBitrateYMax') || 'auto';
      if (v === cur) inp.checked = true;
      inp.addEventListener('change', () => {
        localStorage.setItem('ismBitrateYMax', v);
        // Future: pass to chart engine for dynamic axis. For Q5g we
        // restart all charts since y-axis change is rare.
        const repo = window.TestingSessionV2 && window.TestingSessionV2.repo;
        if (repo) renderCharts(content, player);
      });
      lbl.appendChild(inp);
      lbl.appendChild(t(' ' + (v === 'auto' ? 'Auto' : v)));
      ymax.appendChild(lbl);
    }
    tools.appendChild(ymax);
    content.appendChild(tools);

    // Chart canvases / containers
    const bw = chartWrap('bandwidth-chart');
    const buf = chartWrap('buffer-depth-chart');
    const fps = chartWrap('video-fps-chart');
    const events = document.createElement('div');
    events.className = 'events-chart';
    const eventsWrap = document.createElement('div');
    eventsWrap.className = 'chart-wrap events-chart-wrap';
    eventsWrap.appendChild(events);
    content.appendChild(bw.wrap);
    content.appendChild(buf.wrap);
    content.appendChild(fps.wrap);
    content.appendChild(eventsWrap);

    if (window.V2Charts) {
      const repo = (window.TestingSessionV2 && window.TestingSessionV2.repo)
        || (window.TestingV2 && window.TestingV2.repo);
      window.V2Charts.mountAll(player, {
        bandwidthCanvas: bw.canvas,
        bufferCanvas: buf.canvas,
        fpsCanvas: fps.canvas,
        repo,
      });
    }
  }

  // ==== Q5g (split): Player State / Events Timeline =================
  // The v1 page had its own "Player State" collapsible above the
  // bitrate chart: a vis-timeline strip with HTTP / Transport /
  // Lifecycle lanes plus a Reset-Zoom + Pause toolbar. Surface it as
  // its own fold so users can collapse the noisier chart panel
  // without losing the lifecycle ribbon.

  function renderPlayerState(content, player) {
    content.innerHTML = '';

    const tools = document.createElement('div');
    tools.className = 'chart-axis-row';
    tools.style.cssText = 'display:flex;gap:8px;align-items:center;margin-bottom:6px;';
    const reset = button('Reset Zoom', 'btn btn-secondary btn-mini');
    const pause = button('⏸ Pause', 'btn btn-secondary btn-mini');
    const hint = document.createElement('span');
    hint.className = 'chart-hint';
    hint.appendChild(t('Alt/⌥+scroll/drag to zoom · right-drag to pan'));
    tools.appendChild(reset);
    tools.appendChild(pause);
    tools.appendChild(hint);
    content.appendChild(tools);

    const timelineWrap = document.createElement('div');
    timelineWrap.className = 'chart-wrap events-chart-wrap';
    timelineWrap.style.cssText = 'overflow:hidden;max-height:240px;';
    const timeline = document.createElement('div');
    timeline.className = 'events-chart';
    timeline.style.cssText = 'width:100%;min-height:200px;';
    timelineWrap.appendChild(timeline);
    content.appendChild(timelineWrap);

    if (window.V2Charts && window.V2Charts.EventsTimeline) {
      const repo = (window.TestingSessionV2 && window.TestingSessionV2.repo)
        || (window.TestingV2 && window.TestingV2.repo);
      const tl = new window.V2Charts.EventsTimeline(timeline, player);

      // Poll the network log every 2s and feed faulted entries into
      // the timeline. Keeps the strip in sync with the network log
      // pane below; live events show up as soon as faults fire.
      let cancelled = false;
      let lastTs = 0;
      function poll() {
        if (cancelled || !repo) return;
        repo.networkLog(player.id, 200).then(r => {
          if (r && r.ok && r.body && r.body.items) {
            for (const raw of r.body.items) {
              const ts = Date.parse(raw.timestamp || '');
              if (!Number.isFinite(ts) || ts <= lastTs) continue;
              lastTs = ts;
              tl.push(new window.V2Models.NetworkLogEntry(raw));
            }
          }
        }).finally(() => {
          if (!cancelled) setTimeout(poll, 2000);
        });
      }
      poll();
      reset.addEventListener('click', () => tl.timeline.fit());
      let paused = false;
      pause.addEventListener('click', () => {
        paused = !paused;
        pause.textContent = paused ? '▶ Resume' : '⏸ Pause';
        tl.timeline.setOptions({ moveable: !paused });
      });
    }
  }
  function chartWrap(canvasClass) {
    const wrap = document.createElement('div');
    wrap.className = 'chart-wrap';
    const canvas = document.createElement('canvas');
    canvas.className = canvasClass;
    wrap.appendChild(canvas);
    return { wrap, canvas };
  }

  // ==== Q5h: Network Log waterfall =================================
  // Sortable table powered by V2Repo.networkLog polling. Three controls:
  //   • Pause/Live  — stops or resumes the 1.5s poll
  //   • Hide-Successful  — show only faulted rows
  //   • Sort cycle  — click a header to cycle asc/desc/default
  // Tooltip on hover shows phase breakdown (DNS/connect/TLS/wait/transfer)
  // when the entry has the timings, plus the fault chain.
  //
  // The optional brush+overview rail isn't ported in this first cut —
  // the table is the primary affordance, brush is a nice-to-have.

  const NETWORK_COLS = [
    { key: 'time',     label: 'Time',     sortable: true },
    { key: 'flags',    label: '',         sortable: false },
    { key: 'method',   label: 'Method',   sortable: true },
    { key: 'path',     label: 'Path',     sortable: true },
    { key: 'kind',     label: 'Kind',     sortable: true },
    { key: 'bytes',    label: 'Bytes',    sortable: true },
    { key: 'mbps',     label: 'Mbps',     sortable: true },
    { key: 'duration', label: 'Duration', sortable: true },
    { key: 'status',   label: 'Status',   sortable: true },
  ];

  function renderNetworkLog(content, player, ctx) {
    content.innerHTML = '';

    // Brush range state, kept on ctx so it survives across re-renders.
    if (!ctx.brush) ctx.brush = { startMs: 0, endMs: 0, follow: true };

    // ---- Toolbar ----
    const tools = document.createElement('div');
    tools.className = 'network-log-controls';
    tools.style.cssText = 'display:flex;gap:12px;align-items:center;flex-wrap:wrap;margin-bottom:8px;';
    const refresh = button('Refresh', 'btn btn-secondary btn-mini');
    const pause = button(ctx.paused ? '▶ Live' : '⏸ Pause', 'btn btn-secondary btn-mini');
    const followCheck = checkLabel('Follow Latest', ctx.brush.follow, () => {
      ctx.brush.follow = !ctx.brush.follow;
      ctx.lastEntries && rerender(ctx.lastEntries);
    });
    const hide = checkLabel('Hide Successful', ctx.hideSuccessful, () => {
      ctx.hideSuccessful = !ctx.hideSuccessful;
      ctx.lastEntries && rerender(ctx.lastEntries);
    });
    const badge = document.createElement('span');
    badge.style.cssText = 'font-size:11px;color:#6b7280;margin-left:auto;';
    refresh.addEventListener('click', () => doFetch(true));
    pause.addEventListener('click', () => {
      ctx.paused = !ctx.paused;
      pause.textContent = ctx.paused ? '▶ Live' : '⏸ Pause';
      if (!ctx.paused) doFetch();
    });
    tools.appendChild(refresh);
    tools.appendChild(pause);
    tools.appendChild(followCheck);
    tools.appendChild(hide);
    tools.appendChild(badge);
    content.appendChild(tools);

    // ---- Brush overview rail ----
    // Histogram of request density across the full time span. The brush
    // (semi-transparent overlay) selects a sub-range to filter the row
    // table. Drag body = pan, handles = resize, click on rail = jump-to.
    const overviewWrap = document.createElement('div');
    overviewWrap.className = 'netwf-overview';
    overviewWrap.style.cssText = 'position:relative;height:36px;background:#f9fafb;border:1px solid #e5e7eb;border-radius:4px;margin-bottom:6px;cursor:pointer;user-select:none;';
    const bars = document.createElement('div');
    bars.className = 'netwf-overview-bars';
    bars.style.cssText = 'position:absolute;inset:0;display:flex;align-items:flex-end;padding:2px 0;';
    overviewWrap.appendChild(bars);
    const brushEl = document.createElement('div');
    brushEl.className = 'netwf-brush';
    brushEl.style.cssText = 'position:absolute;top:0;bottom:0;background:rgba(59,130,246,0.18);border-left:2px solid #3b82f6;border-right:2px solid #3b82f6;cursor:grab;left:0;width:100%;';
    const leftH = document.createElement('div');
    leftH.className = 'netwf-brush-handle left';
    leftH.style.cssText = 'position:absolute;top:0;bottom:0;left:-3px;width:8px;cursor:ew-resize;background:#3b82f6;opacity:0.4;';
    const rightH = document.createElement('div');
    rightH.className = 'netwf-brush-handle right';
    rightH.style.cssText = 'position:absolute;top:0;bottom:0;right:-3px;width:8px;cursor:ew-resize;background:#3b82f6;opacity:0.4;';
    brushEl.appendChild(leftH);
    brushEl.appendChild(rightH);
    overviewWrap.appendChild(brushEl);
    content.appendChild(overviewWrap);

    function updateBrushUi(dataStart, dataEnd) {
      const span = Math.max(50, dataEnd - dataStart);
      const leftPct = ((ctx.brush.startMs - dataStart) / span) * 100;
      const widthPct = ((ctx.brush.endMs - ctx.brush.startMs) / span) * 100;
      brushEl.style.left = Math.max(0, leftPct) + '%';
      brushEl.style.width = Math.max(2, Math.min(100 - Math.max(0, leftPct), widthPct)) + '%';
    }

    function renderBars(entries) {
      bars.innerHTML = '';
      if (!entries.length) return;
      const dataStart = entries[0].timestamp ? Date.parse(entries[0].timestamp) : 0;
      const dataEnd = Math.max(...entries.map(e => {
        const t = e.timestamp ? Date.parse(e.timestamp) : 0;
        return t + Math.max(50, e.totalMs || 50);
      }));
      const span = Math.max(50, dataEnd - dataStart);
      // Bin requests into 60 buckets across the span.
      const N = 60;
      const counts = new Array(N).fill(0);
      let max = 1;
      entries.forEach(e => {
        const tms = e.timestamp ? Date.parse(e.timestamp) : 0;
        const idx = Math.floor(((tms - dataStart) / span) * N);
        const i = Math.max(0, Math.min(N - 1, idx));
        counts[i]++;
        if (counts[i] > max) max = counts[i];
      });
      for (let i = 0; i < N; i++) {
        const bar = document.createElement('div');
        const h = (counts[i] / max) * 100;
        bar.style.cssText = 'flex:1;height:' + h.toFixed(1) +
          '%;background:#3b82f6;opacity:0.55;margin:0 1px;';
        bars.appendChild(bar);
      }
      // If brush hasn't been set or follow mode is on, snap to full range.
      if (ctx.brush.follow || ctx.brush.endMs === 0 ||
          ctx.brush.endMs > dataEnd || ctx.brush.startMs < dataStart) {
        ctx.brush.startMs = dataStart;
        ctx.brush.endMs = dataEnd;
      }
      updateBrushUi(dataStart, dataEnd);
    }
    ctx.renderBars = renderBars;

    // Brush drag handlers
    let drag = null;
    function startDrag(ev, mode) {
      const rect = overviewWrap.getBoundingClientRect();
      if (rect.width <= 0 || !ctx.lastEntries.length) return;
      const dataStart = Date.parse(ctx.lastEntries[0].timestamp);
      const dataEnd = Math.max(...ctx.lastEntries.map(e => {
        const t = e.timestamp ? Date.parse(e.timestamp) : 0;
        return t + Math.max(50, e.totalMs || 50);
      }));
      drag = {
        mode, rect, dataStart, dataEnd,
        span: Math.max(50, dataEnd - dataStart),
        startBrushStart: ctx.brush.startMs,
        startBrushEnd: ctx.brush.endMs,
        pointerStartX: ev.clientX,
      };
      brushEl.style.cursor = mode === 'pan' ? 'grabbing' : 'ew-resize';
      ctx.brush.follow = false;
      followCheck.querySelector('input').checked = false;
      ev.preventDefault(); ev.stopPropagation();
    }
    function onMove(ev) {
      if (!drag) return;
      const dxPx = ev.clientX - drag.pointerStartX;
      const dxMs = (dxPx / drag.rect.width) * drag.span;
      const minWidthMs = Math.max(100, drag.span * 0.02);
      if (drag.mode === 'pan') {
        let s = drag.startBrushStart + dxMs;
        let e = drag.startBrushEnd + dxMs;
        const w = e - s;
        if (s < drag.dataStart) { s = drag.dataStart; e = s + w; }
        if (e > drag.dataEnd) { e = drag.dataEnd; s = e - w; }
        ctx.brush.startMs = s; ctx.brush.endMs = e;
      } else if (drag.mode === 'resize-left') {
        let s = drag.startBrushStart + dxMs;
        if (s < drag.dataStart) s = drag.dataStart;
        if (s > ctx.brush.endMs - minWidthMs) s = ctx.brush.endMs - minWidthMs;
        ctx.brush.startMs = s;
      } else if (drag.mode === 'resize-right') {
        let e = drag.startBrushEnd + dxMs;
        if (e > drag.dataEnd) e = drag.dataEnd;
        if (e < ctx.brush.startMs + minWidthMs) e = ctx.brush.startMs + minWidthMs;
        ctx.brush.endMs = e;
      }
      updateBrushUi(drag.dataStart, drag.dataEnd);
      // Live-sync sibling charts via the v1-compatible custom event.
      document.dispatchEvent(new CustomEvent('replay:brush-range-change', {
        detail: { sessionId: player.id, startMs: ctx.brush.startMs, endMs: ctx.brush.endMs, source: 'network-log', live: true },
      }));
    }
    function onUp() {
      if (!drag) return;
      drag = null;
      brushEl.style.cursor = 'grab';
      ctx.lastEntries && rerender(ctx.lastEntries);
      document.dispatchEvent(new CustomEvent('replay:brush-range-change', {
        detail: { sessionId: player.id, startMs: ctx.brush.startMs, endMs: ctx.brush.endMs, source: 'network-log', live: false },
      }));
    }
    brushEl.addEventListener('mousedown', (ev) => {
      if (ev.target.classList.contains('netwf-brush-handle')) return;
      startDrag(ev, 'pan');
    });
    leftH.addEventListener('mousedown', (ev) => startDrag(ev, 'resize-left'));
    rightH.addEventListener('mousedown', (ev) => startDrag(ev, 'resize-right'));
    overviewWrap.addEventListener('mousedown', (ev) => {
      if (ev.target !== overviewWrap && !ev.target.classList.contains('netwf-overview-bars')) return;
      // Click on bare rail jumps brush centre to click point.
      if (!ctx.lastEntries.length) return;
      const rect = overviewWrap.getBoundingClientRect();
      const dataStart = Date.parse(ctx.lastEntries[0].timestamp);
      const dataEnd = Math.max(...ctx.lastEntries.map(e => {
        const t = e.timestamp ? Date.parse(e.timestamp) : 0;
        return t + Math.max(50, e.totalMs || 50);
      }));
      const span = Math.max(50, dataEnd - dataStart);
      const targetMs = dataStart + ((ev.clientX - rect.left) / rect.width) * span;
      const w = ctx.brush.endMs - ctx.brush.startMs;
      let s = Math.max(dataStart, targetMs - w / 2);
      let e = Math.min(dataEnd, s + w);
      s = e - w;
      ctx.brush.startMs = s; ctx.brush.endMs = e; ctx.brush.follow = false;
      followCheck.querySelector('input').checked = false;
      updateBrushUi(dataStart, dataEnd);
      ctx.lastEntries && rerender(ctx.lastEntries);
    });
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
    ctx.detachBrush = () => {
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    };

    // ---- Table host ----
    const tableWrap = document.createElement('div');
    tableWrap.style.cssText = 'max-height:480px;overflow:auto;border:1px solid #e5e7eb;border-radius:6px;';
    const table = document.createElement('table');
    table.style.cssText = 'width:100%;border-collapse:collapse;font-size:11px;';
    const thead = document.createElement('thead');
    const tbody = document.createElement('tbody');
    const headRow = document.createElement('tr');
    NETWORK_COLS.forEach(col => {
      const th = document.createElement('th');
      th.style.cssText = 'background:#f5f5f5;padding:6px 8px;text-align:left;font-weight:600;border-bottom:2px solid #ddd;white-space:nowrap;cursor:' + (col.sortable ? 'pointer' : 'default') + ';position:sticky;top:0;';
      th.appendChild(t(col.label));
      if (col.sortable) {
        th.addEventListener('click', () => {
          if (ctx.sortKey !== col.key) { ctx.sortKey = col.key; ctx.sortDir = 'asc'; }
          else if (ctx.sortDir === 'asc') ctx.sortDir = 'desc';
          else { ctx.sortKey = null; ctx.sortDir = null; }
          ctx.lastEntries && rerender(ctx.lastEntries);
        });
        if (ctx.sortKey === col.key) {
          const arrow = document.createElement('span');
          arrow.style.cssText = 'margin-left:4px;color:#3b82f6;';
          arrow.textContent = ctx.sortDir === 'asc' ? '▲' : '▼';
          th.appendChild(arrow);
        }
      }
      headRow.appendChild(th);
    });
    thead.appendChild(headRow);
    table.appendChild(thead);
    table.appendChild(tbody);
    tableWrap.appendChild(table);
    content.appendChild(tableWrap);

    function rerender(entries) {
      tbody.innerHTML = '';
      let visible = entries.slice();
      // Default time window: last 10 minutes. The ring buffer can hold
      // ~hours of history when traffic is light; without a default
      // window the user sees entries from this morning at the top.
      // Brush, when set, overrides this.
      if (ctx.brush && ctx.brush.endMs > ctx.brush.startMs) {
        visible = visible.filter(e => {
          const t = e.timestamp ? Date.parse(e.timestamp) : 0;
          return t >= ctx.brush.startMs && t <= ctx.brush.endMs;
        });
      } else {
        const cutoff = Date.now() - 10 * 60 * 1000;
        visible = visible.filter(e => {
          const t = e.timestamp ? Date.parse(e.timestamp) : 0;
          return t >= cutoff;
        });
      }
      if (ctx.hideSuccessful) {
        visible = visible.filter(e => e.faulted || (e.status >= 400));
      }
      // Default sort: newest first so the most-recent activity is at
      // the top of the table on page load. Explicit user clicks via
      // the column headers override.
      if (ctx.sortKey) {
        visible.sort(rowComparator(ctx.sortKey, ctx.sortDir));
      } else {
        visible.sort(rowComparator('time', 'desc'));
      }
      badge.textContent = visible.length + ' / ' + entries.length + ' requests';
      // Render cap matches the v1 page: full ring buffer (5000) so the
      // 10-minute time window the user expects is actually visible.
      visible.slice(0, 5000).forEach(entry => tbody.appendChild(renderRow(entry)));
    }
    ctx.rerender = rerender;

    // ---- Polling loop ----
    function doFetch(force) {
      if (ctx.paused && !force) return;
      const repo = window.TestingSessionV2 && window.TestingSessionV2.repo;
      if (!repo) return;
      // Pull the full ring (5000) so the table window matches v1 — at
      // ~30 req/s during steady playback, 5000 covers the last ~3 min
      // of LL-HLS or ~10 min if the player isn't pulling partials.
      repo.networkLog(player.id, 5000).then(r => {
        if (r && r.ok && r.body && r.body.items) {
          const entries = r.body.items.map(raw => new window.V2Models.NetworkLogEntry(raw));
          ctx.lastEntries = entries;
          ctx.renderBars && ctx.renderBars(entries);
          rerender(entries);
        }
      }).finally(() => {
        if (!ctx.cancelled) ctx.timer = setTimeout(doFetch, 1500);
      });
    }
    if (ctx.timer) clearTimeout(ctx.timer);
    doFetch(true);
  }

  function rowComparator(key, dir) {
    const sign = dir === 'asc' ? 1 : -1;
    const get = (e) => {
      switch (key) {
        case 'time':     return e.timestamp ? Date.parse(e.timestamp) : 0;
        case 'method':   return e.method || '';
        case 'path':     return e.path || e.url || '';
        case 'kind':     return e.requestKind || '';
        case 'bytes':    return e.bytesIn || 0;
        case 'mbps':     return e.totalMs && e.bytesIn ? (e.bytesIn * 8) / (e.totalMs * 1000) : 0;
        case 'duration': return e.totalMs || 0;
        case 'status':   return e.status || 0;
      }
      return 0;
    };
    return (a, b) => {
      const av = get(a), bv = get(b);
      if (av < bv) return -1 * sign;
      if (av > bv) return 1 * sign;
      return 0;
    };
  }

  function renderRow(entry) {
    const tr = document.createElement('tr');
    tr.style.cssText = 'border-bottom:1px solid #f3f4f6;';
    if (entry.faulted) tr.style.background = '#fef2f2';

    // Time — render in the viewer's LOCAL timezone so the column is
    // immediately readable. The previous implementation used UTC
    // (toISOString().substring(11,23)), which silently presents a
    // 7-hour offset on the West Coast and masks "is this entry
    // recent?" reasoning.
    const timeStr = entry.timestamp
      ? (function () {
          try {
            const d = new Date(entry.timestamp);
            const hh = String(d.getHours()).padStart(2, '0');
            const mm = String(d.getMinutes()).padStart(2, '0');
            const ss = String(d.getSeconds()).padStart(2, '0');
            const ms = String(d.getMilliseconds()).padStart(3, '0');
            return hh + ':' + mm + ':' + ss + '.' + ms;
          } catch (_) { return '—'; }
        })()
      : '—';
    tr.appendChild(td(timeStr));

    // Flags
    const flags = [];
    if (entry.faulted) flags.push('!');
    const flagsCell = td(flags.join(' '));
    flagsCell.style.color = '#dc2626';
    flagsCell.style.fontWeight = '600';
    tr.appendChild(flagsCell);

    // Method
    tr.appendChild(td(entry.method || '—'));

    // Path
    const pathCell = td(entry.path || entry.url || '—');
    pathCell.style.cssText = 'font-family:monospace;max-width:350px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;';
    pathCell.title = (entry.method || '') + ' ' + (entry.url || entry.path || '');
    tr.appendChild(pathCell);

    // Kind
    tr.appendChild(td(entry.requestKind || '—'));

    // Bytes
    tr.appendChild(td(entry.bytesIn != null ? (entry.bytesIn / 1024).toFixed(1) + 'k' : '—'));

    // Mbps
    const mbps = (entry.totalMs && entry.bytesIn)
      ? ((entry.bytesIn * 8) / (entry.totalMs * 1000)).toFixed(2)
      : '—';
    tr.appendChild(td(mbps));

    // Duration
    tr.appendChild(td(entry.totalMs != null ? entry.totalMs.toFixed(0) + 'ms' : '—'));

    // Status
    const statusCell = td(String(entry.status || '—'));
    statusCell.style.fontWeight = '600';
    if (entry.status >= 500) statusCell.style.color = '#dc2626';
    else if (entry.status >= 400) statusCell.style.color = '#d97706';
    else if (entry.status >= 300) statusCell.style.color = '#1e40af';
    else if (entry.status >= 200) statusCell.style.color = '#059669';
    tr.appendChild(statusCell);

    // Hover tooltip with phase-breakdown stacked bar.
    tr.addEventListener('mouseenter', (ev) => showRowTooltip(ev, entry));
    tr.addEventListener('mouseleave', hideRowTooltip);
    tr.addEventListener('mousemove', (ev) => positionRowTooltip(ev));

    return tr;
  }

  // ---- Row tooltip with phase-breakdown stacked bar ----------------
  let _tip = null;
  function ensureTipNode() {
    if (_tip) return _tip;
    _tip = document.createElement('div');
    _tip.className = 'netwf-tooltip';
    _tip.style.cssText = 'position:fixed;z-index:10000;background:#0f172a;color:#e5e7eb;' +
      'padding:8px 10px;border-radius:6px;font-size:11px;font-family:monospace;' +
      'box-shadow:0 8px 20px rgba(0,0,0,0.35);pointer-events:none;max-width:520px;display:none;';
    document.body.appendChild(_tip);
    return _tip;
  }
  function hideRowTooltip() { if (_tip) _tip.style.display = 'none'; }
  function positionRowTooltip(ev) {
    if (!_tip || _tip.style.display === 'none') return;
    const margin = 14;
    let x = ev.clientX + margin;
    let y = ev.clientY + margin;
    const r = _tip.getBoundingClientRect();
    if (x + r.width > window.innerWidth) x = ev.clientX - r.width - margin;
    if (y + r.height > window.innerHeight) y = ev.clientY - r.height - margin;
    _tip.style.left = x + 'px';
    _tip.style.top = y + 'px';
  }
  function showRowTooltip(ev, entry) {
    const tip = ensureTipNode();
    tip.innerHTML = '';

    // Header line
    const head = document.createElement('div');
    head.style.cssText = 'font-weight:600;margin-bottom:4px;color:#fff;';
    head.appendChild(t((entry.method || '?') + ' ' + (entry.path || entry.url || '')));
    tip.appendChild(head);

    // Detail lines
    const detail = document.createElement('div');
    detail.style.cssText = 'opacity:0.85;margin-bottom:6px;line-height:1.5;';
    const detailLines = [
      'Status: ' + (entry.status || '—') + '   Kind: ' + (entry.requestKind || '—'),
      'Bytes: ' + (entry.bytesIn != null ? (entry.bytesIn / 1024).toFixed(1) + 'k' : '—') +
        '   Total: ' + (entry.totalMs != null ? entry.totalMs.toFixed(0) + 'ms' : '—'),
      'TTFB: ' + (entry.ttfbMs != null ? entry.ttfbMs.toFixed(0) + 'ms' : '—') +
        '   Wait: ' + (entry.clientWaitMs != null ? entry.clientWaitMs.toFixed(0) + 'ms' : '—'),
    ];
    detailLines.forEach(line => {
      const d = document.createElement('div');
      d.appendChild(t(line));
      detail.appendChild(d);
    });
    tip.appendChild(detail);

    // Stacked phase-breakdown bar
    const phases = [
      { label: 'DNS',      ms: entry.dnsMs,      color: '#8b5cf6' },
      { label: 'Connect',  ms: entry.connectMs,  color: '#f59e0b' },
      { label: 'TLS',      ms: entry.tlsMs,      color: '#10b981' },
      { label: 'Wait',     ms: entry.clientWaitMs, color: '#3b82f6' },
      { label: 'Transfer', ms: entry.transferMs, color: '#6366f1' },
    ].filter(p => p.ms != null && p.ms > 0);
    const totalPhase = phases.reduce((sum, p) => sum + p.ms, 0);
    if (totalPhase > 0) {
      const bar = document.createElement('div');
      bar.style.cssText = 'display:flex;height:18px;background:#334155;border-radius:3px;overflow:hidden;margin-bottom:4px;';
      phases.forEach(p => {
        const seg = document.createElement('div');
        seg.title = p.label + ': ' + p.ms.toFixed(0) + 'ms';
        seg.style.cssText = 'background:' + p.color + ';flex-basis:' +
          ((p.ms / totalPhase) * 100).toFixed(1) + '%;display:flex;align-items:center;' +
          'justify-content:center;color:#fff;font-size:9px;font-weight:600;';
        if (p.ms / totalPhase > 0.08) seg.appendChild(t(p.label));
        bar.appendChild(seg);
      });
      tip.appendChild(bar);

      // Phase legend with numeric values
      const legend = document.createElement('div');
      legend.style.cssText = 'opacity:0.75;font-size:10px;';
      legend.appendChild(t(phases.map(p => p.label + ' ' + p.ms.toFixed(0) + 'ms').join('  ·  ')));
      tip.appendChild(legend);
    }

    // Fault chain
    if (entry.faulted) {
      const f = document.createElement('div');
      f.style.cssText = 'margin-top:6px;color:#fca5a5;border-top:1px solid #334155;padding-top:4px;';
      f.appendChild(t('FAULT — type: ' + (entry.faultType || '?') +
        '  action: ' + (entry.faultAction || '?') +
        '  category: ' + (entry.faultCategory || '?')));
      tip.appendChild(f);
    }

    tip.style.display = 'block';
    positionRowTooltip(ev);
  }
  function td(text) {
    const el = document.createElement('td');
    el.style.cssText = 'padding:4px 8px;vertical-align:middle;';
    el.appendChild(t(text));
    return el;
  }
  function button(label, cls) {
    const b = document.createElement('button');
    b.className = cls || 'btn';
    b.appendChild(t(label));
    return b;
  }
  function checkLabel(label, checked, onChange) {
    const wrap = document.createElement('label');
    wrap.style.cssText = 'font-size:11px;display:flex;align-items:center;gap:4px;cursor:pointer;';
    const inp = document.createElement('input');
    inp.type = 'checkbox';
    inp.checked = !!checked;
    inp.addEventListener('change', () => onChange(inp.checked));
    wrap.appendChild(inp);
    wrap.appendChild(t(label));
    return wrap;
  }

  // ==== Q5j: Group banner + controls ===============================
  // Surfaces group affiliation + add/remove member + disband.

  function renderGroup(content, player, groups) {
    content.innerHTML = '';
    const myGroup = groups.list().find(g => g.memberPlayerIds.includes(player.id));
    if (!myGroup) {
      const note = document.createElement('div');
      note.style.cssText = 'font-size:12px;color:#6b7280;padding:6px 8px;';
      note.appendChild(t('Not a member of any group. Use Testing Monitor to link players.'));
      content.appendChild(note);
      return;
    }
    const banner = document.createElement('div');
    banner.className = 'group-banner';
    const meta = document.createElement('div');
    meta.className = 'group-meta';
    meta.appendChild(t('🔗 ' + (myGroup.label || myGroup.id) +
      ' — ' + myGroup.memberPlayerIds.length + ' members'));
    banner.appendChild(meta);
    const actions = document.createElement('div');
    actions.className = 'group-actions';
    const leaveBtn = button('Leave', 'btn btn-secondary btn-mini');
    leaveBtn.addEventListener('click', () => myGroup.removeMember(player.id));
    const disbandBtn = button('Disband', 'btn btn-secondary btn-mini');
    disbandBtn.addEventListener('click', () => {
      if (confirm('Disband group?')) myGroup.disband();
    });
    actions.appendChild(leaveBtn);
    actions.appendChild(disbandBtn);
    banner.appendChild(actions);
    content.appendChild(banner);
  }

  // ---- mountAll ----------------------------------------------------
  // Wires every section into the host node and re-renders on every
  // Player change event. Returns a teardown function so a future
  // session swap can clean up.

  function mountAll(host, player, opts) {
    if (!host) return () => {};
    opts = opts || {};
    host.innerHTML = '';

    const sd = collapsible(host, 'session-details', 'Session Details', true);
    const pm = collapsible(host, 'player-metrics', 'Player Metrics', false);
    const fi = collapsible(host, 'fault-injection', 'Fault Injection', false);
    const st = collapsible(host, 'server-timeouts', 'Server Timeouts', false);
    const ns = collapsible(host, 'network-shaping', 'Network Shaping', false);
    const ps = collapsible(host, 'player-state', 'Player State', true);
    const ch = collapsible(host, 'charts', 'Live Charts', true);
    const nl = collapsible(host, 'network-log', 'Network Log', false);
    const gp = collapsible(host, 'group', 'Group', false);

    // Charts are heavy + Chart.js doesn't appreciate getting torn down
    // on every Player change. Mount them once and skip the re-render.
    let chartsMounted = false;
    let playerStateMounted = false;
    let networkLogMounted = false;
    const networkCtx = { paused: false, hideSuccessful: false, sortKey: null, sortDir: null, lastEntries: [] };

    const renderAll = () => {
      try { renderSessionDetails(sd.content, player); } catch (e) { console.error(e); }
      try { renderPlayerMetrics(pm.content, player); } catch (e) { console.error(e); }
      try { renderFaultInjection(fi.content, player); } catch (e) { console.error(e); }
      try { renderServerTimeouts(st.content, player); } catch (e) { console.error(e); }
      try { renderNetworkShaping(ns.content, player); } catch (e) { console.error(e); }
      if (opts.groups) {
        try { renderGroup(gp.content, player, opts.groups); } catch (e) { console.error(e); }
      }
      if (!playerStateMounted) {
        try { renderPlayerState(ps.content, player); playerStateMounted = true; }
        catch (e) { console.error(e); }
      }
      if (!chartsMounted) {
        try { renderCharts(ch.content, player); chartsMounted = true; }
        catch (e) { console.error(e); }
      }
      if (!networkLogMounted) {
        try { renderNetworkLog(nl.content, player, networkCtx); networkLogMounted = true; }
        catch (e) { console.error(e); }
      }
    };
    renderAll();

    const off = player.on('change', renderAll);
    const offGroup = opts.groups ? opts.groups.on('change', renderAll) : null;
    return () => {
      try { off(); } catch (_) {}
      try { offGroup && offGroup(); } catch (_) {}
      networkCtx.cancelled = true;
      if (networkCtx.timer) clearTimeout(networkCtx.timer);
      host.innerHTML = '';
    };
  }

  window.TestingSessionSections = { mountAll };
})();
