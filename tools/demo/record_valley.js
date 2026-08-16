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
const USER = process.env.DEMO_USER || '';
const PASS = process.env.DEMO_PASS || '';

// A pattern this long has a lot of dead air. The recorder records everything;
// the FFWD ranges in narrator_app.py compress the plateaus afterwards.
const RUN_TIMEOUT_MS = Number(process.env.RUN_TIMEOUT_MS || 45 * 60 * 1000);

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
window.__rectOf = (sel) => {
  const e = document.querySelector(sel); if (!e) return null;
  const r = e.getBoundingClientRect();
  return { x: r.x, y: r.y, w: r.width, h: r.height };
};
window.__scrollTo = (sel) => {
  const e = document.querySelector(sel); if (!e) return false;
  e.scrollIntoView({ behavior: 'smooth', block: 'center' }); return true;
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
 *  hours/minutes/seconds, and these counts are read aloud. */
function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
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
  osa('tell application "QuickTime Player" to activate',
      'tell application "QuickTime Player" to start (new movie recording)');
}

function phoneStop(dest) {
  // `save` needs the document still open; `stop` finalises the capture.
  osa('tell application "QuickTime Player" to stop document 1');
  // QuickTime needs a beat between stop and save or it writes a 0-byte file.
  spawnSync('sleep', ['2']);
  osa(`tell application "QuickTime Player" to save document 1 in POSIX file "${dest}"`,
      'tell application "QuickTime Player" to close document 1 saving no');
}

/* ─── main ──────────────────────────────────────────────────────────── */

(async () => {
  fs.mkdirSync(OUT, { recursive: true });

  /* 1. Resolve the device -------------------------------------------- */
  const list = await api('/api/v2/players');
  const items = list.items || [];
  const candidates = items.filter((p) => {
    const pm = p.player_metrics || p.current_play?.player_metrics || {};
    return pm.source === 'ios';
  });
  const chosen = PLAYER
    ? items.find((p) => p.id === PLAYER)
    : candidates.find((p) => {
        const pm = p.player_metrics || p.current_play?.player_metrics || {};
        return pm.state === 'playing';
      });

  if (!chosen) {
    console.error('No iOS session is playing. Sessions seen:');
    for (const p of items) {
      const pm = p.player_metrics || p.current_play?.player_metrics || {};
      console.error(`  #${p.display_id}  ${p.id}  ${pm.source || '?'}  ${pm.device_model || '?'}  ${pm.state || '?'}`);
    }
    console.error('\nStart playback on the iPhone first — a shaping pattern applied to an');
    console.error('idle session shapes nothing, and the take records a flat chart.');
    process.exit(1);
  }

  const pid = chosen.id;
  const pm0 = chosen.player_metrics || chosen.current_play?.player_metrics || {};
  const variants = chosen.current_play?.manifest?.variants || [];

  console.log('── preflight ─────────────────────────────────────────────');
  console.log(`  session      #${chosen.display_id}  ${pid}`);
  console.log(`  device       ${pm0.device_model}  ${pm0.player_tech} ${pm0.player_tech_version}`);
  console.log(`  content      ${pm0.content_name}`);
  console.log(`  ladder       ${variants.length} variants`);
  console.log(`  state        ${pm0.state}  buffer ${pm0.buffer_depth_s}s  offset ${pm0.live_offset_s}s`);
  console.log(`  on rung      ${rungName(pm0.video_resolution)} (fetching ${rungName(pm0.fetching_resolution)})`);
  console.log(`  pattern cfg  fill=${FILL} step=${STEP_SECONDS}s margin=${MARGIN}%`);

  // A step shorter than the buffer is the one configuration that quietly ruins
  // the thing this demo exists to show, so say so rather than discover it in
  // playback.
  if (pm0.buffer_depth_s && STEP_SECONDS < pm0.buffer_depth_s) {
    console.log(`  ⚠ step ${STEP_SECONDS}s < buffer ${pm0.buffer_depth_s}s — the DISPLAYED variant will`);
    console.log('    trail the cap continuously and never settle between steps.');
  }
  if (chosen.shape?.pattern) {
    console.error('\n✗ This session already has a pattern applied. Clear it in the UI first,');
    console.error('  or the take starts mid-descent with no settled "before".');
    process.exit(1);
  }
  console.log('──────────────────────────────────────────────────────────\n');

  if (DRY) {
    console.log('DRY=1 — preflight only. Nothing recorded, no pattern applied.');
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
  const page = await ctx.newPage();

  /* 3. Cue plumbing --------------------------------------------------- */
  let t0 = 0;                      // wall clock at recording start
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

  async function moveTo(sel) {
    const r = await page.evaluate((s) => window.__rectOf(s), sel);
    if (!r) return null;
    await page.evaluate(([x, y]) => window.__moveCursor(x, y), [r.x + r.w / 2, r.y + r.h / 2]);
    await sleep(450);
    return r;
  }
  async function spot(sel) {
    await page.evaluate((s) => window.__scrollTo(s), sel);
    await sleep(700);
    const r = await page.evaluate((s) => window.__rectOf(s), sel);
    if (r) {
      rects[sel] = r;
      await page.evaluate((rr) => window.__spot(rr), r);
    }
    return r;
  }
  const unspot = () => page.evaluate(() => window.__spot(null));

  async function clickSel(sel) {
    await moveTo(sel);
    await page.evaluate(() => window.__clickPulse());
    await page.click(sel);
    await sleep(350);
  }

  /* 4. Open the page and select the device ---------------------------- */
  await page.goto(PAGE_URL, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.session-tab', { timeout: 30000 });

  // Match on display_id — the pill carries "Session #N", and N came from the
  // same API record we resolved the player from. Matching on the device or
  // content tail would be ambiguous the moment a second iPhone connects.
  const pillSel = `.session-tab:has-text("Session #${chosen.display_id}")`;
  await page.waitForSelector(pillSel, { timeout: 15000 });
  await page.click(pillSel);
  await page.waitForSelector(`input[name="tpl-${pid}"]`, { timeout: 20000 });

  /* 5. Start both recordings ------------------------------------------ */
  if (PHONE) {
    console.log('starting QuickTime capture of the phone…');
    phoneStart();
    await sleep(1500);            // let QuickTime actually get going
  }
  t0 = Date.now();
  mark('setup');
  lay('web-full');

  cue(`A real iPhone is playing a live low-latency HLS stream. `
    + `${pm0.device_model}, ${pm0.player_tech} ${pm0.player_tech_version}.`, 6000);
  await sleep(6000);

  cue(`The stream publishes ${variants.length} variants, from `
    + `${rungName([...variants].sort((a, b) => a.bandwidth - b.bandwidth)[0].resolution)} to `
    + `${rungName([...variants].sort((a, b) => b.bandwidth - a.bandwidth)[0].resolution)}. `
    + `The player has settled on the top one.`, 6000);
  await sleep(6000);

  /* 6. Configure the pattern -----------------------------------------
   * ORDER MATTERS, and not for the obvious reason. onMaxStepChange and
   * onStepSecondsChange both read `draft.template ?? 'ramp_up'` — so touching
   * fill density or step duration BEFORE picking a template silently builds a
   * ramp_up step table. It is corrected the moment Valley is picked, but any
   * narration or spotlight in between would be describing the wrong pattern.
   * So: configure everything first, in silence, then assert, then narrate. */
  mark('configure');

  // Radio order follows MAX_STEP_CHOICES in NetworkShapingPattern.vue, where
  // the "None" sentinel sits LAST, not first.
  await clickSel(`input[name="fill-${pid}"] >> nth=${['1.125', '1.25', '1.375', 'none'].indexOf(FILL)}`);
  await clickSel(`input[name="stps-${pid}"] >> nth=${[6, 12, 18, 24, 60, 120].indexOf(STEP_SECONDS)}`);
  await clickSel(`input[name="mgn-${pid}"] >> nth=${[0, 5, 10, 25, 50].indexOf(MARGIN)}`);
  // Valley last, so the final build is unambiguously a valley.
  await clickSel(`input[name="tpl-${pid}"] >> nth=${5}`);
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

  await spot(`.template-row`);
  cue(`Valley: hold the cap above the top variant, walk it all the way down, `
    + `then walk it back up.`, 6000);
  await sleep(6000);
  await unspot();

  await spot('.steps');
  cue(`${stepCount} steps at ${STEP_SECONDS} seconds each — from `
    + `${fmtMbps(topCap)} megabits down to ${fmtMbps(floor)} and back.`, 6500);
  await sleep(6500);
  await unspot();

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
  await spot('button.apply');
  cue('Applying the pattern.', 2500);
  await sleep(1800);
  await clickSel('button.apply');
  await unspot();
  lay('side-by-side');

  /* 8. Watch it happen -------------------------------------------------
   * Everything from here is generated from live readings. The recorder makes
   * no claim it did not just measure — the whole point of writing the cue at
   * the moment the value changed rather than scripting the prose in advance. */
  const started = Date.now();
  let lastStep = null, lastFetch = null, lastDisp = null;
  let troughDone = false, recovering = false;
  let minCapSeen = Infinity, lastCue = 0;
  const shifts = [];

  lastFetch = preM.fetching_resolution;
  lastDisp = preM.video_resolution;

  while (Date.now() - started < RUN_TIMEOUT_MS) {
    await sleep(POLL_MS);
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
    if (step != null) lastStep = step;

    /* Fetched variant changed — the leading edge of an ABR decision. */
    if (m.fetching_resolution && m.fetching_resolution !== lastFetch) {
      const from = rungName(lastFetch), to = rungName(m.fetching_resolution);
      const down = to && from && parseInt(to) < parseInt(from);
      shifts.push({ at: now(), from, to, dir: down ? 'down' : 'up', cap, res: m.fetching_resolution });
      cue(`Cap is now ${fmtMbps(cap)} megabits. The player ${down ? 'drops' : 'raises'} `
        + `what it FETCHES: ${from} to ${to}.`, 5000);
      lastFetch = m.fetching_resolution;
      lastCue = Date.now();
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
  mark('wrap');
  lay('web-full');
  const fin = await api(`/api/v2/players/${pid}`);
  const fm = fin.current_play?.player_metrics || fin.player_metrics || {};
  const downs = shifts.filter((s) => s.dir === 'down').length;
  const ups = shifts.filter((s) => s.dir === 'up').length;
  cue(`${plural(shifts.length, 'variant change')} — ${downs} down, ${ups} back up. `
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
    phoneOffset: Number(process.env.PHONE_OFFSET || 0),
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
  console.log('\nNext: set DEMO_DIR and run  python3 narrator_app.py');
})().catch((e) => { console.error(e); process.exit(1); });
