// Drive testing.html through a Valley shaping pattern against a real iOS
// device, and write down what actually happened.
//
// This is the app-specific half of the demo tooling. The generic half
// (narration, captions, fast-forward, export) is vendored from the Encoder
// project unchanged — see README.md.
//
// TWO RECORDINGS, not one. Playwright records the browser to a webm; the
// iPhone's screen is captured separately through QuickTime, because the phone's
// screen is not an AVFoundation device ffmpeg can open (avfoundation lists only
// "Jonathans iPhone Camera", which is Continuity Camera — the wrong thing).
// render_layout.py composites the two afterwards using the `layout` track this
// recorder emits.
//
// Consequences of that split, both deliberate:
//   - NO caption strip is drawn in the page. In the Encoder demo the captions
//     were injected into the DOM, which was right when the page WAS the video.
//     Here the browser is a sub-rectangle of the final frame, so a caption
//     burned into it would shrink and shift with the layout. Captions are data
//     only; render_layout.py reserves the strip on the composite.
//   - The cursor and spotlight DO stay in the page. They point at browser
//     elements, so they belong in the browser's own frame.
//
// Nothing here is on the serving path. It drives the dashboard over the DOM and
// reads the same v2 API the dashboard reads, exactly as an operator would.
// playwright is required lazily, at the point the browser is actually launched.
// Preflight (DRY=1) only talks to the v2 API, and having it fail on a missing
// npm dependency would defeat the purpose of a cheap pre-take check.
const { execFileSync, spawnSync } = require('child_process');
const fs = require('fs'), os = require('os'), path = require('path');

const BASE = process.env.BASE || 'https://dev.jeoliver.com:21000';
const PAGE_URL = `${BASE}/dashboard/testing.html`;

// Every artifact goes to the WORK dir, never next to the script — a take writes
// a webm, a mov, a cues.json and (once narrated) a few hundred wav clips, none
// of which belong in a checkout.
const WORK = process.env.DEMO_DIR || path.join(os.homedir(), 'Desktop', 'smashing-demo');
const OUT = path.join(WORK, process.env.OUTDIR || 'valley1');

// Pattern configuration, sized against the live 12-rung ladder: fill density
// None gives 24 ladder caps -> a 47-step valley, so 6s steps is a ~4.7 minute
// take with little to fast-forward.
//
// Note what 6s buys and costs. The device's buffer runs ~22s, so a 6s step is
// well under it: the cap moves roughly four times before any one change reaches
// the screen, and the DISPLAYED variant trails the cap continuously instead of
// settling between steps. The fetched variant still tracks each step promptly,
// so the fetched-vs-displayed split is if anything more visible — it just reads
// as a persistent offset rather than a sequence of discrete catch-ups.
// Raise STEP_SECONDS above the buffer depth if you want the settled version.
const FILL = process.env.FILL || 'none';          // none | 1.375 | 1.25 | 1.125
const STEP_SECONDS = Number(process.env.STEP_SECONDS || 6);
const MARGIN = Number(process.env.MARGIN || 5);
// How many steps the pattern should have once configured. The recorder ASSERTS
// this before applying: a wrong count means the controls did not take, and a
// take that records the wrong pattern is worse than no take. Set to 0 to skip
// (e.g. a deliberately short rehearsal at a different fill density).
const EXPECT_STEPS = Number(process.env.EXPECT_STEPS || 0);

// Seconds to observe at the top of the ladder before applying, so the video has
// a settled "before" to cut back to.
const SETTLE_S = Number(process.env.SETTLE_S || 25);
// Poll cadence against the v2 API. The device posts metrics on its own
// heartbeat; 1s is comfortably finer than that without hammering the box.
const POLL_MS = Number(process.env.POLL_MS || 1000);

// Browser recording size. Chosen for the composite: the phone is very tall and
// narrow (1179x2553 ~ 0.46), so side-by-side gives it a full-height column and
// leaves ~1480x950 for the browser — a 1.56 aspect, which 1600x1000 fills
// without letterboxing.
const W = Number(process.env.W || 1600);
const H = Number(process.env.H || 1000);

const PHONE = process.env.PHONE !== '0';          // drive QuickTime
const PLAYER = process.env.PLAYER || '';          // pin a player_id
// DRY=1 runs the preflight and stops. Nothing is recorded, no pattern is
// applied, the phone is left alone. Worth running before every real take —
// it is the cheap version of finding out the device went idle.
const DRY = process.env.DRY === '1';
// CAPTIONS=1 draws the narration in the page. Rehearsals only — see __capInit.
const CAPTIONS = process.env.CAPTIONS === '1';
const USER = process.env.DEMO_USER || '';
const PASS = process.env.DEMO_PASS || '';

// A pattern this long has a lot of dead air. The recorder records everything;
// the FFWD ranges in narrator_app.py compress the plateaus afterwards.
const RUN_TIMEOUT_MS = Number(process.env.RUN_TIMEOUT_MS || 45 * 60 * 1000);

// Whole valley cycles to record. The proxy loops the step list forever, so the
// take ends on a cycle boundary rather than a stopwatch — stopping mid-descent
// is the one ending the video cannot have. At 47 steps × 6s a cycle is ~4.7
// minutes, so 2 cycles is ~9.5 minutes of pattern plus the intro.
const CYCLES = Number(process.env.CYCLES || 2);

// How long to wait, on camera, for someone to start playback on the phone.
const WAIT_PLAY_MS = Number(process.env.WAIT_PLAY_MS || 3 * 60 * 1000);

// APPIUM=1 drives the phone instead of waiting for a hand: the app is brought
// to its home screen before the camera rolls, and playback is started on cue
// once the empty-panel narration has run. Makes the take reproducible.
// Unset, the recorder waits for someone to tap play (which is fine, and is what
// every rehearsal used).
const APPIUM = process.env.APPIUM === '1';
const CONTENT = process.env.CONTENT || 'fpv5_p200_h264_6s';
const CHAR_DIR = process.env.CHAR_DIR
  || path.join(__dirname, '..', '..', 'tests', 'characterization');

// Which panels are unfolded for the take. Everything else in FOLD_KEYS is
// folded, so the frame carries the two things the demo is about and nothing
// else — Fault Injection in particular defaults to OPEN and is pure noise here.
//
// Done by seeding localStorage rather than clicking, because the page's own
// `?open_folds=` deep-link can only force a panel OPEN, never closed. The
// storage scheme is CollapsibleSection.vue's: testing_session_collapse_<key>.
const FOLD_KEYS = [
  'session-details', 'fault-injection', 'content-manipulation', 'server-timeouts',
  'network-shaping', 'focus-window', 'player-metrics', 'player-state',
  'bitrate-chart', 'network-log', 'play-log',
];
const FOLDS_OPEN = (process.env.FOLDS_OPEN
  || 'network-shaping,bitrate-chart,player-state').split(',').map((s) => s.trim());

// Chart legend groups switched off before the take. "Variant Bands" is the
// twelve shaded avg→peak rung bands, which BandwidthChart shows by default.
// Empty string keeps everything.
const HIDE_LEGENDS = (process.env.HIDE_LEGENDS === undefined
  ? 'Variant Bands' : process.env.HIDE_LEGENDS).split(',').map((s) => s.trim()).filter(Boolean);

// Charts to render at double height (200px → 540px). Each MetricsLineChart owns
// its own Expand toggle, persisted under dashboard_v3_chart_expand_<title>, so
// this is seeded the same way as the folds. The panel-level ⤢ in
// BitrateChartPanelToolbar is a DIFFERENT control and does not change height —
// clicking it looked right and grew nothing.
const EXPAND_CHARTS = (process.env.EXPAND_CHARTS === undefined
  ? 'bandwidth' : process.env.EXPAND_CHARTS).split(',').map((s) => s.trim()).filter(Boolean);

/* ─── page-side overlay ─────────────────────────────────────────────────
 * Cursor, click ring and spotlight box. Same shapes as the Encoder recorder
 * minus the caption strip (see the header note). */
const INIT = `
window.__ui = () => {
  if (document.getElementById('__pwcursor')) return;
  const mk = (id, css) => { const d = document.createElement('div'); d.id = id; d.style.cssText = css; document.body.appendChild(d); return d; };
  mk('__pwcursor', 'position:fixed;left:0;top:0;width:22px;height:22px;z-index:2147483647;pointer-events:none;transition:transform 420ms cubic-bezier(.4,.1,.2,1);transform:translate(60px,60px)')
    .innerHTML = '<svg width="22" height="22" viewBox="0 0 22 22"><path d="M2 2 L2 16 L6 12.5 L8.6 18.4 L11.4 17.2 L8.8 11.4 L14 11.2 Z" fill="#fff" stroke="#111" stroke-width="1.3" stroke-linejoin="round"/></svg>';
  mk('__pwring', 'position:fixed;left:0;top:0;width:34px;height:34px;margin:-6px 0 0 -6px;border:2px solid #4cc9f0;border-radius:50%;opacity:0;z-index:2147483646;pointer-events:none;transition:transform 420ms cubic-bezier(.4,.1,.2,1),opacity 300ms');
  mk('__pwbox', 'position:fixed;border:2px solid #4cc9f0;border-radius:6px;box-shadow:0 0 0 9999px rgba(3,7,15,.45),0 0 18px rgba(76,201,240,.6);opacity:0;z-index:2147483644;pointer-events:none;transition:all 420ms cubic-bezier(.4,.1,.2,1)');
};
/* Rehearsal-only caption strip (CAPTIONS=1).
 *
 * A real take draws NO caption in the page: the browser ends up as a
 * sub-rectangle of the composite, so text burned in here would shrink and shift
 * with the layout. render_layout.py reserves a strip on the composite instead
 * and make_ass.py burns into that.
 *
 * But a rehearsal exists to check the NARRATION against the ACTION, and the raw
 * webm carries no captions at all — which makes the one thing a rehearsal is
 * for impossible to judge. So the strip is available, off by default, and
 * should stay off for anything being composited. */
window.__capInit = () => {
  if (document.getElementById('__pwcap')) return;
  document.body.style.paddingBottom = '130px';
  const d = document.createElement('div');
  d.id = '__pwcap';
  d.style.cssText = 'position:fixed;left:0;right:0;bottom:0;height:130px;'
    + 'z-index:2147483645;display:flex;align-items:center;padding:0 32px;'
    + 'pointer-events:none;background:linear-gradient(180deg,rgba(8,14,26,0),rgba(8,14,26,.97) 32%);'
    + 'color:#e6edf7;font:500 19px/1.5 -apple-system,BlinkMacSystemFont,Segoe UI,sans-serif;'
    + 'opacity:0;transition:opacity 250ms';
  document.body.appendChild(d);
};
window.__say = (text) => {
  const b = document.getElementById('__pwcap');
  if (!b) return;
  b.textContent = text;
  b.style.opacity = text ? '1' : '0';
};

window.__moveCursor = (x, y) => { window.__ui();
  const t = 'translate(' + x + 'px,' + y + 'px)';
  document.getElementById('__pwcursor').style.transform = t;
  document.getElementById('__pwring').style.transform = t; };
window.__clickPulse = () => { const r = document.getElementById('__pwring'); if (!r) return;
  r.style.opacity = '1'; setTimeout(() => { r.style.opacity = '0'; }, 380); };
window.__spot = (rect) => { window.__ui(); const b = document.getElementById('__pwbox');
  if (!rect) { b.style.opacity = '0'; return; }
  b.style.left = (rect.x - 6) + 'px'; b.style.top = (rect.y - 6) + 'px';
  b.style.width = (rect.w + 12) + 'px'; b.style.height = (rect.h + 12) + 'px';
  b.style.opacity = '1'; };
// These take (selector, index) rather than a single string. Playwright's
// "sel >> nth=3" is LOCATOR syntax and is not a valid CSS selector, so passing
// it through to querySelector throws — the index has to travel separately.
window.__rectOf = (sel, i) => {
  const e = document.querySelectorAll(sel)[i || 0]; if (!e) return null;
  const r = e.getBoundingClientRect();
  return { x: r.x, y: r.y, w: r.width, h: r.height };
};
window.__scrollTo = (sel, i, block) => {
  const e = document.querySelectorAll(sel)[i || 0]; if (!e) return false;
  e.scrollIntoView({ behavior: 'smooth', block: block || 'center' }); return true;
};


/* ── hand-drawn annotation ──────────────────────────────────────────────
 * A marker-pen ellipse that draws itself on, holds, then fades. Two
 * overlapping passes with per-vertex jitter is what separates it from a
 * geometric ellipse — a clean <ellipse> reads as UI chrome, and the point of
 * this is that it reads as someone pointing.
 *
 * Deterministic: the jitter comes from a seeded PRNG, so a re-record of the
 * same take draws the same squiggle rather than a new one. */
window.__scribInit = () => {
  if (document.getElementById('__pwscrib')) return document.getElementById('__pwscrib');
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.id = '__pwscrib';
  s.setAttribute('style', 'position:fixed;left:0;top:0;width:100vw;height:100vh;' +
    'z-index:2147483643;pointer-events:none;overflow:visible');
  document.body.appendChild(s);
  return s;
};
window.__scribble = (o) => {
  const svg = window.__scribInit();
  const NS = 'http://www.w3.org/2000/svg';
  let seed = (o.seed || 1) * 9301 + 49297;
  const rnd = () => { seed = (seed * 9301 + 49297) % 233280; return seed / 233280; };
  const rx = o.rx || 60, ry = o.ry || 34;
  let d = '';
  // Two loops, the second offset — the way you actually circle something twice.
  for (let loop = 0; loop < 2; loop++) {
    const spin = loop * 0.45;                 // start the 2nd pass elsewhere
    const grow = 1 + loop * 0.08;
    for (let i = 0; i <= 26; i++) {
      const a = spin + (i / 26) * Math.PI * 2 * 1.06;   // >1 turn = overshoot
      const j = 0.90 + rnd() * 0.20;
      const x = o.x + Math.cos(a) * rx * grow * j;
      const y = o.y + Math.sin(a) * ry * grow * j;
      d += (i === 0 && loop === 0 ? 'M' : 'L') + x.toFixed(1) + ' ' + y.toFixed(1) + ' ';
    }
  }
  const p = document.createElementNS(NS, 'path');
  p.setAttribute('d', d);
  p.setAttribute('fill', 'none');
  p.setAttribute('stroke', o.color || '#ff3b6b');
  p.setAttribute('stroke-width', o.width || 3.5);
  p.setAttribute('stroke-linecap', 'round');
  p.setAttribute('stroke-linejoin', 'round');
  svg.appendChild(p);
  const len = p.getTotalLength();
  p.style.strokeDasharray = len;
  p.style.strokeDashoffset = len;
  p.style.transition = 'stroke-dashoffset ' + (o.ms || 700) + 'ms ease-out';
  requestAnimationFrame(() => { p.style.strokeDashoffset = '0'; });
  return true;
};
window.__scribClear = () => {
  const s = document.getElementById('__pwscrib');
  if (s) while (s.firstChild) s.removeChild(s.firstChild);
};

/* Find the Chart.js instance carrying a named series. Resolving by LABEL
 * rather than by canvas index means adding or reordering a chart in the stack
 * does not silently move the annotation onto the wrong graph. */
window.__chartByLabel = (label) => {
  if (!window.Chart || !window.Chart.getChart) return null;
  for (const c of document.querySelectorAll('canvas')) {
    const ch = window.Chart.getChart(c);
    if (ch && (ch.data.datasets || []).some((d) => d.label === label)) return { c, ch };
  }
  return null;
};

/* Circle an actual data point: the nearest sample of the named series to tMs. */
window.__circleSeries = (label, tMs, opts) => {
  const found = window.__chartByLabel(label);
  if (!found) return null;
  const { c, ch } = found;
  const ds = ch.data.datasets.find((d) => d.label === label);
  const pts = (ds.data || []).filter((p) => p && typeof p === 'object' && p.y != null);
  if (!pts.length) return null;
  let best = pts[0], bd = Infinity;
  for (const p of pts) {
    const px = p.x instanceof Date ? p.x.getTime() : Number(p.x);
    const d = Math.abs(px - tMs);
    if (d < bd) { bd = d; best = p; }
  }
  const bx = best.x instanceof Date ? best.x.getTime() : Number(best.x);
  const rect = c.getBoundingClientRect();
  // Clamp into the plot area. The interesting point is almost always the newest
  // one, which sits hard against the live edge — an unclamped circle then spills
  // off the right of the plot into empty page, pointing at nothing.
  const a = ch.chartArea || { left: 0, right: c.clientWidth, top: 0, bottom: c.clientHeight };
  const rx = (opts && opts.rx) || 60, ry = (opts && opts.ry) || 34;
  const px = Math.min(Math.max(ch.scales.x.getPixelForValue(bx), a.left + rx), a.right - rx);
  const py = Math.min(Math.max(ch.scales.y.getPixelForValue(best.y), a.top + ry), a.bottom - ry);
  const x = rect.x + px, y = rect.y + py;
  window.__scribble(Object.assign({ x, y }, opts || {}));
  return { x, y, value: best.y };
};

/* Viewport coords of a Chart.js legend entry, by its text.
 *
 * The legend is drawn INSIDE the canvas, so there is no DOM node to click and
 * no selector to write. Chart.js keeps legendHitBoxes parallel to legendItems,
 * which is the only handle on where an entry actually is. Clicking the real
 * entry (rather than calling setDatasetVisibility) is what makes the app record
 * the choice through useLegendVisibility, so it survives a dataset rebuild. */
window.__legendBox = (text) => {
  for (const c of document.querySelectorAll('canvas')) {
    const ch = window.Chart && window.Chart.getChart(c);
    if (!ch || !ch.legend) continue;
    const items = ch.legend.legendItems || [];
    const boxes = ch.legend.legendHitBoxes || [];
    for (let i = 0; i < items.length; i++) {
      if (items[i] && items[i].text === text && boxes[i]) {
        const r = c.getBoundingClientRect();
        return { x: r.x + boxes[i].left + boxes[i].width / 2,
                 y: r.y + boxes[i].top + boxes[i].height / 2,
                 hidden: !!items[i].hidden };
      }
    }
  }
  return null;
};

/* Hide every dataset in a legend group directly.
 *
 * Clicking the real legend entry would be more faithful — it routes through the
 * app's own useLegendVisibility store and survives a dataset rebuild. It is also
 * unreliable here: the entry is drawn INSIDE the canvas, and once the chart is
 * expanded to 540px the legend sits below the viewport, where a synthetic mouse
 * click cannot reach it. Scrolling it into view then moves the chart out of
 * frame. So set visibility through Chart.js instead and re-assert it as the run
 * goes on, since this route does not persist across a rebuild. */
window.__hideGroup = (group) => {
  let hidden = 0;
  for (const c of document.querySelectorAll('canvas')) {
    const ch = window.Chart && window.Chart.getChart(c);
    if (!ch) continue;
    let touched = false;
    (ch.data.datasets || []).forEach((d, i) => {
      if (d._groupLegend !== group) return;
      if (ch.isDatasetVisible(i)) { ch.setDatasetVisibility(i, false); touched = true; hidden++; }
    });
    if (touched) ch.update('none');
  }
  return hidden;
};

/* How many datasets of a legend group are currently drawn — used to CONFIRM the
 * hide landed, rather than assuming it did. */
window.__groupVisible = (group) => {
  let shown = 0, total = 0;
  for (const c of document.querySelectorAll('canvas')) {
    const ch = window.Chart && window.Chart.getChart(c);
    if (!ch) continue;
    (ch.data.datasets || []).forEach((d, i) => {
      if (d._groupLegend !== group) return;
      total++;
      if (ch.isDatasetVisible(i)) shown++;
    });
  }
  return { shown, total };
};

/* Circle the newest item on the Player State timeline (vis-timeline renders
 * DOM items, not a canvas, so there is no scale to query — the right-most
 * item IS the most recent event). */
window.__circleNewestEvent = (opts) => {
  const items = [...document.querySelectorAll('.vis-item')];
  if (!items.length) return null;
  let best = null, bx = -Infinity;
  for (const it of items) {
    const r = it.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) continue;
    if (r.x > bx) { bx = r.x; best = r; }
  }
  if (!best) return null;
  const x = best.x + best.width / 2, y = best.y + best.height / 2;
  window.__scribble(Object.assign({ x, y, rx: 46, ry: 30 }, opts || {}));
  return { x, y };
};
`;

/* ─── small helpers ─────────────────────────────────────────────────── */

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Fetch JSON from the v2 API, tolerating the self-signed cert on test-dev. */
async function api(pathname) {
  const url = `${BASE}${pathname}`;
  const res = await fetch(url, {
    headers: USER ? { Authorization: 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64') } : {},
  });
  if (!res.ok) throw new Error(`GET ${pathname} -> ${res.status}`);
  return res.json();
}

/** The resolution the player reports, as a short rung name: "3840x2160" -> "2160p". */
function rungName(res) {
  if (!res) return null;
  const m = /x(\d+)$/.exec(String(res));
  return m ? `${m[1]}p` : String(res);
}

function fmtMbps(v) {
  if (v == null || !Number.isFinite(v)) return '?';
  return v >= 10 ? v.toFixed(1) : v.toFixed(2);
}

/** "1 stall" / "2 stalls". pronounce.py's de-pluralising tail only covers
 *  hours/minutes/seconds, and these counts are read aloud.
 *
 *  The -es case is not pedantry: "variant switch" pluralised by appending -s
 *  produced "3 variant switchs", which a speech engine says exactly as written. */
function plural(n, word) {
  if (n === 1) return `${n} ${word}`;
  const es = /(s|x|z|ch|sh)$/.test(word);
  return `${n} ${word}${es ? 'es' : 's'}`;
}

/* ─── QuickTime screen capture of the phone ─────────────────────────────
 * QuickTime's device chooser is NOT scriptable — `new movie recording` uses
 * whatever source was last selected in the UI. So the operator picks
 * "Jonathans iPhone" once by hand (see README preflight) and this reuses it.
 *
 * Starting the recording from here rather than by hand is what makes the sync
 * offset roughly constant instead of unknown: the residual error is QuickTime's
 * own start latency, which one `phoneOffset` nudge in cues.json then pins. */
function osa(...lines) {
  const args = [];
  for (const l of lines) args.push('-e', l);
  return execFileSync('osascript', args, { encoding: 'utf8' }).trim();
}

function phoneStart() {
  // Close anything already open FIRST. Selecting the iPhone leaves a movie
  // recording window on screen, and `new movie recording` would then make a
  // SECOND document — after which `document 1` is whichever QuickTime feels is
  // frontmost, and stop/save could target the wrong one at the end of a
  // twelve-minute take. The device choice survives the close; it is remembered
  // per application, not per window.
  osa('tell application "QuickTime Player" to activate',
      'tell application "QuickTime Player" to close every document saving no');
  osa('tell application "QuickTime Player" to start (new movie recording)');
  const n = osa('tell application "QuickTime Player" to count documents');
  if (n.trim() !== '1') {
    console.error(`  ⚠ QuickTime has ${n} documents open — expected exactly 1.`);
    console.error('    stop/save at the end may target the wrong one.');
  }
}

function phoneStop(dest) {
  // `save` needs the document still open; `stop` finalises the capture.
  osa('tell application "QuickTime Player" to stop document 1');
  // QuickTime needs a beat between stop and save or it writes a 0-byte file.
  spawnSync('sleep', ['2']);
  osa(`tell application "QuickTime Player" to save document 1 in POSIX file "${dest}"`,
      'tell application "QuickTime Player" to close document 1 saving no');
}

/* ─── phone control via Appium ──────────────────────────────────────────
 * A long-lived `demo-device` child, not two commands: the Appium session lives
 * in that process's memory, and the demo needs the app brought to home at one
 * moment and playback started at a LATER one chosen by the recorder. Phases are
 * sequenced over its stdin. See tests/characterization/cmd/demo-device. */
function phoneController() {
  const { spawn } = require('child_process');
  const args = ['run', './cmd/demo-device', '-clip', CONTENT];
  const child = spawn('go', args, { cwd: CHAR_DIR, stdio: ['pipe', 'pipe', 'inherit'] });
  const waiters = [];
  let buf = '';
  child.stdout.on('data', (d) => {
    buf += d.toString();
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim();
      buf = buf.slice(i + 1);
      if (!line) continue;
      console.log(`  [device] ${line}`);
      for (let k = waiters.length - 1; k >= 0; k--) {
        if (line.startsWith(waiters[k].prefix)) { waiters.splice(k, 1)[0].resolve(line); }
        else if (line.startsWith('ERR')) { waiters.splice(k, 1)[0].reject(new Error(line)); }
      }
    }
  });
  const died = new Promise((_r, rej) =>
    child.on('exit', (c) => rej(new Error(`demo-device exited (${c})`))));
  return {
    // Races the child dying, so a crashed launcher fails fast instead of
    // hanging until the caller's timeout.
    wait(prefix, ms) {
      return Promise.race([
        new Promise((resolve, reject) => {
          waiters.push({ prefix, resolve, reject });
          setTimeout(() => reject(new Error(`timed out waiting for ${prefix}`)), ms);
        }),
        died,
      ]);
    },
    send(cmd) { child.stdin.write(cmd + '\n'); },
    stop() { try { child.stdin.write('quit\n'); child.stdin.end(); } catch (e) { /* already gone */ } },
  };
}

/* ─── main ──────────────────────────────────────────────────────────── */

(async () => {
  fs.mkdirSync(OUT, { recursive: true });

  /* 1. Preflight: the panel must start EMPTY --------------------------
   * The take opens on an empty Active Sessions list and a phone sitting on its
   * home screen, so the session APPEARING is something the viewer watches
   * happen. That means clearing any session still registered from a previous
   * run before the camera rolls. */
  // The app goes to its home screen BEFORE the sessions are released. The other
  // order loses: releasing while the app is still playing just makes it
  // re-register a second later, which is exactly the abort the preflight has to
  // raise when a human forgets to stop playback first.
  let phone = null;
  if (APPIUM && !DRY) {
    console.log('bringing the app to its home screen (Appium)…');
    phone = phoneController();
    try {
      await phone.wait('READY', 6 * 60 * 1000);
    } catch (e) {
      console.error(`✗ ${e.message}`);
      console.error('  Is the Appium server up? (appium, on localhost:4723)');
      phone.stop();
      process.exit(1);
    }
  }

  const list = await api('/api/v2/players');
  const items = list.items || [];

  console.log('── preflight ─────────────────────────────────────────────');
  console.log(`  sessions     ${items.length} registered`);
  for (const p of items) {
    const pm = p.player_metrics || p.current_play?.player_metrics || {};
    console.log(`   · #${p.display_id} ${p.id.slice(0, 8)} ${pm.source || '?'} `
      + `${pm.device_model || '?'} ${pm.state || '?'}`);
  }

  if (items.length && !DRY) {
    // v2 is the dashboard's own delete path. go-proxy has a separate v1
    // DELETE /api/session/{id}; they do not share code, and the dashboard's
    // list is what has to end up empty, so use the v2 one.
    for (const p of items) {
      const res = await fetch(`${BASE}/api/v2/players/${p.id}`, {
        method: 'DELETE',
        headers: USER ? { Authorization: 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64') } : {},
      });
      console.log(`  released #${p.display_id} → ${res.status}`);
    }
    await sleep(2000);
    const after = await api('/api/v2/players');
    const left = (after.items || []).length;
    if (left) {
      console.error(`\n✗ ${left} session(s) still registered after release.`);
      console.error('  The app is probably still playing and re-registering. Put it on the');
      console.error('  home screen (stop playback) and run again.');
      process.exit(1);
    }
    console.log('  panel is empty');
  }

  console.log(`  pattern cfg  fill=${FILL} step=${STEP_SECONDS}s margin=${MARGIN}% `
    + `× ${CYCLES} cycle${CYCLES === 1 ? '' : 's'}`);
  console.log('──────────────────────────────────────────────────────────\n');

  if (DRY) {
    console.log('DRY=1 — preflight only. Nothing released, nothing recorded.');
    return;
  }

  /* 2. Browser -------------------------------------------------------- */
  let chromium;
  try {
    ({ chromium } = require('playwright'));
  } catch (e) {
    console.error('playwright is not installed. From tools/demo:  npm install');
    process.exit(1);
  }
  const browser = await chromium.launch({ headless: false, args: [`--window-size=${W},${H}`] });
  const ctx = await browser.newContext({
    viewport: { width: W, height: H },
    ignoreHTTPSErrors: true,
    httpCredentials: USER ? { username: USER, password: PASS } : undefined,
    recordVideo: { dir: OUT, size: { width: W, height: H } },
  });
  await ctx.addInitScript(INIT);
  // Runs before the app's own scripts, so CollapsibleSection reads these on
  // first mount and the take never shows a fold opening on camera.
  await ctx.addInitScript(([keys, open]) => {
    for (const k of keys) {
      try {
        localStorage.setItem('testing_session_collapse_' + k,
          open.includes(k) ? 'true' : 'false');
      } catch { /* private mode — the folds just keep their defaults */ }
    }
  }, [FOLD_KEYS, FOLDS_OPEN]);
  await ctx.addInitScript((charts) => {
    for (const c of charts) {
      try { localStorage.setItem('dashboard_v3_chart_expand_' + c, 'true'); } catch { /* ignore */ }
    }
  }, EXPAND_CHARTS);

  // Phone first, then the page. Playwright starts the webm the moment the page
  // is created, so the phone has to be rolling BEFORE that for the offset to
  // come out positive — render_layout seeks INTO the phone recording to find
  // the browser's t=0, and a negative seek is not a thing.
  let phoneReadyAt = 0;
  if (PHONE && process.env.ANNOTATE_TEST !== '1') {
    console.log('starting QuickTime capture of the phone…');
    phoneStart();
    await sleep(1500);            // let QuickTime actually get going
    phoneReadyAt = Date.now();
  }

  const page = await ctx.newPage();
  // Recording is live from this instant — the setup that follows (navigate,
  // pick the session, hide the bands) is IN the video, at the head.

  /* 3. Cue plumbing ---------------------------------------------------
   * t0 is set HERE, at page creation, because that is when the webm starts.
   *
   * It used to be set after setup finished, which quietly put every caption
   * ahead of the picture by however long setup took — a couple of seconds when
   * setup was fast, and up to half a minute once the legend lookup started
   * polling for the chart to populate. The cue timings are the whole trick, so
   * the clock has to be the video's clock. Setup simply occupies the head of
   * the timeline now, as its own segment, where the FFWD range can compress it. */
  let t0 = Date.now();
  // Measured, not assumed: how far into the phone recording the browser's t=0
  // falls. Still nudge it once per take — this only removes the guesswork about
  // the launch order, not QuickTime's own start latency.
  const phoneOffset = phoneReadyAt ? Math.max(0, (t0 - phoneReadyAt) / 1000) : 0;
  const cues = [];
  const layout = [];
  const segments = [];
  const rects = {};                // named DOM rects, captured for later crops

  const now = () => (Date.now() - t0) / 1000;

  /** Record a caption at the current time. `holdMs` is how long the recorder
   *  will dwell here — make_ass.py uses it only for the LAST cue. */
  function cue(text, holdMs = 4000) {
    const at = now();
    cues.push({ at: Math.round(at * 1000) / 1000, text, holdMs, words: text.split(/\s+/).length });
    console.log(`  [${at.toFixed(1)}s] ${text}`);
    if (CAPTIONS) page.evaluate((t) => window.__say(t), text).catch(() => {});
  }

  /** Record a layout change for render_layout.py. */
  function lay(preset, extra = {}) {
    const at = Math.round(now() * 1000) / 1000;
    layout.push({ at, preset, ...extra });
    console.log(`  [${at.toFixed(1)}s] «layout ${preset}»`);
  }

  function mark(name) {
    const at = now();
    if (segments.length) segments[segments.length - 1].to = at;
    segments.push({ name, from: at, to: at });
  }

  /* The page-side helpers run these through document.querySelectorAll, so a
   * Playwright-only selector reaches the browser as invalid CSS and throws
   * something that names querySelector rather than the real mistake. Both
   * `>> nth=` and `:has-text()` have already cost a take that way; fail here,
   * naming the fix. Pass an index argument instead. */
  function assertCss(sel) {
    const bad = ['>>', ':has-text(', ':text(', ':nth-match('].find((t) => sel.includes(t));
    if (bad) {
      throw new Error(`selector "${sel}" uses Playwright-only syntax (${bad}) — `
        + 'the page-side helpers need plain CSS. Resolve the index first and '
        + 'pass it as the second argument.');
    }
  }

  async function moveTo(sel, i = 0) {
    assertCss(sel);
    const r = await page.evaluate(([s, n]) => window.__rectOf(s, n), [sel, i]);
    if (!r) return null;
    await page.evaluate(([x, y]) => window.__moveCursor(x, y), [r.x + r.w / 2, r.y + r.h / 2]);
    await sleep(450);
    return r;
  }
  // `block` is passed through to scrollIntoView. Defaults to 'center', but the
  // pattern panel wants 'nearest': Apply sits directly above the generated
  // 47-row rate table, so centring Apply drags the table up into frame. With
  // 'nearest' an already-visible target does not scroll at all, and the view
  // stays anchored on the top of the panel where the controls are.
  async function spot(sel, i = 0, block = 'center') {
    assertCss(sel);
    await page.evaluate(([s, n, b]) => window.__scrollTo(s, n, b), [sel, i, block]);
    await sleep(700);
    const r = await page.evaluate(([s, n]) => window.__rectOf(s, n), [sel, i]);
    if (r) {
      // Keyed by selector+index so a later crop preset can resolve the box this
      // take actually had, rather than one recomputed against a changed layout.
      rects[i ? `${sel}#${i}` : sel] = r;
      await page.evaluate((rr) => window.__spot(rr), r);
    }
    return r;
  }
  const unspot = () => page.evaluate(() => window.__spot(null));

  /** Toggle a Chart.js legend entry off by clicking it, and verify it took.
   *  Silent — this is stage dressing, not something the narration mentions. */
  async function hideLegend(text) {
    await page.evaluate(() => window.__scrollTo('canvas'));
    await sleep(500);
    // The legend does not exist until the chart has datasets, which waits on
    // the first SSE payload. Looking too early finds nothing and silently
    // leaves the bands on — poll instead of assuming the page is ready.
    let box = null;
    for (let i = 0; i < 30 && !box; i++) {
      box = await page.evaluate((t) => window.__legendBox(t), text);
      if (!box) await sleep(1000);
    }
    if (!box) { console.error(`  ⚠ legend "${text}" not found — leaving it visible`); return; }
    await page.evaluate((t) => window.__hideGroup(t), text);
    const after = await page.evaluate((t) => window.__groupVisible(t), text);
    if (after.total && after.shown > 0) {
      console.error(`  ⚠ legend "${text}" still showing ${after.shown}/${after.total}`);
    } else {
      console.log(`  legend "${text}" hidden (${after.total} series)`);
    }
  }

  async function clickSel(sel, i = 0) {
    await moveTo(sel, i);
    await page.evaluate(() => window.__clickPulse());
    await page.locator(sel).nth(i).click();
    await sleep(350);
  }

  /* 4. Open the page and select the device ---------------------------- */
  mark('setup');
  lay('web-full');
  await page.goto(PAGE_URL, { waitUntil: 'domcontentloaded' });
  if (CAPTIONS) {
    await page.evaluate(() => window.__capInit());
    console.log('  CAPTIONS=1 — narration drawn in the page. Rehearsal only:');
    console.log('  do not composite a take recorded this way.');
  }
  await page.waitForSelector('.page-card', { timeout: 30000 });

  /* 4a. The empty panel, then the session arriving --------------------- */
  mark('empty');
  await spot('.page-card', 0, 'start');
  cue('Nothing is connected. The dashboard has no session to show.', 6000);
  await sleep(6000);
  await unspot();

  cue('Starting playback on the phone.', 4000);

  if (phone) {
    // On cue, and only now — the empty panel had to be narrated first.
    phone.send('play');
    try {
      await phone.wait('PLAYING', 2 * 60 * 1000);
    } catch (e) {
      console.error(`  ⚠ ${e.message} — falling back to waiting for a manual start`);
    }
  } else {
    console.log('\n  ⏳ waiting for a session to appear — START PLAYBACK ON THE IPHONE\n');
  }
  let chosen = null;
  const waitUntil = Date.now() + WAIT_PLAY_MS;
  while (Date.now() < waitUntil && !chosen) {
    await sleep(1500);
    const now2 = await api('/api/v2/players');
    chosen = (now2.items || []).find((p) => {
      if (PLAYER) return p.id === PLAYER;
      const pm = p.player_metrics || p.current_play?.player_metrics || {};
      return pm.source === 'ios';
    }) || null;
  }
  if (!chosen) {
    console.error('✗ no session appeared — nothing started playing.');
    await ctx.close(); await browser.close();
    process.exit(1);
  }

  const pid = chosen.id;
  console.log(`  session #${chosen.display_id} ${pid} appeared at ${now().toFixed(1)}s`);
  cue('There it is. The phone registered a session the moment it asked for the '
    + 'first playlist.', 6000);
  await sleep(5000);

  // Match on display_id — the pill carries "Session #N", and N came from the
  // same API record. Matching on the device or content tail would be ambiguous
  // the moment a second iPhone connects.
  const pillSel = `.session-tab:has-text("Session #${chosen.display_id}")`;
  await page.waitForSelector(pillSel, { timeout: 30000 });
  // Resolve to a plain index. `:has-text()` is fine for Playwright's own
  // waitForSelector, but clickSel drives the page-side cursor through
  // querySelectorAll, which only speaks CSS.
  const pills = page.locator('.session-tab');
  const pillCount = await pills.count();
  let pillIdx = -1;
  for (let i = 0; i < pillCount; i++) {
    const txt = await pills.nth(i).innerText();
    if (txt.includes(`Session #${chosen.display_id}`)) { pillIdx = i; break; }
  }
  if (pillIdx < 0) {
    console.error(`✗ no pill for Session #${chosen.display_id} among ${pillCount}`);
    await ctx.close(); await browser.close();
    process.exit(1);
  }
  await clickSel('.session-tab', pillIdx);
  await page.waitForSelector(`input[name="tpl-${pid}"]`, { timeout: 30000 });

  // Let the play establish itself before reading anything off it — the first
  // seconds carry startup transients, and the tour narrates measured values.
  await sleep(SETTLE_S * 1000);

  const rec0 = await api(`/api/v2/players/${pid}`);
  const pm0 = rec0.current_play?.player_metrics || rec0.player_metrics || {};
  const variants = rec0.current_play?.manifest?.variants || [];
  console.log(`  device ${pm0.device_model} · ${variants.length} variants · `
    + `rung ${rungName(pm0.video_resolution)} · buffer ${pm0.buffer_depth_s}s`);
  if (pm0.buffer_depth_s && STEP_SECONDS < pm0.buffer_depth_s) {
    console.log(`  ⚠ step ${STEP_SECONDS}s < buffer ${pm0.buffer_depth_s}s — the DISPLAYED variant`);
    console.log('    will trail the cap continuously and never settle between steps.');
  }

  /* 5. Stage dressing -------------------------------------------------- */
  // Clear the plot. BandwidthChart appends a shaded
  // avg→peak band per variant with `hidden: false`, so twelve bands stripe the
  // plot area by default. Useful at a desk, noise at video size.
  for (const g of HIDE_LEGENDS) await hideLegend(g);
  // ANNOTATE_TEST=1 exercises the annotation path against the live chart and
  // screenshots the result, without recording or applying anything. The circle
  // depends on Chart.js internals (legendHitBoxes, scales.getPixelForValue), so
  // it is the one part that cannot be verified by reading the code.
  if (process.env.ANNOTATE_TEST === '1') {
    await page.evaluate(() => window.__scrollTo('canvas'));
    await sleep(800);
    const a = await page.evaluate((t) => window.__circleSeries('Fetching Variant', t, { seed: 7 }), Date.now());
    const b = await page.evaluate((t) => window.__circleSeries('Displayed Variant', t, { seed: 21, color: '#a855f7' }), Date.now());
    console.log('  Fetching Variant  ->', JSON.stringify(a));
    console.log('  Displayed Variant ->', JSON.stringify(b));
    await sleep(1500);
    const shot = path.join(OUT, 'annotate-test.png');
    await page.screenshot({ path: shot });
    console.log('  wrote', shot);
    await ctx.close(); await browser.close();
    return;
  }

  // Anchor at the TOP of the pattern panel and stay there for the whole
  // configure/settle/apply stretch — the generated rate table lives below the
  // fold and never needs to come into frame.
  await page.evaluate(() => window.__scrollTo('.template-row', 0, 'start'));
  await sleep(600);

  /* 5a. Tour the two panels this demo is about ------------------------- */
  mark('tour');
  lay('side-by-side');

  cue(`A real iPhone, playing a live low-latency HLS stream. `
    + `${pm0.device_model}, ${pm0.player_tech} ${pm0.player_tech_version}.`, 6000);
  await sleep(6000);

  const asc = [...variants].sort((a, b) => a.bandwidth - b.bandwidth);
  cue(`The stream publishes ${variants.length} variants, from `
    + `${rungName(asc[0].resolution)} to ${rungName(asc[asc.length - 1].resolution)}.`, 6000);
  await sleep(6000);

  // Player State — the event timeline. Everything quoted here is read back off
  // the record, so the tour cannot describe a startup that did not happen.
  await spot('.vis-timeline', 0, 'center');
  cue(`Player State is the event timeline. First frame at `
    + `${(pm0.first_frame_time_s ?? 0).toFixed(1)} seconds, `
    + `${plural(pm0.profile_shift_count || 0, 'variant switch')} so far.`, 7000);
  await sleep(7000);
  await unspot();

  // Bitrate chart — name the three series the rest of the demo depends on.
  await spot('canvas', 0, 'center');
  cue(`And the bitrate chart. Three lines matter: the cap we impose, the rung `
    + `the player FETCHES, and the rung actually on SCREEN.`, 8000);
  await sleep(8000);

  cue(`Right now all three agree — ${rungName(pm0.video_resolution)}, `
    + `buffer ${(pm0.buffer_depth_s ?? 0).toFixed(0)} seconds. Nothing is `
    + `constraining it yet.`, 7000);
  await sleep(7000);
  await unspot();
  lay('web-full');

  /* 6. Configure the pattern -----------------------------------------
   * ORDER MATTERS. Template FIRST: the Margin / Step duration / Fill density
   * rows sit inside `v-if="activeTemplate !== 'sliders'"`, so they do not
   * exist in the DOM until a template is picked. Reaching for them first is
   * a 30s timeout on a locator that will never resolve.
   *
   * (Picking the template first is also what makes onMaxStepChange /
   * onStepSecondsChange safe. Both read `draft.template ?? 'ramp_up'`, so with
   * no template chosen they would build a ramp_up table under a valley label —
   * unreachable through the UI precisely because those controls are hidden
   * until a template exists, but the fallback is there in the source and is
   * why the order is not arbitrary.)
   *
   * Picking Valley builds a first table at the DEFAULT fill (1.25 → 59 steps);
   * the fill click then rebuilds it at 24 caps → 47 steps. Both tables are on
   * screen briefly, which is exactly why nothing narrates or spotlights until
   * the whole configuration has settled and the step count has been asserted. */
  mark('configure');

  // Radio indices follow the *_CHOICES arrays in NetworkShapingPattern.vue.
  // Note MAX_STEP_CHOICES puts the "None" sentinel LAST, not first.
  const idxOf = (arr, v, what) => {
    const i = arr.indexOf(v);
    if (i < 0) throw new Error(`${what}: ${v} is not one of ${arr.join(', ')}`);
    return i;
  };
  // Template first — the rest of the rows do not render until it is set.
  await clickSel(`input[name="tpl-${pid}"]`,
    idxOf(['sliders', 'square_wave', 'ramp_up', 'ramp_down', 'pyramid', 'valley', 'transient_shock'],
          'valley', 'template'));
  await page.waitForSelector(`input[name="fill-${pid}"]`, { timeout: 15000 });
  await clickSel(`input[name="fill-${pid}"]`, idxOf(['1.125', '1.25', '1.375', 'none'], FILL, 'FILL'));
  await clickSel(`input[name="stps-${pid}"]`, idxOf([6, 12, 18, 24, 60, 120], STEP_SECONDS, 'STEP_SECONDS'));
  await clickSel(`input[name="mgn-${pid}"]`, idxOf([0, 5, 10, 25, 50], MARGIN, 'MARGIN'));
  await sleep(900);

  const stepCount = await page.locator('.step-row').count();
  if (EXPECT_STEPS && stepCount !== EXPECT_STEPS) {
    console.error(`\n✗ pattern has ${stepCount} steps, expected ${EXPECT_STEPS}.`);
    console.error('  The controls did not take. Aborting rather than record the wrong pattern.');
    await ctx.close(); await browser.close();
    process.exit(1);
  }
  console.log(`  pattern configured: ${stepCount} steps\n`);

  // Read the generated rates back out of the table, so everything narrated
  // about the shape of the valley comes from the table the operator can see.
  const rates = await page.$$eval('.step-row input.col-rate',
    (els) => els.map((e) => Number(e.value)).filter((v) => Number.isFinite(v)));
  const topCap = rates.length ? Math.max(...rates) : null;
  const floor = rates.length ? Math.min(...rates) : null;

  await spot(`.template-row`, 0, 'nearest');
  cue(`Valley: hold the cap above the top variant, walk it all the way down, `
    + `then walk it back up.`, 6000);
  await sleep(6000);
  await unspot();

  // Deliberately NOT spotlighting `.steps`. The generated table is 47 rows of
  // rates — accurate, and unreadable at video size. The same facts (count,
  // ceiling, floor) are read out here and shown compactly by `.applied-summary`
  // once the pattern is running.
  cue(`${stepCount} steps at ${STEP_SECONDS} seconds each — from `
    + `${fmtMbps(topCap)} megabits down to ${fmtMbps(floor)} and back.`, 6500);
  await sleep(6500);

  /* 7. Settle, then apply --------------------------------------------- */
  mark('settle');
  cue(`Before touching anything: the cap is off, and the player is holding the `
    + `top rung.`, Math.max(3000, SETTLE_S * 1000));
  await sleep(SETTLE_S * 1000);

  // Baseline the cumulative counters so "that is the Nth shift" counts shifts
  // caused by THIS pattern, not by everything since the play began.
  const pre = await api(`/api/v2/players/${pid}`);
  const preM = pre.current_play?.player_metrics || pre.player_metrics || {};
  const shift0 = preM.profile_shift_count || 0;
  const stall0 = preM.stalling_count || 0;
  const rebuf0 = preM.buffering_count || 0;

  mark('descent');
  await spot('button.apply', 0, 'nearest');
  cue('Applying the pattern.', 2500);
  await sleep(1800);
  await clickSel('button.apply');
  await unspot();

  // Applying collapses the editor back to `.applied-summary`, which takes the
  // 47-row step table off screen on its own — the compact running summary is
  // what stays visible.
  await spot('.applied-summary', 0, 'nearest');
  await sleep(2500);
  await unspot();

  // Expand the chart panel. The stack renders at 200px per chart by default,
  // which is fine on a desk and far too short once the browser is a
  // sub-rectangle of a 1920x1080 frame.
  // Height comes from the seeded expand state, not a click. Assert it anyway:
  // a demo that quietly records 200px charts is the failure this step exists to
  // avoid, and the seed silently doing nothing looks identical to it working.
  const chartH = await page.evaluate(() => {
    const c = document.querySelector('canvas'); return c ? Math.round(c.getBoundingClientRect().height) : 0;
  });
  console.log(`  bandwidth chart height ${chartH}px`);
  if (EXPAND_CHARTS.includes('bandwidth') && chartH < 400) {
    console.error('  ⚠ chart is not expanded (expected ~540px) — check EXPAND_CHARTS');
  }
  await page.evaluate(() => window.__scrollTo('canvas'));
  await sleep(1200);
  lay('side-by-side');

  /* 8. Watch it happen -------------------------------------------------
   * Everything from here is generated from live readings. The recorder makes
   * no claim it did not just measure — the whole point of writing the cue at
   * the moment the value changed rather than scripting the prose in advance. */
  const started = Date.now();
  let lastStep = null, lastFetch = null, lastDisp = null;
  let troughDone = false, recovering = false;
  let minCapSeen = Infinity, lastCue = 0, cycle = 0;
  const shifts = [];

  lastFetch = preM.fetching_resolution;
  lastDisp = preM.video_resolution;

  /* Annotation queue. Circling a transition means scrolling a panel into view
   * and drawing — around a second of work. Doing that inline would stall the
   * poll loop, and at 6s steps a stalled loop misses transitions, so the
   * annotation is queued and drained between polls instead. */
  const pending = [];
  let annotated = 0;
  // OFF by default. The circles draw correctly on the right data point — see
  // ANNOTATE_TEST=1 — but the overlay is position:fixed with viewport
  // coordinates captured at draw time, so any scroll afterwards leaves the
  // circle behind while the chart moves out from under it. Until it is anchored
  // to the page (or redrawn on scroll) it points at nothing more often than not.
  // ANNOTATE_SHIFTS=2 turns it back on.
  const ANNOTATE_SHIFTS = Number(process.env.ANNOTATE_SHIFTS || 0);

  async function drainAnnotations() {
    while (pending.length && pending[0].at <= now()) {
      const a = pending.shift();
      try {
        if (a.kind === 'chart') {
          await page.evaluate(() => window.__scrollTo('canvas'));
          await sleep(400);
          const hit = await page.evaluate(
            ([label, t, s, col]) => window.__circleSeries(label, t, { seed: s, color: col }),
            [a.series, a.tMs, a.seed, a.color]);
          if (!hit) console.error(`  ⚠ no "${a.series}" point to circle`);
        } else if (a.kind === 'timeline') {
          await page.evaluate(() => window.__scrollTo('.vis-timeline'));
          await sleep(400);
          await page.evaluate((s) => window.__circleNewestEvent({ seed: s }), a.seed);
        } else if (a.kind === 'clear') {
          await page.evaluate(() => window.__scribClear());
          await page.evaluate(() => window.__scrollTo('canvas'));
        }
      } catch (e) {
        console.error(`  ⚠ annotation (${a.kind}) failed: ${e.message}`);
      }
    }
  }

  while (Date.now() - started < RUN_TIMEOUT_MS) {
    await sleep(POLL_MS);
    await drainAnnotations();
    let rec;
    try {
      rec = await api(`/api/v2/players/${pid}`);
    } catch (e) {
      continue;                                  // a dropped poll is not a failure
    }
    const m = rec.current_play?.player_metrics || rec.player_metrics || {};
    const sm = rec.current_play?.server_metrics || rec.server_metrics || {};
    const sh = rec.shape || {};
    const step = sh.pattern_step_runtime;
    const cap = sh.pattern_rate_runtime_mbps;

    if (cap != null && cap < minCapSeen) minCapSeen = cap;

    // Pattern finished (or was cleared) — the shape drops its runtime fields.
    if (step == null && lastStep != null) break;

    /* Cycle counting. The proxy's step loop is
     *     stepIndex = (stepIndex + 1) % len(steps)
     * so the pattern runs forever and a cycle boundary is simply the step index
     * going BACKWARDS. Counting wraps is the only way to stop on whole cycles —
     * a wall-clock timeout would cut mid-descent, which is the one place the
     * video must not end. */
    if (step != null) {
      if (lastStep != null && step < lastStep) {
        cycle++;
        console.log(`  ── cycle ${cycle} complete at ${now().toFixed(1)}s ──`);
        if (cycle >= CYCLES) {
          cue(`That is ${plural(CYCLES, 'full cycle')} of the valley.`, 6000);
          lastStep = step;
          break;
        }
        cue(`Cycle ${cycle} done — the cap is back at the top and the player has `
          + `recovered. Going round again.`, 7000);
        lastCue = Date.now();
        troughDone = false;                 // re-arm the per-cycle narration
        recovering = false;
        minCapSeen = Infinity;
      }
      lastStep = step;
    }

    /* Fetched variant changed — the leading edge of an ABR decision. */
    if (m.fetching_resolution && m.fetching_resolution !== lastFetch) {
      const from = rungName(lastFetch), to = rungName(m.fetching_resolution);
      const down = to && from && parseInt(to) < parseInt(from);
      shifts.push({ at: now(), from, to, dir: down ? 'down' : 'up', cap, res: m.fetching_resolution });
      cue(`Cap is now ${fmtMbps(cap)} megabits. The player ${down ? 'drops' : 'raises'} `
        + `what it FETCHES: ${from} to ${to}.`, 5000);
      lastFetch = m.fetching_resolution;
      lastCue = Date.now();

      // Circle the first few downshifts on both surfaces — the bitrate chart
      // (where the rung steps down) and the Player State timeline (where the
      // event lands). Only the first few: an annotation on all 20-odd shifts
      // stops reading as emphasis and starts reading as decoration.
      //
      // The series is "Fetching Variant" — the chart's own name for the rung
      // being pulled (BandwidthChart.vue:561, off video_bitrate_mbps snapped to
      // the nearest rung peak). Its sibling "Displayed Variant" gets circled
      // separately when the change actually reaches the screen, which is the
      // pair this demo exists to show.
      if (down && annotated < ANNOTATE_SHIFTS) {
        annotated++;
        const seed = annotated * 137;
        const t = now();
        pending.push({ at: t + 0.5, kind: 'chart', series: 'Fetching Variant',
                       color: '#ef4444', tMs: Date.now(), seed });
        pending.push({ at: t + 4.5, kind: 'timeline', seed: seed + 11 });
        pending.push({ at: t + 8.5, kind: 'clear' });
      }
    }

    /* Displayed variant changed — the same decision reaching the screen, one
     * buffer-depth later. This lag is the thing the two-up layout exists to
     * show, so it gets its own cue with the measured delay.
     *
     * The delay must be measured against the fetch change that INTRODUCED this
     * resolution, not against the most recent fetch change. At 6s steps under a
     * ~22s buffer the cap moves ~4 times before any one change reaches the
     * screen, so "now minus the last fetch change" would report ~6s for a lag
     * that is really ~22s — a wrong number stated confidently. Search the
     * shift history for the most recent switch TO this rung instead. */
    if (m.video_resolution && m.video_resolution !== lastDisp) {
      const from = rungName(lastDisp), to = rungName(m.video_resolution);
      const origin = [...shifts].reverse().find((s) => s.res === m.video_resolution);
      const lag = origin ? now() - origin.at : null;
      cue(`Now it reaches the screen: ${from} to ${to}`
        + (lag != null ? `, ${lag.toFixed(0)} seconds after it started fetching that rung — `
          + `that gap is the buffer draining.` : '.'), 5000);
      // Circle the arrival too, in the Displayed Variant's own colour — so the
      // two circles on screen are the two halves of the same decision.
      if (annotated && annotated <= ANNOTATE_SHIFTS) {
        const t = now();
        pending.push({ at: t + 0.4, kind: 'chart', series: 'Displayed Variant',
                       color: '#a855f7', tMs: Date.now(), seed: annotated * 311 });
        pending.push({ at: t + 6.0, kind: 'clear' });
      }
      lastDisp = m.video_resolution;
      lastCue = Date.now();
    }

    /* Trough. */
    if (!troughDone && step != null && cap != null && cap <= minCapSeen + 0.001
        && step > 2 && cap < 1.0) {
      troughDone = true;
      lay('phone-full');
      cue(`Bottom of the valley — ${fmtMbps(cap)} megabits. Buffer `
        + `${(m.buffer_depth_s ?? 0).toFixed(0)} seconds, `
        + `${plural((m.stalling_count || 0) - stall0, 'stall')}, `
        + `${plural((m.buffering_count || 0) - rebuf0, 'rebuffer')} so far.`, 8000);
      lastCue = Date.now();
    }

    /* Recovery begins — cap rising again past the floor. */
    if (troughDone && !recovering && cap != null && cap > minCapSeen * 1.5) {
      recovering = true;
      mark('recovery');
      lay('side-by-side');
      cue('The cap starts climbing back. Now the question is how fast the player '
        + 'trusts the extra headroom.', 6000);
      lastCue = Date.now();
    }

    /* Periodic keep-alive so long plateaus are not silent, but not chatty. */
    if (Date.now() - lastCue > 45000 && step != null) {
      cue(`Step ${step} — cap ${fmtMbps(cap)}, fetching ${rungName(m.fetching_resolution)}, `
        + `showing ${rungName(m.video_resolution)}, buffer `
        + `${(m.buffer_depth_s ?? 0).toFixed(0)}s.`, 5000);
      lastCue = Date.now();
    }
  }

  /* 9. Wrap up --------------------------------------------------------- */
  // Clear the pattern before anything else. The recorder REFUSES to start when
  // a pattern is already applied, so a take that left its own behind would
  // block the next one — and leave the device throttled in the meantime. Done
  // through the UI's own Clear button rather than the API, for the same reason
  // everything else here goes through the DOM. CLEAR=0 keeps it running.
  if (process.env.CLEAR !== '0') {
    try {
      // Two .clear buttons exist (applied-summary and step-actions); either
      // one commits null, so take whichever is on screen.
      await page.locator('button.clear').first().click({ timeout: 5000 });
      console.log('  pattern cleared');
    } catch (e) {
      console.error('  ⚠ could not clear the pattern — clear it in the UI, or the');
      console.error('    next take will refuse to start and the device stays throttled.');
    }
  }

  mark('wrap');
  lay('web-full');
  const fin = await api(`/api/v2/players/${pid}`);
  const fm = fin.current_play?.player_metrics || fin.player_metrics || {};
  const downs = shifts.filter((s) => s.dir === 'down').length;
  const ups = shifts.filter((s) => s.dir === 'up').length;
  // A take cut short by RUN_TIMEOUT_MS never completes a cycle, and "Over 0
  // cycles" is a sentence no one should have to hear.
  cue(`${cycle ? `Over ${plural(cycle, 'cycle')}: ` : 'Partway through the first cycle: '}`
    + `${plural(shifts.length, 'variant change')} — ${downs} down, ${ups} back up. `
    + `${plural((fm.stalling_count || 0) - stall0, 'stall')} and `
    + `${plural((fm.buffering_count || 0) - rebuf0, 'rebuffer')} across the whole valley.`, 9000);
  await sleep(9000);

  if (segments.length) segments[segments.length - 1].to = now();

  /* 10. Close and write ------------------------------------------------ */
  const videoPath = await page.video().path();
  await ctx.close();                     // flushes the webm
  await browser.close();

  let phonePath = null;
  if (PHONE) {
    phonePath = path.join(OUT, 'phone.mov');
    try {
      phoneStop(phonePath);
    } catch (e) {
      console.error('QuickTime save failed:', e.message);
      console.error('The browser take is still good — save the phone recording by hand');
      console.error(`and set "phone" in cues.json to its path.`);
      phonePath = null;
    }
  }

  const data = {
    video: videoPath,
    phone: phonePath,
    // Sync between two independently-started recordings. This is the initial
    // guess: QuickTime's start latency after `phoneStart()` returned. Nudge it
    // in cues.json once per take until a visible reaction lines up.
    phoneOffset: process.env.PHONE_OFFSET !== undefined
      ? Number(process.env.PHONE_OFFSET) : Math.round(phoneOffset * 1000) / 1000,
    playerId: pid,
    displayId: chosen.display_id,
    device: pm0.device_model,
    content: pm0.content_name,
    variants: variants.length,
    pattern: { template: 'valley', fill: FILL, stepSeconds: STEP_SECONDS, margin: MARGIN, steps: stepCount },
    rates,
    shifts,
    rects,
    segments,
    layout,
    cues,
  };
  const cuesPath = path.join(OUT, 'cues.json');
  fs.writeFileSync(cuesPath, JSON.stringify(data, null, 2));
  console.log(`\nwrote ${cuesPath} — ${cues.length} cues, ${layout.length} layout marks`);
  console.log(`browser: ${videoPath}`);
  if (phonePath) console.log(`phone:   ${phonePath}`);
  // Tear the Appium session down explicitly. Leaving it open wedges the next
  // run with "create session deadline exceeded", which reads like a device
  // fault rather than a leaked session from the run before.
  if (phone) phone.stop();

  console.log('\nNext: set DEMO_DIR and run  python3 narrator_app.py');
})().catch((e) => { console.error(e); process.exit(1); });
