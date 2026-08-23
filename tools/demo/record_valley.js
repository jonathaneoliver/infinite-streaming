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

// FULL-HEIGHT CAPTURE. The viewport is made tall enough to hold the whole
// dashboard, so the recorder never scrolls and framing is chosen afterwards by
// panning a window over the tall frame in render_layout.py.
//
// Two reasons that beats scrolling at record time. Framing stops being an
// irreversible decision taken twelve minutes before anyone sees it, and
// becomes data you can change without re-recording. And a scrolled page can no
// longer ruin a take, because there is nothing to scroll.
//
// 0 keeps the old behaviour: viewport = H, and the recorder scrolls.
const PAGE_H = Number(process.env.PAGE_H || 0);
const TALL = PAGE_H > H;
const VIEW_H = TALL ? PAGE_H : H;

// 2x device pixels. At 1080p output the side-by-side layout scales the browser
// to 0.60x, which is where axis labels and the legend stop being readable —
// and rendering to 4K from a 1x capture only upscales: more pixels, no more
// detail. Capturing at 2x puts REAL pixels behind a 4K frame.
//
// This multiplies with PAGE_H — the recorded frame is (W*DPR x VIEW_H*DPR).
// Watch that product; past roughly 20 megapixels a frame the encoder suffers.
const DPR = Number(process.env.DPR || 2);

const PHONE = process.env.PHONE !== '0';          // drive QuickTime
const PLAYER = process.env.PLAYER || '';          // pin a player_id
// DRY=1 runs the preflight and stops. Nothing is recorded, no pattern is
// applied, the phone is left alone. Worth running before every real take —
// it is the cheap version of finding out the device went idle.
const DRY = process.env.DRY === '1';
// {left, top, right, bottom} for the QuickTime mirror window. Size does not
// affect the capture, so this is purely about not covering the screen.
// Empty to leave it where it is.
const QT_BOUNDS = process.env.QT_BOUNDS === undefined ? '20, 60, 500, 280' : process.env.QT_BOUNDS;
// CAPTIONS=1 draws the narration in the page. Rehearsals only — see __capInit.
const CAPTIONS = process.env.CAPTIONS === '1';
// Preflight the tall capture with a short throwaway recording. On by default:
// it costs ~15s and take 7 lost 22 minutes to exactly this failure.
const PAINT_CHECK = process.env.PAINT_CHECK !== '0';
// How many displayed-variant arrivals to call out IN EACH DIRECTION. The lag
// they describe is one idea; take 7 said it 27 times.
const DISPLAYED_MAX = Number(process.env.DISPLAYED_MAX || 3);
// How many rung changes to call out IN EACH DIRECTION. Take 8 narrated 30 of
// its 39; the chart shows the rest better than a voice can.
const LIMIT_MAX = Number(process.env.LIMIT_MAX || 5);

/* NARRATION DRIVEN BY THE SERVER'S OWN EVENT LABELS.
 *
 * The forwarder classifies every sample as it ingests it — qoe_labels.go and
 * labels.go between them produce a vocabulary of ~29 <severity>=<event> tags,
 * with thresholds in qoe_thresholds.go. It already detects downshift overshoot,
 * downshift storms, conservative ABR, min-variant-stuck, throughput divergence,
 * live-offset drift and startup-time breaches. Measured at 0.05s behind the
 * proxy once its connect-time backfill drains, so it is live for our purposes.
 *
 * Each entry below carries three things, because they are three different
 * questions:
 *
 *   say     what to narrate. Past tense — see `after`.
 *   after   how long to WAIT before saying it. A label fires the instant the
 *           condition is met, when the chart still shows one small kink. The
 *           shape that makes it legible needs more line than that, so the
 *           narration is held until the viewer can actually see what is being
 *           described. Nothing to do with pipeline latency; a deliberate delay.
 *   rank    how much this matters when several land at once. Severity comes
 *           first (the server already ranks error > critical > warning > info);
 *           rank breaks ties within a severity.
 *   every   minimum gap between two narrations OF THE SAME KIND. Per-type,
 *           not global: an overshoot and a stall are different observations and
 *           should not silence each other, but the fifth overshoot in a minute
 *           teaches nothing the first did not.
 *
 * Labels not listed here are recorded and never spoken. shift_up/shift_down,
 * first_frame and play_start are deliberately absent — the recorder narrates
 * those itself, off the metrics, and does not need them twice. timejump is
 * absent because it fired 145 times in one take. */
const LABEL_NARRATION = {
  '*qoe_downshift_overshoot': {
    rank: 3, after: 18, every: 300,
    say: 'That drop went further than it had to. The player gave up more than '
      + 'the limit actually took away, then climbed back — you can see the '
      + 'notch in the fetched line where it overshot and corrected.',
  },
  '*qoe_downshift_storm': {
    rank: 4, after: 20, every: 300,
    say: 'That is a downshift storm — several drops in quick succession rather '
      + 'than one considered step. The player is chasing an estimate that keeps '
      + 'moving under it.',
  },
  '*qoe_abr_conservative': {
    rank: 2, after: 25, every: 300,
    // Still leaving headroom on the table? If it has since climbed to within
    // a rung of what the limit allows, the gap being described has closed.
    stillTrue: (m, sh) => {
      const cap = sh?.pattern_rate_runtime_mbps;
      const got = m?.video_bitrate_mbps;
      return cap != null && got != null && got < cap * 0.6;
    },
    say: 'Look at the gap that has opened between the limit and what the player '
      + 'is fetching. There is headroom there it is not taking. Coming back up '
      + 'is a risk — it has to spend buffer to find out whether the bandwidth '
      + 'is really there — so it climbs slower than it fell.',
  },
  '*qoe_min_variant_stuck': {
    rank: 2, after: 20, every: 300,
    // Only worth saying while it is STILL on the bottom rung.
    stillTrue: (m) => /(^|x)234$/.test(String(m?.video_resolution || '')),
    say: 'It has been parked on the bottom rung for a while now. There is '
      + 'nowhere lower to go, so this is the floor of what the ladder can do '
      + 'about a limit this tight.',
  },
  '*qoe_throughput_divergence': {
    rank: 1, after: 15, every: 300,
    stillTrue: (m, sh) => {
      const client = m?.avg_network_bitrate_mbps;
      const server = sh?.mbps_shaper_avg ?? sh?.mbps_transfer_rate;
      if (client == null || server == null || !server) return false;
      return Math.abs(client - server) / server > 0.4;
    },
    say: 'The client and the server disagree about the throughput here. What '
      + 'AVPlayer believes it is receiving has come apart from what the rate '
      + 'limiter actually pushed — and the player makes its decisions on its '
      + 'own number, not ours.',
  },
  '*qoe_live_offset_concerning': {
    rank: 2, after: 15, every: 300,
    // A live offset that recovered on its own is not worth interrupting for.
    stillTrue: (m) => (m?.live_offset_s ?? 0) > 12,
    say: 'The live offset is stretching. The player is falling behind the live '
      + 'edge because it cannot fetch fast enough to keep up with the clock.',
  },
  'stall_frozen': {
    rank: 9, after: 3, every: 120,
    say: 'And there it stalls — the picture has stopped. The buffer ran dry '
      + 'before the next segment arrived.',
  },
  'stall_segment': {
    rank: 6, after: 5, every: 120,
    say: 'A segment stall — the player waited on a segment that did not arrive '
      + 'in time.',
  },
  '*qoe_rate_cap_breach': {
    /* The kernel-measured served rate went more than 1.25x over the applied
     * cap (qoe_labels.go, RateCapBreachFactor).
     *
     * Worth being precise about, because the obvious reading is wrong: this is
     * NOT the client over-reading. AVPlayer's own network_bitrate routinely
     * reports 2-3x the cap on burst and that is a known quirk, which is exactly
     * why the detector gates on nftables_bandwidth_mbps instead. So when this
     * fires, more bytes really did move than we asked for.
     *
     * Point-in-time, so no stillTrue: the burst happened. Highest rank of the
     * shaping labels — on a demo whose entire subject is a bandwidth limit,
     * "the limit did not hold" outranks anything the player did about it. */
    rank: 7, after: 10, every: 300,
    say: 'The limiter just over-delivered — more bytes went through than the '
      + 'cap allows. That is measured in the kernel, not reported by the '
      + 'player, so it is not the over-read AVPlayer is known for. Usually it '
      + 'is a burst at a step boundary, where the new rate takes effect a '
      + 'fraction late.',
  },
  '*qoe_ladder_gap': {
    /* The complement of abr_conservative, and the reason that label can be
     * trusted: headroom exists, but no rung fits it, so the player staying put
     * is CORRECT. Saying so once stops the whole demo reading as a list of the
     * player's failings when the ladder is what ran out. */
    rank: 1, after: 20, every: 600,
    say: 'There is spare bandwidth here and the player is not using it — but '
      + 'this time it is right not to. The next rung up costs more than the '
      + 'headroom that just appeared, so there is nothing to climb to. That is '
      + 'a gap in the ladder, not caution in the player.',
    stillTrue: (m, sh) => {
      const cap = sh?.pattern_rate_runtime_mbps;
      const got = m?.video_bitrate_mbps;
      return cap != null && got != null && got < cap * 0.8;
    },
  },
  '*qoe_vst_breach': {
    rank: 5, after: 5, every: 600,
    say: 'Video start time breached its threshold there — the time from asking '
      + 'for playback to seeing a frame went past what we would accept.',
  },
};

// Narrate from the server's labels at all. 0 falls back to the recorder's own
// detectors, which cover less and duplicate what the server already computes.
const LABELS = process.env.LABELS !== '0';
// Blanket override for every `every` above, when a take wants more or less.
const LABEL_COOLDOWN_S = Number(process.env.LABEL_COOLDOWN_S || 0);

/* How many things a narrator may say in any rolling minute.
 *
 * The per-type cooldowns are independent by design, which means they cannot
 * see each other: six different labels each firing once is six narrations in a
 * minute and every cooldown is satisfied. A human demonstrator has a felt
 * ceiling regardless of how many interesting things are happening, and past it
 * the audience stops hearing any of it. Milestones are exempt — the spine of
 * the demo is never crowded out by commentary. */
const NARRATION_PER_MIN = Number(process.env.NARRATION_PER_MIN || 6);

/* How many times each NOTABLE ABR behaviour gets narrated.
 *
 * Take 8 contained 5 over-downshifts, 6 multi-rung drops and several flapping
 * windows. Narrating all of them would be the play-by-play problem again: the
 * first instance teaches the viewer something, the fifth is noise. */
const NOTABLE_MAX = {
  overshoot: Number(process.env.NOTABLE_OVERSHOOT || 2),
  plunge: Number(process.env.NOTABLE_PLUNGE || 1),
  hunting: Number(process.env.NOTABLE_HUNTING || 1),
};
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

// How each fold is spoken, and what it is FOR — the narration names the panel
// and says why it is open or shut, which is the part a viewer cannot infer
// from a collapsed header.
const FOLD_SAY = {
  'network-shaping': ['Network Shaping', 'where the network limit gets set'],
  'player-state': ['Player State', 'the event timeline'],
  'bitrate-chart': ['the Bitrate Charts', 'what the player did about it'],
  'fault-injection': ['Fault Injection', 'HTTP errors, hangs, corrupted responses'],
  'content-manipulation': ['Content Manipulation', 'rewriting the manifest under the player'],
  'server-timeouts': ['Server Timeouts', 'holding a connection open until it gives up'],
  'network-log': ['the Network Log', 'every request the proxy handled'],
  'play-log': ['the Play Log', 'all three streams on one scroll'],
  'player-metrics': ['Player Metrics', 'the raw fields'],
  'session-details': ['Session Details', 'the session\'s own identifiers'],
  'focus-window': ['the Focus Window', 'scrubbing the archive'],
};

/** "A, B and C" — an Oxford-free list, because it is read aloud. */
function speakList(items) {
  if (items.length <= 1) return items[0] || '';
  return items.slice(0, -1).join(', ') + ' and ' + items[items.length - 1];
}

// Chart legend groups switched off before the take. "Variant Bands" is the
// twelve shaded avg→peak rung bands, which BandwidthChart shows by default.
// Empty string keeps everything.
// Empty by default: the shaded avg->peak band per variant IS the ladder, and
// seeing the Fetching / Displayed lines step between rungs is most of what the
// chart is for. They were hidden for one take as visual noise and that was
// wrong — without them the two variant lines move against a blank field and the
// viewer has nothing to read the steps against.
//
// Note this is applied to the LIVE PAGE, so unlike framing it is baked into the
// recording. Changing it needs a new take.
const HIDE_LEGENDS = (process.env.HIDE_LEGENDS || '')
  .split(',').map((s) => s.trim()).filter(Boolean);

// Charts to render at double height (200px → 540px). Each MetricsLineChart owns
// its own Expand toggle, persisted under dashboard_v3_chart_expand_<title>, so
// this is seeded the same way as the folds. The panel-level ⤢ in
// BitrateChartPanelToolbar is a DIFFERENT control and does not change height —
// clicking it looked right and grew nothing.
const EXPAND_CHARTS = (process.env.EXPAND_CHARTS === undefined
  ? 'bandwidth' : process.env.EXPAND_CHARTS).split(',').map((s) => s.trim()).filter(Boolean);

// Collapse the left nav rail.
const SIDEBAR_COLLAPSED = process.env.SIDEBAR_COLLAPSED !== '0';

// Chart rolling window. The dashboard defaults to DEFAULT_FOCUS_MS = 10 min,
// which spreads a 4.7-minute valley cycle over half the plot; 5 minutes frames
// roughly one cycle. Unlike the folds and the expand state this is NOT
// persisted — liveSpan is in-memory per player — so it cannot be seeded and has
// to be driven the way the UI does it: Alt + wheel over the canvas.
const FOCUS_MIN = Number(process.env.FOCUS_MIN || 5);

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
/* Vertical centre of an element in DEVICE pixels, measured against the
 * document rather than the viewport. Under a full-height capture nothing
 * scrolls, so this is a fixed coordinate in the recorded frame — which is what
 * lets the compositor pan to it afterwards. */
/* Top and bottom of an element in DEVICE pixels, document-relative. Lets the
 * recorder frame the UNION of two panels — "both of these on screen" — instead
 * of centring on one and hoping the other fits. */
window.__boundsOf = (sel, i, dpr) => {
  const e = document.querySelectorAll(sel)[i || 0];
  if (!e) return null;
  const r = e.getBoundingClientRect();
  const d = dpr || 1;
  return { top: Math.round((r.top + window.scrollY) * d),
           bottom: Math.round((r.bottom + window.scrollY) * d) };
};

/* A legend entry's full rect in viewport px, for the spotlight. __legendBox
 * returns only a click point; parking a highlight on a series needs its box. */
window.__legendRect = (text) => {
  for (const c of document.querySelectorAll('canvas')) {
    const ch = window.Chart && window.Chart.getChart(c);
    if (!ch || !ch.legend) continue;
    const items = ch.legend.legendItems || [];
    const boxes = ch.legend.legendHitBoxes || [];
    for (let i = 0; i < items.length; i++) {
      if (items[i] && items[i].text === text && boxes[i]) {
        const r = c.getBoundingClientRect();
        return { x: r.x + boxes[i].left, y: r.y + boxes[i].top,
                 w: boxes[i].width, h: boxes[i].height };
      }
    }
  }
  return null;
};

/* Latest value of a named series, so the tour can quote what it is showing
 * rather than describe it in the abstract. */
window.__seriesLatest = (label) => {
  const f = window.__chartByLabel(label);
  if (!f) return null;
  const ds = f.ch.data.datasets.find((d) => d.label === label);
  const pts = (ds && ds.data || []).filter((q) => q && typeof q === 'object' && q.y != null);
  return pts.length ? pts[pts.length - 1].y : null;
};

window.__centreOf = (sel, i, dpr) => {
  const e = document.querySelectorAll(sel)[i || 0];
  if (!e) return null;
  const r = e.getBoundingClientRect();
  return Math.round((r.top + window.scrollY + r.height / 2) * (dpr || 1));
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
  /* ONE loop with a small overshoot — the gesture is "circled this", not
   * "scribbled over it". Two passes read as the latter.
   *
   * The overshoot matters more than the wobble: a stroke that closes exactly on
   * its start looks drawn by a machine, and one that carries past and crosses
   * itself looks like a hand that did not stop in time. STEPS is high enough
   * that the jitter reads as an unsteady line rather than a coarse polygon.
   *
   * The jitter tapers in over the first few points, because a pen is
   * steadiest where it starts and drifts as it goes. */
  const STEPS = 64;
  const TURN = o.turn || 1.12;                // >1 = carry past the start
  for (let i = 0; i <= STEPS; i++) {
    const t = i / STEPS;
    const a = -Math.PI * 0.55 + t * Math.PI * 2 * TURN;   // start upper-left
    const ease = Math.min(1, t * 4);
    const j = 1 + (rnd() - 0.5) * 0.13 * ease;
    const drift = 1 + t * 0.05;               // the loop opens slightly
    const x = o.x + Math.cos(a) * rx * j * drift;
    const y = o.y + Math.sin(a) * ry * j * drift;
    d += (i === 0 ? 'M' : 'L') + x.toFixed(1) + ' ' + y.toFixed(1) + ' ';
  }
  /* The path goes inside a <g> so it can be MOVED without being redrawn.
   * Redrawing each frame would re-roll nothing (the jitter is seeded) but would
   * restart the stroke reveal, and the shape must stay the shape it was. */
  const g = document.createElementNS(NS, 'g');
  const p = document.createElementNS(NS, 'path');
  p.setAttribute('d', d);
  p.setAttribute('fill', 'none');
  p.setAttribute('stroke', o.color || '#ff3b6b');
  p.setAttribute('stroke-width', o.width || 3.5);
  p.setAttribute('stroke-linecap', 'round');
  p.setAttribute('stroke-linejoin', 'round');
  g.appendChild(p);
  svg.appendChild(g);
  const len = p.getTotalLength();
  p.style.strokeDasharray = len;
  p.style.strokeDashoffset = len;
  p.style.transition = 'stroke-dashoffset ' + (o.ms || 780)
    + 'ms cubic-bezier(.35,.05,.6,1)';
  requestAnimationFrame(() => { p.style.strokeDashoffset = '0'; });

  /* Every drawing expires; only an anchored one also MOVES.
   *
   * Expiry used to be a side effect of tracking, so an un-anchored scribble
   * lived until something cleared the whole overlay — which is why an explicit
   * clear existed, and why that clear could land in the middle of a wait and
   * delete a circle that had not been drawn yet. */
  window.__scribTrack(g, o.anchor || null, { x: o.x, y: o.y }, o.holdMs || 9000);
  return true;
};

/* Keep a drawing glued to its data point while the chart scrolls underneath.
 *
 * One rAF loop for every live annotation. Each frame it re-asks Chart.js where
 * the anchored timestamp now sits and translates the group by the difference
 * from where it was drawn. Chart.js is the authority on that mapping, so this
 * follows a pan, a zoom and a rescale identically — there is no rate to measure
 * and nothing to drift out of sync.
 *
 * Hides rather than removes when the point scrolls off the left of the plot:
 * the window can be widened again, and a circle that reappears with its data is
 * better than one that vanished permanently. */
window.__scribLive = window.__scribLive || [];
window.__scribTrack = (g, anchor, drawnAt, holdMs) => {
  window.__scribLive.push({ g, anchor, drawnAt, until: Date.now() + holdMs });
  if (window.__scribRAF) return;
  const step = () => {
    const now = Date.now();
    window.__scribLive = window.__scribLive.filter((a) => {
      if (now > a.until) {
        if (a.g.parentNode) a.g.parentNode.removeChild(a.g);
        return false;
      }
      if (!a.anchor) return true;          // expires, but never moves
      const at = window.__scribAnchorXY(a.anchor);
      if (!at) { a.g.style.display = 'none'; return true; }
      a.g.style.display = '';
      a.g.setAttribute('transform',
        'translate(' + (at.x - a.drawnAt.x).toFixed(1) + ','
        + (at.y - a.drawnAt.y).toFixed(1) + ')');
      return true;
    });
    if (window.__scribLive.length) {
      window.__scribRAF = requestAnimationFrame(step);
    } else {
      window.__scribRAF = null;
    }
  };
  window.__scribRAF = requestAnimationFrame(step);
};

/* Where does an anchored data point sit RIGHT NOW, in viewport pixels?
 * Returns null once it has scrolled out of the plot area. */
window.__scribAnchorXY = (anchor) => {
  const found = window.__chartByLabel(anchor.label);
  if (!found) return null;
  const { c, ch } = found;
  if (!ch.scales || !ch.scales.x) return null;
  const a = ch.chartArea;
  if (!a) return null;
  const px = ch.scales.x.getPixelForValue(anchor.bx);
  if (px < a.left - 4 || px > a.right + 4) return null;   // scrolled away
  const py = ch.scales.y.getPixelForValue(anchor.by);
  const rect = c.getBoundingClientRect();
  return { x: rect.x + px, y: rect.y + py };
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
  /* Reserve the DRAWN extent, not the nominal radius.
   *
   * The stroke is not an rx-by-ry ellipse: it opens by 5% as it goes round
   * (drift) and carries up to ~6.5% of jitter, so it reaches about 1.12x. The
   * clamp used to reserve rx exactly, which is fine in the middle of the plot
   * and wrong at the live edge — precisely where a change that JUST happened
   * sits. The circle then hung off the right of the chart into the page margin.
   *
   * Tracking pulls it back inside within a second or two as the data scrolls
   * inward, but the first seconds are the ones being narrated. */
  const REACH = 1.15;
  const ex = rx * REACH, ey = ry * REACH;
  const rawX = ch.scales.x.getPixelForValue(bx);

  /* Not yet. A change that just happened sits against the live edge, where a
   * whole circle cannot fit. Sliding it inward to make it fit would point it at
   * a value the data does not have, so wait instead: the point scrolls inward
   * at a few pixels a second and will clear the edge shortly. The caller
   * re-tries. */
  if (opts && opts.requireInside !== false) {
    if (rawX > a.right - ex) return { waiting: true, shortBy: Math.round(rawX - (a.right - ex)) };
    if (rawX < a.left + ex) return { waiting: false, gone: true };
  }
  const px = Math.min(Math.max(rawX, a.left + ex), a.right - ex);
  const py = Math.min(Math.max(ch.scales.y.getPixelForValue(best.y), a.top + ey), a.bottom - ey);
  const x = rect.x + px, y = rect.y + py;
  /* Hand the DATA coordinate down, not just the pixel one. The pixel is where
   * the point is now; the anchor is what it means, and only the anchor survives
   * the next scroll. */
  window.__scribble(Object.assign(
    { x, y, anchor: { label, bx, by: best.y } }, opts || {}));
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

/** Height in lines, for comparing two rungs. "1080p" -> 1080.
 *
 *  Ordering by the number rather than a fixed ladder list, so this keeps
 *  working on content whose ladder is not the one this demo happens to use.
 *  Returns -1 for anything unparseable, which sorts below every real rung. */
function idxOfRung(name) {
  const m = /^(\d+)p$/.exec(String(name || ''));
  return m ? Number(m[1]) : -1;
}

function fmtMbps(v) {
  if (v == null || !Number.isFinite(v)) return '?';
  return v >= 10 ? v.toFixed(1) : v.toFixed(2);
}

/** Record a few seconds with the real capture settings and check that the tall
 *  viewport actually PAINTS into the video.
 *
 *  The naive version of this — sample one pixel three-quarters of the way down
 *  — cries wolf. With no session selected the dashboard is short, so that depth
 *  lands past the content and reads grey whether or not anything is wrong.
 *
 *  So compare two numbers instead: where the picture STOPS in the recorded
 *  frame, and where the document's content actually ENDS. A short page is fine
 *  (those agree). A page that outruns its own capture is the bug — that is take
 *  7, where paint stopped dead at 1268 CSS px while content ran well past it.
 *
 *  Returns {ok, detail}. ok on any internal error: a preflight that cannot run
 *  must not block a take. */
async function paintCheck(chromium) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'paintcheck-'));
  try {
    const b = await chromium.launch({ headless: false, args: [`--window-size=${W},${H}`] });
    const ctx = await b.newContext({
      viewport: { width: W, height: VIEW_H },
      deviceScaleFactor: DPR,
      ignoreHTTPSErrors: true,
      httpCredentials: USER ? { username: USER, password: PASS } : undefined,
      recordVideo: { dir, size: { width: W * DPR, height: VIEW_H * DPR } },
    });
    const p = await ctx.newPage();
    await p.goto(`${BASE}/dashboard/testing.html`, { waitUntil: 'networkidle', timeout: 60000 });
    await p.waitForTimeout(5000);

    /* Where does the document's own content end, in CSS px? scrollHeight is no
     * use — it is floor-limited by the viewport, so a short page still reports
     * 3416 against a 3400 viewport. Ask the elements instead. */
    const contentBottom = await p.evaluate(() => {
      let max = 0;
      for (const el of document.body.querySelectorAll('*')) {
        const r = el.getBoundingClientRect();
        if (r.width && r.height) max = Math.max(max, r.bottom + window.scrollY);
      }
      return Math.round(max);
    });

    await ctx.close();
    await b.close();

    const clip = fs.readdirSync(dir).filter((f) => f.endsWith('.webm'))[0];
    if (!clip) return { ok: true, detail: 'no clip written' };

    // One column down the middle of the frame.
    const col = require('child_process').execFileSync('ffmpeg',
      ['-v', 'error', '-i', path.join(dir, clip), '-frames:v', '1',
       '-vf', `crop=w=2:h=${VIEW_H * DPR}:x=${W}:y=0`,
       '-f', 'rawvideo', '-pix_fmt', 'rgb24', '-'], { maxBuffer: 1 << 24 });

    const isGrey = (r, g, bl) => Math.abs(r - g) < 8 && Math.abs(g - bl) < 8
      && r > 100 && r < 190;
    const rows = Math.floor(col.length / 6);
    let paintStops = null;
    for (let i = 0; i < rows; i += 1) {
      const o = i * 6;
      if (!isGrey(col[o], col[o + 1], col[o + 2])) continue;
      let run = true;
      for (let j = i; j < Math.min(i + 60, rows); j += 1) {
        const q = j * 6;
        if (!isGrey(col[q], col[q + 1], col[q + 2])) { run = false; break; }
      }
      if (run) { paintStops = Math.round(i / DPR); break; }
    }

    if (paintStops == null) {
      return { ok: true, detail: `paints to the bottom (content ends ${contentBottom}px)` };
    }
    // 60px of slack: the boundary is read off a single column, and the last
    // element's box can sit a little above the visual end of the page.
    const ok = paintStops + 60 >= Math.min(contentBottom, VIEW_H);
    return {
      ok,
      detail: `paint stops at ${paintStops}px CSS, content ends at ${contentBottom}px`
        + (ok ? ' — consistent' : ' — CONTENT IS BEING CUT OFF'),
    };
  } catch (e) {
    return { ok: true, detail: `could not run (${e.message.split('\n')[0]})` };
  } finally {
    try { fs.rmSync(dir, { recursive: true, force: true }); } catch (e) { /* best effort */ }
  }
}

/** Subscribe to the forwarder's labelled event stream for one play.
 *
 *  Calls onLabel(severity, label, tsMs) for each label on each row, once.
 *  Two things it has to get right:
 *
 *  BACKFILL. Connecting replays the play's history — 1041 rows against the
 *  proxy's 92 in one minute of measuring — and those are minutes-old rows being
 *  caught up, not live events. Narrating them would fire every label at once,
 *  in the past, out of order. Rows stamped before the take began are dropped.
 *
 *  DEDUPE. The same condition can label consecutive samples, so a label is
 *  taken once per (label, second) rather than once per row.
 *
 *  Never throws into the take: if the sidecar is absent the stream simply never
 *  yields, and the caller keeps its own detectors. */
async function subscribeLabels(playerId, playId, sinceMs, onLabel) {
  const url = `${BASE}/analytics/api/v2/timeseries?streams=events`
    + `&player_id=${encodeURIComponent(playerId)}`
    + (playId ? `&play_id=${encodeURIComponent(playId)}` : '');
  let res;
  try {
    res = await fetch(url, { headers: { accept: 'text/event-stream' } });
  } catch (e) {
    console.error(`  ⚠ label stream unavailable (${e.message}) — `
      + 'falling back to the recorder\'s own detectors');
    return false;
  }
  if (!res.ok || !res.body) {
    console.error(`  ⚠ label stream returned ${res.status} — using own detectors`);
    return false;
  }
  console.log('  label stream connected — narrating from the server\'s own '
    + 'classification');

  (async () => {
    const seen = new Set();
    let buf = '';
    try {
      for await (const chunk of res.body) {
        buf += Buffer.from(chunk).toString('utf8');
        const lines = buf.split('\n');
        buf = lines.pop() || '';
        for (const line of lines) {
          if (!line.startsWith('data:')) continue;
          let d;
          try { d = JSON.parse(line.slice(5).trim()); } catch (e) { continue; }
          const labels = d.labels;
          if (!labels || !labels.length || !d.ts) continue;
          // "2026-08-21 22:53:02.209" is UTC; Date.parse needs the marker.
          const tsMs = Date.parse(d.ts.replace(' ', 'T') + 'Z');
          if (!Number.isFinite(tsMs) || tsMs < sinceMs) continue;   // backfill
          for (const raw of labels) {
            const [sev, name] = String(raw).includes('=')
              ? [String(raw).split('=')[0], String(raw).split('=').slice(1).join('=')]
              : ['info', String(raw)];
            const key = `${name}@${Math.floor(tsMs / 1000)}`;
            if (seen.has(key)) continue;
            seen.add(key);
            onLabel(sev, name, tsMs);
          }
        }
      }
    } catch (e) {
      console.error(`  ⚠ label stream ended: ${e.message}`);
    }
  })();
  return true;
}

/** Measured audio bitrate of the stream's audio rendition, in kbps.
 *
 *  Segment bytes over segment seconds. Includes container overhead, so it
 *  reads a few percent above the encoder's target — which is the honest number
 *  anyway, since those bytes are on the wire and count against the cap.
 *
 *  Returns null on any failure; the caller drops the figure rather than
 *  guessing. */
async function audioKbps(masterUrl) {
  try {
    const base = masterUrl.replace(/[^/]*$/, '');
    const master = await (await fetch(`${BASE}/${masterUrl}`)).text();
    const m = /URI="([^"]+audio[^"]*\.m3u8)"/i.exec(master);
    if (!m) return null;
    const mediaUrl = m[1].startsWith('/') ? m[1].slice(1) : base + m[1];
    const media = await (await fetch(`${BASE}/${mediaUrl}`)).text();
    const seg = /#EXTINF:([\d.]+),\s*\n(\S+)/.exec(media);
    if (!seg) return null;
    const dur = parseFloat(seg[1]);
    const segUrl = seg[2].startsWith('/') ? seg[2].slice(1)
      : mediaUrl.replace(/[^/]*$/, '') + seg[2];
    const r = await fetch(`${BASE}/${segUrl}`);
    const buf = await r.arrayBuffer();
    if (!dur || !buf.byteLength) return null;
    return Math.round((buf.byteLength * 8) / dur / 1000);
  } catch (e) {
    return null;
  }
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

  // Assert the SOURCE, not just that a document exists.
  //
  // `natural dimensions` reports the capture size: 2556x1180 for this iPhone's
  // screen, ~1920x1080 or 1280x720 for a camera. QuickTime's device chooser is
  // not scriptable, so a restart can silently leave the wrong source selected
  // and the take records a webcam for twelve minutes while everything else
  // looks healthy.
  //
  // It populates a moment AFTER start, not immediately — polling rather than
  // reading once is the difference between this check working and it reporting
  // a false 0,0.
  let dims = '';
  for (let i = 0; i < 20; i++) {
    dims = osa('tell application "QuickTime Player" to get natural dimensions of document 1').trim();
    if (dims && !/^0,\s*0$/.test(dims)) break;
    spawnSync('sleep', ['1']);
  }
  const [dw, dh] = dims.split(',').map((v) => Number(v.trim()));
  if (!dw || !dh) {
    throw new Error('QuickTime reports no capture source (natural dimensions 0,0) — '
      + 'pick the iPhone in File > New Movie Recording');
  }
  console.log(`  phone source ${dw}x${dh}`);

  // Shrink and park the mirror window. QuickTime captures the DEVICE at its
  // native resolution, so the window is only a monitor — its size has no effect
  // on the recording, and a full-size iPhone mirror otherwise sits across the
  // screen for twelve minutes for no benefit.
  //
  // Not minimised: a windowed capture keeps rendering, and hiding it entirely
  // removes the one way to notice mid-take that the phone has stopped.
  if (QT_BOUNDS) {
    try {
      osa(`tell application "QuickTime Player" to set bounds of window 1 to {${QT_BOUNDS}}`);
      console.log(`  phone window parked at {${QT_BOUNDS}}`);
    } catch (e) {
      console.error('  ⚠ could not resize the QuickTime window — harmless, carrying on');
    }
  }
  // A landscape phone screen is wide and short; a webcam is ~16:9 at a much
  // smaller width. Warn rather than throw: a different device is legitimate.
  if (dw < 1500) {
    console.error(`  ⚠ ${dw}x${dh} does not look like a phone screen — is a CAMERA selected?`);
  }
}

/* Stop the capture and get the file where we want it.
 *
 * Two things about QuickTime make the obvious one-liner fail, and take1 lost
 * its phone recording to both:
 *
 * 1. QuickTime Player IS SANDBOXED. `save … in POSIX file "<anywhere>"` fails
 *    with "You don't have permission", and the modal that raises blocks the
 *    AppleEvent — which surfaces 60s later as -1712 "AppleEvent timed out".
 *    That misdiagnoses as a slow save, and a longer timeout does nothing.
 *    ~/Movies is inside the sandbox's allowed set, so save there and let the
 *    SHELL (not sandboxed) move the result.
 *
 * 2. `save` writes a .qtpxcomposition BUNDLE — a directory containing
 *    `Movie Recording.mov` — not a flat file at the path you named. The inner
 *    movie is the original capture, no re-encode, so lifting it out beats
 *    `export`, which would transcode.
 *
 * `with timeout` stays: harmless, and a genuinely long finalise is plausible
 * for a twelve-minute capture even once the permission problem is gone. */
function phoneStop(dest) {
  const stage = path.join(os.homedir(), 'Movies', `demo-take-${Date.now()}.mov`);

  osa('tell application "QuickTime Player" to stop document 1');
  // QuickTime needs a beat between stop and save or it writes a 0-byte file.
  spawnSync('sleep', ['3']);
  osa('tell application "QuickTime Player"',
      '  with timeout of 900 seconds',
      `    save document 1 in POSIX file "${stage}"`,
      '  end timeout',
      'end tell');
  osa('tell application "QuickTime Player" to close every document saving no');

  // Accept either shape — a flat file, or the bundle's inner movie.
  const inner = path.join(`${stage}.qtpxcomposition`, 'Movie Recording.mov');
  const src = fs.existsSync(stage) ? stage : (fs.existsSync(inner) ? inner : null);
  if (!src) {
    throw new Error(`QuickTime wrote neither ${stage} nor ${inner}`);
  }
  // copy+unlink, NOT rename: QuickTime must stage inside ~/Movies (its sandbox
  // allows nothing else) while DEMO_DIR lives on an external drive, and
  // rename() across filesystems fails EXDEV. That cost take 2 its automatic
  // save — the recording was fine, the move was not.
  fs.copyFileSync(src, dest);
  fs.unlinkSync(src);
  fs.rmSync(`${stage}.qtpxcomposition`, { recursive: true, force: true });

  // Assert it is real. A 0-byte or truncated file here is worth failing on
  // while the operator is still in the room.
  const size = fs.statSync(dest).size;
  if (size < 1_000_000) {
    throw new Error(`phone recording is only ${size} bytes — capture did not take`);
  }
  console.log(`  phone recording ${(size / 1e9).toFixed(2)} GB → ${dest}`);
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
      // The real-device path goes to :4799 (CHAR_IOS_DIRECT_APPIUM_URL), not
      // the sim farm's :4723 — naming the wrong port sends the next person to
      // check a server that was never involved.
      console.error('  XCTDaemonErrorDomain Code=41 ("Not authorized for performing');
      console.error('  UI testing actions") reads as a phone problem and usually is not.');
      console.error('  Check THIS MACHINE first — the host causes are invisible to');
      console.error('  tunnel/list/status checks, which all report healthy anyway:');
      console.error('');
      console.error('    1. an ORPHANED WebDriverAgent holding the device\'s one XCTest');
      console.error('       session. The tell is that it is OLDER than the running');
      console.error('       appium, so it cannot be appium\'s child:');
      console.error('         ps -o pid,lstart,command -p $(pgrep -f "xcodebuild.*WebDriverAgent" | head -1)');
      console.error('         pgrep -f "appium --port"');
      console.error('       Fix: pkill -f "xcodebuild.*WebDriverAgent"');
      console.error('    2. appium on :4799   (curl -s localhost:4799/status)');
      console.error('       — the real-device path is :4799, NOT the sim farm\'s :4723');
      console.error('    3. go-ios tunnel up  (ios tunnel ls)');
      console.error('');
      console.error('  Only then the device: is it LOCKED, and is Settings > Developer >');
      console.error('  Enable UI Automation on? If the appium log shows a [Xcode] line');
      console.error('  from WebDriverAgentRunner-Runner[pid], WDA already LAUNCHED on the');
      console.error('  phone — pairing, signing and Developer Mode are fine, so suspect 1.');
      console.error('  See .claude/findings/orphaned-wda-blocks-session-code41-2026-08-21.md');
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

  let chromium;
  try {
    ({ chromium } = require('playwright'));
  } catch (e) {
    console.error('playwright is not installed. From tools/demo:  npm install');
    process.exit(1);
  }

  /* Will the tall viewport actually PAINT into the recorded video?
   *
   * Take 7 recorded 22 minutes in which everything below 1268 CSS px was flat
   * grey — unpainted compositor surface, cutting mid-chart, while the page
   * itself reported scrollHeight 3416. The page was tall; the capture was not.
   *
   * It cannot be checked from inside the run: page.screenshot() goes through
   * CDP's captureScreenshot, which rasterises beyond the window surface and so
   * reports healthy even when the video is grey (this cost a debugging pass).
   * And the video is unreadable until the context closes. So the only honest
   * check is a short throwaway recording made with the same settings.
   *
   * Fifteen seconds to avoid discovering it twenty-two minutes later. */
  if (TALL && PAINT_CHECK) {
    const { ok, detail } = await paintCheck(chromium);
    console.log(`  paint check: ${detail}`);
    if (!ok) {
      console.error('✗ the tall viewport is NOT painting into the recorded video.');
      console.error('  Everything below the window surface would come out flat grey.');
      console.error('  Known causes, in order:');
      console.error('    1. the browser window was moved between displays with');
      console.error('       different scale factors, or resized, during a previous run');
      console.error('    2. headless — measured WORSE than headed for this, not better');
      console.error('    3. the window is occluded or on another Space');
      console.error('  Leave the browser window alone once the take starts.');
      console.error('  Set PAINT_CHECK=0 to record anyway.');
      process.exit(1);
    }
  }


  if (DRY) {
    console.log('DRY=1 — preflight only. Nothing released, nothing recorded.');
    return;
  }

  /* 2. Browser -------------------------------------------------------- */
  const browser = await chromium.launch({ headless: false, args: [`--window-size=${W},${H}`] });
  const ctx = await browser.newContext({
    viewport: { width: W, height: VIEW_H },
    deviceScaleFactor: DPR,
    ignoreHTTPSErrors: true,
    httpCredentials: USER ? { username: USER, password: PASS } : undefined,
    // Record at the DEVICE pixel size, not the CSS size. Leaving this at the
    // CSS viewport would downsample the 2x render straight back to 1x and
    // throw away exactly the detail deviceScaleFactor was set to capture.
    recordVideo: { dir: OUT, size: { width: W * DPR, height: VIEW_H * DPR } },
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
  // The nav rail costs ~250px of a 1600px-wide capture and carries nothing the
  // demo refers to. ShellLayout persists its state under this key, so it is
  // seeded like the folds rather than clicked — no collapse animation on camera.
  await ctx.addInitScript((collapse) => {
    try { localStorage.setItem('ismSidebarCollapsed', collapse ? '1' : '0'); } catch { /* ignore */ }
  }, SIDEBAR_COLLAPSED);

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
  /** `on` names a focus target, as for lay(). It becomes the cue's `pan`, and
   *  the compositor moves the framing to it as the line is spoken — so the
   *  view follows the narration instead of holding one crop for four minutes. */
  /* A cue is only worth emitting if there is TIME TO SAY IT.
   *
   * Take 7 shipped with 44 of its 104 cues overrunning their slot by 137
   * seconds in total, because cues are stamped at event times and ABR events
   * cluster: during a fast descent two variant changes can land 0.6s apart,
   * and each wants ~7s of speech. The narration then talks over itself for the
   * rest of the take, and the captions cut each other off.
   *
   * The recorder cannot know the generated audio length — that is Voicebox's
   * business, later. But speech rate is stable enough to budget against:
   * take 7's 104 clips measured 12.6 characters per second overall.
   *
   * So: track when the last cue would stop speaking, and drop any cue MARKED
   * DROPPABLE that would collide with it. Droppable is opt-in, not the
   * default — the tour, the trough and the cycle boundaries are the spine of
   * the demo, and a rule that could silently delete them would be worse than
   * the overlap it fixes. Only the per-change play-by-play opts in, and not
   * narrating every single rung change is the right outcome anyway: nobody can
   * follow five of them in ten seconds. */
  const SPEAK_GAP_MS = Number(process.env.SPEAK_GAP_MS || 400);
  const SPEAK_CPS = Number(process.env.SPEAK_CPS || 13.5);
  let speakingUntil = 0;
  let cuesDropped = 0;
  // Rolling budget (#4) — take-seconds at which each cue was spoken.
  const spokenAt = [];
  let budgetDropped = 0;
  /* Hush window (#1) — take-seconds until which commentary stays quiet.
   *
   * When the narration has just promised something ("now watch how fast it
   * trusts the headroom"), the right thing to do is stop talking and let it
   * happen. A demonstrator goes quiet over their own payoff; we were narrating
   * straight through it. Only `critical` gets through — a stall during the
   * payoff IS the payoff. */
  let hushUntil = 0;
  let hushDropped = 0;

  function speechSeconds(text) {
    return text.length / SPEAK_CPS;
  }

  function cue(text, holdMs = 4000, on = null, opts = {}) {
    const at = now();
    // #1 — quiet over the payoff. Milestones and critical events still speak.
    if (opts.budgeted && !opts.critical && at < hushUntil) {
      hushDropped += 1;
      console.log(`  [${at.toFixed(1)}s] (hushed for another `
        + `${(hushUntil - at).toFixed(0)}s) ${text.slice(0, 50)}`);
      return false;
    }
    // #4 — rolling per-minute ceiling across ALL commentary, which the
    // independent per-type cooldowns cannot enforce between them.
    if (opts.budgeted) {
      while (spokenAt.length && at - spokenAt[0] > 60) spokenAt.shift();
      if (spokenAt.length >= NARRATION_PER_MIN) {
        budgetDropped += 1;
        console.log(`  [${at.toFixed(1)}s] (over budget, `
          + `${spokenAt.length}/min) ${text.slice(0, 50)}`);
        return false;
      }
    }
    if (opts.droppable && at < speakingUntil) {
      cuesDropped += 1;
      console.log(`  [${at.toFixed(1)}s] (skipped, still speaking for `
        + `${(speakingUntil - at).toFixed(1)}s) ${text.slice(0, 60)}`);
      return false;
    }
    // Milestones may start before the previous line has finished; measure from
    // the later of the two so the budget never runs backwards.
    speakingUntil = Math.max(at, speakingUntil) + speechSeconds(text);
    spokenAt.push(at);
    if (opts.hush) hushUntil = Math.max(hushUntil, at + opts.hush);
    const pan = on && focus[on] != null ? focus[on] : undefined;
    cues.push({ at: Math.round(at * 1000) / 1000, text, holdMs,
                ...(pan == null ? {} : { pan }),
                words: text.split(/\s+/).length });
    console.log(`  [${at.toFixed(1)}s] ${text}`);
    if (CAPTIONS) page.evaluate((t) => window.__say(t), text).catch(() => {});
    return true;
  }

  /** Narrate, then wait until it has actually been SAID.
   *
   *  Every call site used to be `cue(text, N)` followed by `await sleep(N)`,
   *  where N was picked for how long the PICTURE needed and had nothing to do
   *  with how long the LINE takes to speak. The Limit tour line runs 28 seconds
   *  of speech and was held for 15, so the next line began 13 seconds early and
   *  everything after it inherited the overlap.
   *
   *  holdMs is a floor now, not the answer: wait the longer of what the picture
   *  needs and what the voice needs. */
  async function say(text, holdMs = 4000, on = null, opts = {}) {
    const spoke = cue(text, holdMs, on, opts);
    const need = spoke === false ? 0
      : Math.ceil(speechSeconds(text) * 1000) + SPEAK_GAP_MS;
    await sleep(Math.max(holdMs, need));
    return spoke;
  }

  /** Named vertical targets, in device pixels, filled in once the page has
   *  settled. Under TALL capture the compositor pans to these instead of the
   *  recorder scrolling to them. */
  // `top` is seeded rather than measured, because the opening layout mark is
  // written before the page has even loaded — measureFocus() cannot have run
  // yet, and an unset target silently drops `y`, which is how take 2's opening
  // beat ended up with no framing at all.
  const focus = { top: 0 };

  async function measureFocus() {
    // 'top' is the TOP OF THE PAGE, not the centre of any element: .page-card
    // wraps every panel, so its midpoint sits halfway down the document and
    // framing the empty-sessions beat there shows the wrong thing entirely.
    // 0 makes the compositor clamp the window to the top edge.
    for (const [name, sel] of [['pattern', '.template-row'],
                               ['timeline', '.vis-timeline'], ['chart', 'canvas']]) {
      const y = await page.evaluate(([s2, d]) => window.__centreOf(s2, 0, d), [sel, DPR]);
      if (y != null) focus[name] = y;
    }
    // Combined framings: centre the union so BOTH panels fit the window.
    const tl = await page.evaluate((d) => window.__boundsOf('.vis-timeline', 0, d), DPR);
    const bw = await page.evaluate((d) => window.__boundsOf('canvas', 0, d), DPR);
    const bufIdx = await page.evaluate(() => {
      // Find the buffer chart by its TITLE, not an index — the chart-stack
      // order in SessionDisplay is not the order they render in.
      const wraps = [...document.querySelectorAll('.canvas-wrap')];
      for (let i = 0; i < wraps.length; i++) {
        const head = wraps[i].parentElement && wraps[i].parentElement.innerText || '';
        if (/Buffer/i.test(head.split('\n')[0] || '')) return i;
      }
      return 1;
    });
    const buf = await page.evaluate(([i, d]) => window.__boundsOf('canvas', i, d), [bufIdx, DPR]);
    if (tl && bw) focus.state_chart = Math.round((tl.top + bw.bottom) / 2);
    if (bw && buf) focus.chart_buffer = Math.round((bw.top + buf.bottom) / 2);

    console.log('  focus targets (device px):',
      Object.entries(focus).map(([k, v]) => `${k}=${v}`).join(' '));
  }

  /** Record a layout change for render_layout.py. `on` names a focus target;
   *  it becomes the `y` the compositor centres its window on. */
  function lay(preset, extra = {}) {
    const at = Math.round(now() * 1000) / 1000;
    const { on, ...rest } = extra;
    const y = on && focus[on] != null ? focus[on] : undefined;
    layout.push({ at, preset, ...(y == null ? {} : { y }), ...rest });
    console.log(`  [${at.toFixed(1)}s] «layout ${preset}${on ? ` @${on}` : ''}»`);
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

  /** Scroll, unless the capture already holds the whole page. Leaving the
   *  scroll calls in place and neutering them here keeps one code path for
   *  both modes, rather than two that drift. */
  async function maybeScroll(sel, i = 0, block = 'center') {
    if (TALL) return;
    await page.evaluate(([s2, n, b]) => window.__scrollTo(s2, n, b), [sel, i, block]);
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
    await maybeScroll(sel, i, block);
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

  /** Zoom the charts' rolling window to roughly `minutes`, by the gesture the
   *  UI documents: Alt + wheel over the plot.
   *
   *  This one cannot be seeded. Folds, chart height and the sidebar all persist
   *  to localStorage, but `liveSpan` lives in useChartCoordination's in-memory
   *  per-player state, so the only way in is the gesture.
   *
   *  MEASURED, not counted. The zoom factor per wheel tick is not documented
   *  anywhere, so counting ticks would be guessing — and a take that quietly
   *  recorded an 8-minute window because a tick moved differently is the kind
   *  of failure you only notice in the edit. Read the axis back after each
   *  step, stop when close enough, and say what was actually achieved.
   *
   *  The wheel's sign is discovered the same way: step once, see which way the
   *  span moved, and flip if it went the wrong way. */
  async function setFocusWindow(minutes) {
    const target = minutes * 60 * 1000;
    const sel = `input[name="panel-focus-span"][value="${minutes}"]`;

    const span = () => page.evaluate(() => {
      const f = window.__chartByLabel('Limit (rate_mbps)') || window.__chartByLabel('Fetching Variant');
      return f ? f.ch.scales.x.max - f.ch.scales.x.min : null;
    });

    const before = await span();
    try {
      // 'attached', NOT the default 'visible': BitrateChartPanelToolbar styles
      // the radios `.pill input { display: none }` and lets the label carry the
      // appearance. Waiting for visibility times out on an element that is
      // present and perfectly clickable with force.
      await page.waitForSelector(sel, { timeout: 15000, state: 'attached' });
    } catch (e) {
      console.error(`  ⚠ no ${minutes}m window control — is the dashboard deployed?`);
      console.error(`    window stays at ${before == null ? '?' : (before / 60000).toFixed(1)} min`);
      return;
    }
    // A DOM click, not a synthetic mouse click. The radio is
    // `.pill input { display: none }`, so it has no box — and Playwright's
    // force: true skips the actionability CHECKS but still needs somewhere to
    // click. el.click() dispatches straight to the element and fires the
    // change handler Vue is listening for.
    const clicked = await page.evaluate((s2) => {
      const el = document.querySelector(s2);
      if (!el) return false;
      el.click();
      return true;
    }, sel);
    if (!clicked) { console.error(`  ⚠ ${minutes}m control vanished before the click`); return; }
    await sleep(1200);

    const after = await span();
    console.log(`  window ${before == null ? '?' : (before / 60000).toFixed(1)} min`
      + ` → ${after == null ? '?' : (after / 60000).toFixed(1)} min (wanted ${minutes})`);
    // Verify rather than assume the click landed — the whole point of moving
    // off the gesture was to stop guessing what the chart did.
    if (after == null || Math.abs(after - target) > target * 0.15) {
      console.error(`  ⚠ window did not take — wanted ${minutes} min`);
    }
  }

  /** Glide the cursor onto a named chart series in the legend, highlight it,
   *  and hold while its line is spoken.
   *
   *  By NAME, not coordinates: the legend reflows as series are added and as
   *  entries are struck through, so a remembered pixel offset points at the
   *  wrong series the moment anything changes. __legendRect resolves the box
   *  from Chart.js's own hit boxes, which is the same source the click path
   *  uses.
   *
   *  Returns false when the series is not on the legend at all — a tour line
   *  about a series nobody can see is worse than a missing line, so the caller
   *  skips rather than narrating into empty space. */
  async function tourSeries(label, sentence, holdMs = 8000) {
    const rect = await page.evaluate((t) => window.__legendRect(t), label);
    if (!rect) {
      console.error(`  ⚠ series "${label}" not on the legend — skipping its line`);
      return false;
    }
    const cx = Math.round(rect.x + rect.w / 2);
    const cy = Math.round(rect.y + rect.h / 2);

    // Drive the VISIBLE cursor there first so the move reads as deliberate,
    // then put the real pointer on the same spot to fire the chart's own
    // legend onHover — which bolds this series and dims the rest.
    await page.evaluate(([x, y]) => window.__moveCursor(x, y), [cx, cy]);
    await sleep(500);
    await page.mouse.move(cx, cy, { steps: 12 });
    await sleep(400);

    // Confirm the highlight actually engaged rather than assuming the hover
    // landed: the hovered series keeps its full border width while the others
    // are reduced, so a spread of widths means the app responded.
    const engaged = await page.evaluate((t) => {
      const f = window.__chartByLabel(t);
      if (!f) return false;
      const ws = (f.ch.data.datasets || [])
        .filter((d) => d.borderWidth != null)
        .map((d) => d.borderWidth);
      return new Set(ws).size > 1;
    }, label);
    if (!engaged) console.error(`  ⚠ legend hover did not highlight "${label}"`);

    // say(), not cue()+sleep(holdMs): the tour lines are the longest in the
    // take — the Limit one runs 28 seconds of speech against a 15-second hold,
    // so the next series started while it was still talking and every line
    // after it inherited the overlap.
    await say(sentence, holdMs);
    return true;
  }

  /** Move the pointer off the legend so the chart restores every series. */
  async function endTourHover() {
    await page.mouse.move(5, 5, { steps: 6 });
    await sleep(400);
  }

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
  lay('web-pip', { on: 'top' });   // focus.top is a constant 0, so this is safe pre-measure
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
  await say('Nothing is connected. The dashboard has no session to show.', 6000);
  await unspot();

  // Both sources in frame BEFORE playback starts. The session appearing in an
  // empty panel is the demo's first real beat, and it only reads if the phone
  // that caused it is on screen at the same time — otherwise the viewer sees a
  // row arrive in a list for no visible reason.
  lay('side-by-side', { on: 'top' });
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
  await say('There it is. The phone registered a session the moment it asked for the '
    + 'first playlist.', 6000);

  /* Name the panels before using them. Three are open; the rest are shut on
   * purpose, and saying so stops the folded ones reading as broken.
   *
   * Phrased as separate sentences rather than a list: narrate_sentences.py
   * paces on sentence boundaries, and pronounce.py turns an em-dash into a
   * comma, so a dashed list is read as one undifferentiated run. */
  const SAY_ORDER = ['network-shaping', 'player-state', 'bitrate-chart'];
  // Several names begin with "the", which reads wrong at the head of a
  // sentence; capitalise rather than dropping the article, since it is correct
  // mid-sentence elsewhere.
  const sentence = (t) => t.charAt(0).toUpperCase() + t.slice(1);
  const openParts = SAY_ORDER
    .filter((k) => FOLDS_OPEN.includes(k) && FOLD_SAY[k])
    .map((k) => sentence(`${FOLD_SAY[k][0]}, ${FOLD_SAY[k][1]}.`));
  if (openParts.length) {
    await say(`Three panels are open for this. ${openParts.join(' ')}`, 9000);
  }

  // These panels are not used today, but naming what each one DOES is the
  // part that lands: "Fault Injection, Content Manipulation, Server Timeouts"
  // alone means nothing to anyone who has not used the tool, and the whole
  // value of mentioning them is showing the tool does more than throttling.
  // Costs about ten seconds and buys the viewer a map of the rest.
  const shutParts = ['fault-injection', 'content-manipulation', 'server-timeouts']
    .filter((k) => !FOLDS_OPEN.includes(k) && FOLD_SAY[k])
    .map((k) => sentence(`${FOLD_SAY[k][0]} for ${FOLD_SAY[k][1]}.`));
  if (shutParts.length) {
    await say(`The others are folded on purpose. ${shutParts.join(' ')} `
      + `Each is a different way to break a stream, and none of them are in `
      + `play today. The only thing changing here is how much bandwidth there `
      + `is.`, 11000);
  }

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
  const audioKbpsMeasured = await audioKbps(rec0.current_play?.manifest?.master_url || '');
  if (audioKbpsMeasured) console.log(`  audio rendition ~${audioKbpsMeasured} kbps`);
  else console.error('  ⚠ could not measure the audio rendition — its line will omit the figure');

  /* The baseline cap every session gets before any pattern touches it.
   *
   * go-proxy applies INFINITE_STREAM_DEFAULT_RATE_MBPS to new sessions (#480).
   * The code default is 0 = uncapped, but the deploys set it: override-dev.yml
   * ships 100. So "no pattern running" does NOT mean "no shaping", and the
   * narration should not imply an unthrottled link.
   *
   * Read rather than hardcoded, for the same reason the audio bitrate is
   * measured — a figure quoted from a config file is a claim about a file, not
   * about the server that is actually on camera. This is read at preflight,
   * before the pattern is applied, which is the only moment it is observable. */
  const baselineMbps = rec0.shape?.rate_mbps ?? null;
  if (baselineMbps) console.log(`  baseline cap ${baselineMbps} Mbps (pre-pattern)`);
  else console.error('  ⚠ no baseline cap visible — the Limit line will omit it');

  console.log(`  device ${pm0.device_model} · ${variants.length} variants · `
    + `rung ${rungName(pm0.video_resolution)} · buffer ${pm0.buffer_depth_s}s`
    + ` · content ${pm0.content_name}`);

  // Assert we are recording the clip we think we are.
  //
  // ResumePlaybackClip falls back to the continue-watching hero when its
  // home-tile-<clip> does not render, and the hero resolves to the FEATURED
  // clip until the catalogue finishes loading. Both substitutions report
  // success. One of them already happened once: a run asked for
  // fpv5_p200_h264_6s and played bucks_bunny_p200_h264.
  //
  // That is the worst failure this recorder can have. Every number in the
  // narration — 12 variants, 234p to 2160p, the 47-step valley, the floor —
  // is computed from the requested clip's ladder, so a substitution produces a
  // take that describes one clip over footage of another, fluently and
  // wrongly. Cheaper to lose the take here than to find out in the edit.
  // Prefer the master URL: content_name is frequently absent (it was on takes 2
  // AND 3), and `CONTENT && pm0.content_name && …` then skips the check
  // silently — a guard that only fires when it feels like it is worse than none,
  // because it reads as verified. master_url always carries the content:
  //   go-live/fpv5_p200_h264_6s/master_6s.m3u8
  const masterUrl = rec0.current_play?.manifest?.master_url || '';
  const playing = pm0.content_name
    || (masterUrl.match(/go-live\/([^/]+)\//) || [])[1]
    || null;
  if (CONTENT && !playing) {
    console.error('\n⚠ cannot tell what is playing — neither content_name nor master_url.');
    console.error('  Recording anyway, but the narration is NOT verified against the clip.');
  }
  if (CONTENT && playing && playing !== CONTENT) {
    console.error(`\n✗ playing ${playing}, expected ${CONTENT}.`);
    console.error('  The tile tap fell back to the continue-watching hero. Either put the');
    console.error('  clip in the hero, or check the id with:');
    console.error('    go run ./cmd/demo-device -list-tiles   (in tests/characterization)');
    if (phone) phone.stop();
    await ctx.close(); await browser.close();
    process.exit(1);
  }
  if (pm0.buffer_depth_s && STEP_SECONDS < pm0.buffer_depth_s) {
    console.log(`  ⚠ step ${STEP_SECONDS}s < buffer ${pm0.buffer_depth_s}s — the DISPLAYED variant`);
    console.log('    will trail the cap continuously and never settle between steps.');
  }

  /* 5. Stage dressing -------------------------------------------------- */
  // Clear the plot. BandwidthChart appends a shaded
  // avg→peak band per variant with `hidden: false`, so twelve bands stripe the
  // plot area by default. Useful at a desk, noise at video size.
  for (const g of HIDE_LEGENDS) await hideLegend(g);
  if (FOCUS_MIN > 0) await setFocusWindow(FOCUS_MIN);
  await measureFocus();
  // ANNOTATE_TEST=1 exercises the annotation path against the live chart and
  // screenshots the result, without recording or applying anything. The circle
  // depends on Chart.js internals (legendHitBoxes, scales.getPixelForValue), so
  // it is the one part that cannot be verified by reading the code.
  if (process.env.ANNOTATE_TEST === '1') {
    await page.evaluate(() => window.__scrollTo('canvas'));
    await sleep(800);
    const HOLD = Number(process.env.ANNOTATE_WATCH_S || 12) * 1000 + 8000;
    /* Retry while the point is still under the live edge. A change that has
     * just happened needs ~16s to scroll far enough in for a whole circle to
     * fit; calling once and reporting waiting:true says nothing about whether
     * the wait ever resolves, which is the part worth testing. */
    const circleWhenReady = async (label, seed, colour) => {
      const stamp = Date.now();
      for (let i = 0; i < 40; i += 1) {
        const r = await page.evaluate(([l, t, sd, c, h]) =>
          window.__circleSeries(l, t, { seed: sd, color: c, holdMs: h }),
        [label, stamp, seed, colour, HOLD]);
        if (!r || !r.waiting) return { ...r, waitedS: i };
        if (i === 0) console.log(`  [circle] ${label}: waiting, ${r.shortBy}px short`);
        await sleep(1000);
      }
      return { gaveUp: true };
    };
    const a = await circleWhenReady('Fetching Variant', 7, undefined);
    const b = await circleWhenReady('Displayed Variant', 21, '#a855f7');
    console.log('  Fetching Variant  ->', JSON.stringify(a));
    console.log('  Displayed Variant ->', JSON.stringify(b));

    /* Is it on the right VALUE, not just the right time?
     *
     * X was easy to confirm by eye — the marker line stays threaded through the
     * circle as the chart scrolls. Y could be quietly wrong: getPixelForValue
     * returns a perfectly plausible pixel for the WRONG point if the
     * nearest-by-time search picked badly, and nothing in the picture would say
     * so. So read the chart canvas at the circle's centre and compare it with
     * the series' own colour. If the circle is on its line, the pixel under the
     * middle of it is that line. */
    for (const label of ['Fetching Variant', 'Displayed Variant']) {
      const v = await page.evaluate((lbl) => {
        const f = window.__chartByLabel(lbl);
        if (!f) return null;
        const { c, ch } = f;
        const ds = ch.data.datasets.find((d) => d.label === lbl);
        const pts = (ds.data || []).filter((q) => q && q.y != null);
        if (!pts.length) return null;
        const last = pts[pts.length - 1];
        const bx = last.x instanceof Date ? last.x.getTime() : Number(last.x);
        const px = Math.round(ch.scales.x.getPixelForValue(bx));
        const py = Math.round(ch.scales.y.getPixelForValue(last.y));
        // Sample a few pixels around the point: a 2px line will not always sit
        // exactly on the rounded coordinate.
        const g = c.getContext('2d');
        const dpr = window.devicePixelRatio || 1;
        let hit = null;
        for (let dy = -3; dy <= 3 && !hit; dy += 1) {
          const d = g.getImageData(Math.round(px * dpr),
                                   Math.round((py + dy) * dpr), 1, 1).data;
          if (d[3] > 40 && !(d[0] > 245 && d[1] > 245 && d[2] > 245)) {
            hit = [d[0], d[1], d[2]];
          }
        }
        return { want: ds.borderColor, got: hit, value: last.y, py };
      }, label);
      console.log(`  ${label} colour under centre: `
        + `${v ? JSON.stringify(v.got) : 'n/a'} vs series ${v ? v.want : '?'} `
        + `(value ${v ? v.value : '?'})`);
    }

    /* Does it TRACK? Placement was never the hard part — the old circles were
     * placed correctly and then sat still while the chart scrolled out from
     * under them. Sample the group's transform twice, some seconds apart: a
     * tracking annotation accumulates a negative x translation at exactly the
     * rate the plot is scrolling. Zero movement means it is stuck again. */
    const readShift = () => page.evaluate(() => {
      const g = document.querySelectorAll('#__pwscrib g');
      return Array.from(g).map((el) => {
        const m = /translate\(([-\d.]+),([-\d.]+)\)/.exec(el.getAttribute('transform') || '');
        return m ? { dx: Number(m[1]), dy: Number(m[2]) } : null;
      });
    });
    await sleep(1200);
    const t0shift = await readShift();
    const WATCH_S = Number(process.env.ANNOTATE_WATCH_S || 12);
    await sleep(WATCH_S * 1000);
    const t1shift = await readShift();
    console.log(`  groups at t0: ${t0shift.length}, after ${WATCH_S}s: ${t1shift.length}`);
    if (!t1shift.length) {
      console.log('    the annotations were GONE by the second sample — they '
        + 'expired before the watch finished, which says nothing about tracking');
    }
    console.log(`  after ${WATCH_S}s:`);
    t1shift.forEach((v, i) => {
      const was = t0shift[i];
      if (!v || !was) { console.log(`    [${i}] no transform — NOT tracking`); return; }
      const moved = v.dx - was.dx;
      console.log(`    [${i}] dx ${was.dx.toFixed(0)} -> ${v.dx.toFixed(0)} `
        + `(${moved.toFixed(0)}px, ${(moved / WATCH_S).toFixed(1)}px/s) `
        + (Math.abs(moved) > 2 ? 'TRACKING' : 'STUCK'));
    });
    await sleep(500);
    const shot = path.join(OUT, 'annotate-test.png');
    await page.screenshot({ path: shot });
    console.log('  wrote', shot);
    // Stop the device controller before returning. Without this the child
    // process keeps the event loop alive and the test never exits — it looked
    // like a hang in ctx.close(), which it was not.
    if (phone) { try { phone.stop(); } catch (e) { /* already gone */ } }
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
  lay('side-by-side', { on: 'state_chart' });

  await say(`A real iPhone, playing a live low-latency HLS stream. `
    + `${pm0.device_model}, ${pm0.player_tech} ${pm0.player_tech_version}.`, 6000);

  const asc = [...variants].sort((a, b) => a.bandwidth - b.bandwidth);
  await say(`The stream publishes ${variants.length} variants, from `
    + `${rungName(asc[0].resolution)} to ${rungName(asc[asc.length - 1].resolution)}.`, 6000);

  /* ---- Player State + Bandwidth together -------------------------------
   * The event and the thing that caused it, in one frame. Watching a variant
   * switch appear on the timeline while the line steps on the chart is the
   * pairing; either alone is half the story. */
  lay('side-by-side', { on: 'state_chart' });
  await spot('.vis-timeline', 0, 'center');
  await say(`Player State is the event timeline — every switch, stall and state `
    + `change the player reported. First frame at `
    + `${(pm0.first_frame_time_s ?? 0).toFixed(1)} seconds, `
    + `${plural(pm0.profile_shift_count || 0, 'variant switch')} so far.`, 8000);
  await unspot();

  await say(`Below it, the same moments as numbers. Watch them together: an event on `
    + `the timeline, a step on the chart.`, 7000);

  /* ---- the series, one at a time --------------------------------------- */
  lay('side-by-side', { on: 'state_chart' });
  await say('Five lines on the bandwidth chart are worth knowing by name.', 5500);

  await tourSeries('Limit (rate_mbps)',
    'The Limit is the network limit we set — enforced in the kernel on the '
    + 'proxy, not a suggestion to the player. Everything else on this chart is '
    + 'the player reacting to it.', 9000);

  /* The baseline is a second thought, so it gets a second line.
   *
   * These were one sentence, which ran to 380 characters: 28 seconds of speech
   * against a 15-second hold, and a three-line caption nobody reads to the end.
   * Two shorter lines say the same thing, and the pause between them is where
   * the first idea lands. */
  await say('And it never goes away. With no pattern running the server still '
    + 'holds every session'
    // Not fmtMbps(): that always keeps a decimal, and the baseline is a round
    // config value. "one hundred point zero megabits" is a mouthful for 100.
    + (baselineMbps
      ? ` to ${Number.isInteger(baselineMbps) ? baselineMbps : fmtMbps(baselineMbps)} megabits`
      : ' to a baseline network limit')
    + '. An unthrottled link on the same machine would flatter the player in '
    + 'ways no real viewer would ever see.', 8000);

  await tourSeries('Fetching Variant',
    'Fetching Variant is the rung the player is pulling right now. It moves '
    + 'first, because choosing a rung is the decision; everything else is '
    + 'consequence.');

  await tourSeries('Displayed Variant',
    'Displayed Variant is the rung actually on the screen. It lags the fetched '
    + 'one by roughly a buffer — the segments already downloaded have to play '
    + 'out before the new rung is seen.');

  const avgNet = await page.evaluate(() => window.__seriesLatest('Player avg_network_bitrate'));
  await tourSeries('Player avg_network_bitrate',
    'This one comes from the iPhone: what AVPlayer believes it is receiving'
    + (avgNet == null ? '' : `, currently ${fmtMbps(avgNet)} megabits`)
    + '. It is the player\'s own estimate, and it is not always right.');

  const shaper = await page.evaluate(() => window.__seriesLatest('mbps_shaper_avg'));
  await tourSeries('mbps_shaper_avg',
    'And this is the rate limiter\'s own count of what it actually pushed '
    + 'through'
    + (shaper == null ? '' : `, ${fmtMbps(shaper)} megabits`)
    + '. Server-side ground truth — the number to trust when the client and '
    + 'the network disagree.');

  await endTourHover();
  await say(`Right now nothing is constraining any of it — `
    + `${rungName(pm0.video_resolution)}, buffer `
    + `${(pm0.buffer_depth_s ?? 0).toFixed(0)} seconds.`, 6500);

  /* ---- Bandwidth + buffer/live offset ----------------------------------
   * What a cap COSTS. The buffer is where a squeeze shows up before the
   * picture does, so it belongs in frame with the cap that caused it. */
  lay('side-by-side', { on: 'chart_buffer' });
  await spot('canvas', 1, 'center');
  await say('Underneath: buffer depth and live offset. When the network limit bites, the '
    + 'buffer drains before the picture changes — this is where a squeeze '
    + 'shows up first.', 8000);
  await unspot();

  lay('web-pip', { on: 'pattern' });

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

  /* Where the pattern REVERSES — the step indices at which the limit stops
   * falling and starts rising, and vice versa.
   *
   * Needed to tell hunting from correct tracking. When the limit itself turns
   * around, the player turning around with it is the right behaviour, not
   * instability, and calling that "hunting" would be teaching the viewer
   * something false. For a valley that is the floor in the middle plus both
   * ends; derived from the rates rather than assumed, so a different template
   * gets its own turning points. */
  const turnSteps = (() => {
    if (rates.length < 3) return [];
    const turns = [0, rates.length - 1];
    for (let i = 1; i < rates.length - 1; i += 1) {
      const fallingBefore = rates[i] <= rates[i - 1];
      const fallingAfter = rates[i + 1] <= rates[i];
      if (fallingBefore !== fallingAfter) turns.push(i);
    }
    return turns;
  })();
  if (turnSteps.length) {
    console.log(`  pattern turns at step ${turnSteps.join(', ')} `
      + `(of ${rates.length}) — changes there are tracking, not hunting`);
  }
  const floor = rates.length ? Math.min(...rates) : null;

  await spot(`.template-row`, 0, 'nearest');
  await say(`Valley: hold the network limit above the top variant, walk it all the way down, `
    + `then walk it back up.`, 6000);
  await unspot();

  // Deliberately NOT spotlighting `.steps`. The generated table is 47 rows of
  // rates — accurate, and unreadable at video size. The same facts (count,
  // ceiling, floor) are read out here and shown compactly by `.applied-summary`
  // once the pattern is running.
  await say(`${stepCount} steps at ${STEP_SECONDS} seconds each — from `
    + `${fmtMbps(topCap)} megabits down to ${fmtMbps(floor)} and back.`, 6500);

  /* 7. Settle, then apply --------------------------------------------- */
  mark('settle');
  cue(`Before touching anything: the network limit is off, and the player is holding the `
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
  await say('Applying the pattern.', 2500);

  /* Tell the viewer what to watch for BEFORE it happens.
   *
   * There is a stretch here where the limit is stepping down and the player has
   * not reacted yet — the buffer is still full of segments fetched at the old
   * rate. Naming the order things will move in turns that gap from dead air
   * into anticipation, and it means the eye is on the right lines when the
   * first change finally lands. */
  await sleep(10000);
  await say('See how the limit is starting to step down. Watch the '
    + 'avg_network_bitrate follow it first — that is the player noticing. Then, '
    + 'a little later, the fetching variant will drop to a rung that fits, and '
    + 'later still the displayed variant will follow it onto the screen.',
    9000, 'chart');
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
  lay('side-by-side', { on: 'state_chart' });

  /* 8. Watch it happen -------------------------------------------------
   * Everything from here is generated from live readings. The recorder makes
   * no claim it did not just measure — the whole point of writing the cue at
   * the moment the value changed rather than scripting the prose in advance. */
  const started = Date.now();
  let lastStep = null, lastFetch = null, lastDisp = null;
  // Rung-change call-outs, counted per direction for the same reason as the
  // "on screen now" ones below.
  let limitDown = 0, limitUp = 0;
  // "On screen now" call-outs, counted separately per direction so a busy
  // descent cannot spend the whole budget and leave the recovery silent.
  let displayedDown = 0, displayedUp = 0;

  /* ---- notable ABR behaviour ------------------------------------------
   *
   * The valley makes the player do things worth pointing at, and they are all
   * derivable from the shift history the recorder already keeps. Detected live
   * so they can be narrated, which means each is recognised only once its
   * CONFIRMING shift arrives — an over-correction is not visible until the
   * correction happens. So the narration is phrased in the past tense.
   *
   * Thresholds are expressed in STEPS and in LADDER FRACTIONS rather than in
   * seconds and rung counts. An absolute "within 20 seconds" silently changes
   * meaning when STEP_SECONDS changes, and "3 rungs" means something different
   * on a 5-rung ladder than on this 12-rung one. */
  const notableSeen = { overshoot: 0, plunge: 0, hunting: 0 };
  // Set once the label stream connects. The recorder's own detectors keep
  // RECORDING to notable[] either way, but stop narrating — the server's
  // classification is broader and its thresholds are the configurable ones.
  let labelsLive = false;
  const notable = [];      // recorded even when not narrated, for later analysis
  let lastHuntAt = -1e9;

  /** Peak Mbps for each rung, from the ladder the stream actually published. */
  // Rungs in ladder order, lowest first — the last is the top of the ladder.
  const ladderRungs = variants.map((v) => rungName(v.resolution)).filter(Boolean)
    .sort((a, b) => idxOfRung(a) - idxOfRung(b));
  const rungPeak = {};
  for (const v of variants) {
    const n = rungName(v.resolution);
    if (n) rungPeak[n] = (v.bandwidth || v.average_bandwidth || 0) / 1e6;
  }

  function notice(hist, capNow) {
    if (hist.length < 2) return;
    const cur = hist[hist.length - 1];
    const prev = hist[hist.length - 2];
    const steps = (t) => t / Math.max(1, STEP_SECONDS);

    /* OVER-DOWNSHIFT — dropped further than it needed to, then took some back
     * while the limit had not moved. The ladder is what makes this a claim
     * rather than a guess: if the limit at the drop already covered the rung
     * it climbed back to, the lower rung was never necessary. */
    if (prev.dir === 'down' && cur.dir === 'up'
        && steps(cur.at - prev.at) <= 1.5) {
      const limitFlat = !prev.cap || cur.cap <= prev.cap * 1.1;
      const back = rungPeak[cur.to];
      const hadRoom = back != null && prev.cap != null && back <= prev.cap * 1.05;
      if (limitFlat && (hadRoom || back == null)) {
        notable.push({ kind: 'overshoot', at: cur.at, from: prev.from,
                       bottom: prev.to, back: cur.to, seconds: cur.at - prev.at });
        if (!labelsLive && notableSeen.overshoot < NOTABLE_MAX.overshoot) {
          notableSeen.overshoot += 1;
          cue(`That was an over-correction. It fell to ${prev.to}, then took `
            + `${cur.to} back ${(cur.at - prev.at).toFixed(0)} seconds later `
            + `with the limit unchanged — so ${prev.to} was further than it `
            + `needed to go. ABR judges by recent throughput, and a limit `
            + `falling mid-segment reads worse than it turns out to be.`, 9000);
        }
      }
    }

    /* MULTI-RUNG PLUNGE — skipped a quarter of the ladder in one move. */
    const span = idxOfRung(cur.from) - idxOfRung(cur.to);
    const ladder = Object.keys(rungPeak).length || 12;
    if (cur.dir === 'down' && idxOfRung(cur.from) > 0
        && hist.filter((h) => h.dir === 'down').length
        && span > 0) {
      const skipped = variants.length
        ? variants.filter((v) => {
          const n = idxOfRung(rungName(v.resolution));
          return n > idxOfRung(cur.to) && n < idxOfRung(cur.from);
        }).length + 1
        : 1;
      if (skipped >= Math.max(2, Math.ceil(ladder * 0.25))) {
        notable.push({ kind: 'plunge', at: cur.at, from: cur.from,
                       to: cur.to, rungs: skipped });
        if (!labelsLive && notableSeen.plunge < NOTABLE_MAX.plunge) {
          notableSeen.plunge += 1;
          cue(`It just skipped ${skipped} rungs in one move, ${cur.from} `
            + `straight to ${cur.to}. That is what a player does when the `
            + `limit drops faster than the buffer can absorb — it stops `
            + `stepping down and jumps, because arriving late costs a stall `
            + `and arriving small only costs detail.`, 9000);
        }
      }
    }

    /* HUNTING — three alternating changes inside a couple of steps, meaning
     * the limit is sitting on the boundary between two rungs and neither is
     * stable. Merged: overlapping windows are one event, not three. */
    if (hist.length >= 3) {
      const w = hist.slice(-3);
      const alternating = w[0].dir !== w[1].dir && w[1].dir !== w[2].dir;
      /* Not hunting if the PATTERN is turning around underneath it. At the top
       * and the bottom of the valley the limit reverses, and a player that
       * reverses with it is tracking correctly — the oscillation is ours, not
       * its. Only flag changes of direction while the limit holds its own. */
      const nearTurn = turnSteps.length && w.some((x) => x.step != null
        && turnSteps.some((t) => Math.abs(x.step - t) <= 2));
      if (alternating && !nearTurn && steps(w[2].at - w[0].at) <= 2.5
          && w[0].at - lastHuntAt > STEP_SECONDS * 4) {
        lastHuntAt = w[0].at;
        notable.push({ kind: 'hunting', at: cur.at,
                       path: w.map((x) => `${x.from}>${x.to}`).join(' ') });
        if (!labelsLive && notableSeen.hunting < NOTABLE_MAX.hunting) {
          notableSeen.hunting += 1;
          cue(`Watch it hunt — three changes in `
            + `${(w[2].at - w[0].at).toFixed(0)} seconds. The limit is sitting `
            + `right on the boundary between two rungs, so neither one is `
            + `stable and the player keeps changing its mind.`, 9000);
        }
      }
    }
  }
  let troughDone = false, recovering = false;
  // The floor ASIDE is one-time, even though the floor BEAT repeats.
  let troughExplained = false;
  // The legend revisit fires once, at the first valley floor.
  let secondTourDone = false;

  /* ---- narration scheduled from the server's labels --------------------
   *
   * Three separate problems, deliberately three separate mechanisms:
   *
   * WAIT FOR CONTEXT (`after`). A label fires the moment its condition is met,
   * when the chart shows one small kink and nothing a viewer could point at.
   * Held until the shape has drawn itself, then narrated in the past tense.
   *
   * COOL DOWN PER TYPE (`every`). Take 8 had five overshoots and six plunges.
   * The first teaches something; the fifth is noise. Per-type, so a stall never
   * silences an overshoot — they are different observations.
   *
   * PICK ONE WHEN THEY COLLIDE. Labels arrive in clusters — a single sample can
   * carry shift_down and qoe_abr_conservative together, and a bad moment
   * produces several within a second or two. Narrating all of them talks over
   * the picture and over itself. Rank by the server's own severity first, then
   * by the table's `rank`, and speak only the winner; the losers are recorded. */
  // Freshest sample, for the #3 re-check at speaking time.
  let lastMetrics = {}, lastShape = {};
  const labelSpokenAt = {};     // label -> take-seconds when last narrated
  const labelQueue = [];        // { name, sev, at, prio, say }
  const SEV_RANK = { error: 40, critical: 30, warning: 20, info: 10, testing: 0 };
  // Two narrations closer together than this are a collision, not a sequence.
  const LABEL_COLLIDE_S = Number(process.env.LABEL_COLLIDE_S || 8);
  // How long a label may wait for a gap in the narration before giving up.
  const LABEL_MAX_DEFER_S = Number(process.env.LABEL_MAX_DEFER_S || 45);

  function queueLabel(sev, name, tsMs) {
    const spec = LABEL_NARRATION[name];
    notable.push({ kind: 'label', at: (tsMs - t0) / 1000, label: name, sev });
    if (!spec) return;                       // recorded, never spoken
    const at = (tsMs - t0) / 1000 + spec.after;
    const prio = (SEV_RANK[sev] || 0) + (spec.rank || 0);

    /* Collision: keep the better of the two rather than queueing both. */
    const clash = labelQueue.findIndex((q) => Math.abs(q.at - at) <= LABEL_COLLIDE_S);
    if (clash >= 0) {
      if (labelQueue[clash].prio >= prio) {
        console.log(`  [label] ${name} yields to ${labelQueue[clash].name}`);
        return;
      }
      console.log(`  [label] ${name} outranks ${labelQueue[clash].name}`);
      labelQueue.splice(clash, 1);
    }
    labelQueue.push({ name, sev, at, prio, say: spec.say,
                      every: LABEL_COOLDOWN_S || spec.every });
  }

  /** Drained by the poll loop. Cooldown is checked HERE, at speaking time, not
   *  at queue time — a label queued during another one's cooldown may still be
   *  outside it by the time its context delay has elapsed. */
  function drainLabels() {
    for (let i = labelQueue.length - 1; i >= 0; i -= 1) {
      const q = labelQueue[i];
      if (now() < q.at) continue;
      labelQueue.splice(i, 1);
      /* Still talking? Come back to it. The line was held for its context
       * delay so that it would land on a visible shape; saying it over the top
       * of the previous line loses both. Deferred, not dropped — but not
       * forever: past LABEL_MAX_DEFER_S the shape it describes has scrolled
       * out of the window and it is no longer worth saying. */
      if (now() < speakingUntil) {
        if (now() - q.at > LABEL_MAX_DEFER_S) {
          console.log(`  [label] ${q.name} abandoned — waited `
            + `${(now() - q.at).toFixed(0)}s for a gap and never got one`);
          continue;
        }
        labelQueue.push(q);
        continue;
      }
      const last = labelSpokenAt[q.name];
      if (last != null && now() - last < q.every) {
        console.log(`  [label] ${q.name} suppressed — `
          + `${(now() - last).toFixed(0)}s since the last one, needs ${q.every}s`);
        continue;
      }
      /* #3 — is it still true?
       *
       * The whole point of holding a label for context is that time passes,
       * and in that time the situation can reverse. A human would not announce
       * "it isn't taking the headroom" about a gap that closed ten seconds ago.
       * Ongoing CONDITIONS get re-checked here; point-in-time EVENTS (a stall,
       * an overshoot, a startup breach) have no predicate because they
       * happened, and happening is not undone by what came after. */
      const spec = LABEL_NARRATION[q.name];
      if (spec && spec.stillTrue && !spec.stillTrue(lastMetrics, lastShape)) {
        console.log(`  [label] ${q.name} dropped — no longer true by the time `
          + 'there was context for it');
        continue;
      }
      labelSpokenAt[q.name] = now();
      cue(q.say, 9000, null, { budgeted: true, critical: q.sev === 'critical' });
    }
  }

  /* Subscribe to the server's own classification, scoped to THIS play.
   *
   * Deliberately HERE and not up beside the other preflight reads: queueLabel
   * closes over labelQueue and labelSpokenAt, and labelsLive is a `let` — all
   * declared just above. Starting the stream before those bindings exist meant
   * a ReferenceError the moment the first label arrived.
   *
   * t0 is the cutoff: connecting replays this play's backfill, and narrating
   * minutes-old rows would fire every label at once, in the past.
   *
   * Failure is not fatal — the analytics sidecar is optional and the live path
   * does not depend on it, so a take without it falls back to the recorder's
   * own narrower detectors. */
  if (LABELS) {
    labelsLive = await subscribeLabels(
      pid, rec0.current_play?.id, t0, queueLabel);
  }
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
    drainLabels();
    while (pending.length && pending[0].at <= now()) {
      const a = pending.shift();
      try {
        if (a.kind === 'chart') {
          await page.evaluate(() => window.__scrollTo('canvas'));
          await sleep(400);
          const hit = await page.evaluate(
            ([label, t, s, col]) => window.__circleSeries(label, t, { seed: s, color: col }),
            [a.series, a.tMs, a.seed, a.color]);
          if (hit) {
            console.log(`  [circle] ${a.series}: `
              + (hit.waiting ? `waiting, ${hit.shortBy}px short of clearing the edge`
                : hit.gone ? 'scrolled off'
                : `drawn at ${Math.round(hit.x)},${Math.round(hit.y)} value ${hit.value}`));
          } else {
            console.log(`  [circle] ${a.series}: no point found`);
          }
          if (hit && hit.waiting) {
            /* Still under the live edge. Come back in a second — it is
             * scrolling inward at a few pixels a second and there is no point
             * drawing a circle whose right half falls off the plot. */
            const tries = (a.tries || 0) + 1;
            if (tries <= 40) {
              pending.unshift({ ...a, at: now() + 1, tries });
            } else {
              console.error(`  ⚠ "${a.series}" never came far enough onto the `
                + `chart to circle (short by ${hit.shortBy}px)`);
            }
            break;
          }
          if (hit && hit.gone) {
            console.error(`  ⚠ "${a.series}" scrolled off before it could be circled`);
          } else if (!hit) {
            console.error(`  ⚠ no "${a.series}" point to circle`);
          }
        } else if (a.kind === 'timeline') {
          await page.evaluate(() => window.__scrollTo('.vis-timeline'));
          await sleep(400);
          await page.evaluate((s) => window.__circleNewestEvent({ seed: s }), a.seed);
        } else if (a.kind === 'tour2') {
          /* Revisit the legend once the valley has actually been DRAWN.
           *
           * The first tour happens before the pattern runs, when every series
           * is a flat line and the names are abstract. By the floor the chart
           * has a shape worth pointing at: the limit's V, the fetched rung
           * tracking it down, and the displayed rung trailing both. Same five
           * names, but now they mean something on screen.
           *
           * Shorter holds than the opening tour — this is recognition, not
           * introduction — and only the three series that make the shape. */
          lay('side-by-side', { on: 'chart' });
          await sleep(1200);
          await tourSeries('Limit (rate_mbps)',
            'There is the whole experiment in one line — the network limit '
            + 'walked all the way down and is starting back up. Everything '
            + 'else on this chart is a reaction to that V.', 7000);
          await tourSeries('Fetching Variant',
            'Fetching Variant hugs it on the way down. It sheds quality fast, '
            + 'because falling behind the limit costs a stall and dropping a '
            + 'rung only costs detail.', 7000);
          await tourSeries('Displayed Variant',
            'And Displayed Variant trails the whole way. The gap between these '
            + 'two lines IS the buffer — widest where the limit fell fastest.', 7000);
          await endTourHover();
        } else if (a.kind === 'say') {
          // Queued on fixed offsets, which cannot know how long the previous
          // line runs. Re-queue a beat later rather than speak over it.
          if (now() < speakingUntil) {
            pending.unshift({ ...a, at: speakingUntil + 0.3 });
            break;
          }
          cue(a.text, 8000);
          lastCue = Date.now();
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
    lastMetrics = m;
    lastShape = sh;
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
        /* Did it actually get back to the top? Take 8's second cycle ended on
         * 1800p and the narration claimed recovery anyway, which is the one
         * thing a demo about ABR behaviour must not do. The climb sits right on
         * the boundary at these step lengths, so both outcomes are real and
         * worth saying out loud. */
        const topRung = ladderRungs[ladderRungs.length - 1];
        const reclaimed = topRung && rungName(m.fetching_resolution) === topRung;
        cue(reclaimed
          ? `Cycle ${cycle} done — the limit is back at the top and the player `
            + `took the whole ladder back, all the way to ${topRung}.`
            + (cycle < CYCLES ? ' Going round again.' : '')
          : `Cycle ${cycle} done. The limit is back at the top, but the player `
            + `only climbed as far as ${rungName(m.fetching_resolution)} before `
            + `the cycle turned — it ran out of headroom-time, not headroom. `
            + `Coming up the ladder is slower than going down it.`
            + (cycle < CYCLES ? ' Going round again.' : ''), 8000);
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
      shifts.push({ at: now(), from, to, dir: down ? 'down' : 'up', cap, step, res: m.fetching_resolution });
      notice(shifts, cap);
      /* Terse, droppable, AND capped per direction.
       *
       * The overrun guard only drops what there is no TIME to say; it has no
       * opinion about whether the twenty-first rung change is worth saying at
       * all. Take 8 still called out 30 of its 39 changes. A few on the way
       * down and a few on the way back up is enough to establish that the
       * player is tracking the limit — after that the chart shows it better
       * than a voice can, and the notable-behaviour detectors cover anything
       * genuinely surprising.
       *
       * Per direction, so a busy descent cannot spend the whole budget and
       * leave the recovery silent. */
      const limitSeen = down ? limitDown : limitUp;
      if (limitSeen < LIMIT_MAX) {
        if (down) limitDown += 1; else limitUp += 1;
        cue(`Network limit ${fmtMbps(cap)}. ${down ? 'Down' : 'Up'} to ${to}.`,
          5000, null, { droppable: true });
      }
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
        /* A backstop, not the mechanism — drawings expire on their own now.
         * At +8.5s this used to fire while the circle was still waiting for its
         * point to come onto the chart, deleting it before it existed. */
        pending.push({ at: t + 40, kind: 'clear' });
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
      /* Call out only a FEW of these, and only near the start of each
       * direction.
       *
       * The lag between fetching a rung and seeing it is worth explaining —
       * it is the buffer, made visible. But it is one idea, and take 7 said it
       * 27 times. Repeated that often it stops being an explanation and
       * becomes a tic, and it crowds out the narration that is actually
       * tracking the valley.
       *
       * So: a few on the way down, a few on the way back up, counted per
       * direction rather than overall — otherwise a busy descent spends the
       * whole budget and the recovery, which is the more interesting half,
       * gets nothing. The first of each direction carries the full
       * explanation; the rest are just the numbers. */
      const dropping = idxOfRung(to) < idxOfRung(from);
      const seen = dropping ? displayedDown : displayedUp;
      if (seen < DISPLAYED_MAX) {
        if (dropping) displayedDown += 1; else displayedUp += 1;
        cue(`On screen now: ${from} to ${to}`
          + (lag == null ? '.'
            : seen === 0 ? `, ${lag.toFixed(0)} seconds after it started fetching `
              + `that rung — that gap is the buffer draining.`
              : `, ${lag.toFixed(0)} seconds behind.`),
          5000, null, { droppable: true });
      }
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
      lay('phone-full', { on: 'chart' });
      cue(`Bottom of the valley — ${fmtMbps(cap)} megabits. Buffer `
        + `${(m.buffer_depth_s ?? 0).toFixed(0)} seconds, `
        + `${plural((m.stalling_count || 0) - stall0, 'stall')}, `
        + `${plural((m.buffering_count || 0) - rebuf0, 'rebuffer')} so far.`, 8000);
      lastCue = Date.now();
      // Queued rather than awaited: the poll loop must keep running or it
      // misses transitions. These land as their own cues a beat apart.
      /* The floor aside — why the picture looks like that, where the bitrate
       * went, what we are actually here to watch — is a ONE-TIME explanation.
       * troughDone re-arms every cycle so the trough beat itself repeats, which
       * is right; this does not. Take 8 said all three of these twice. */
      if (!troughExplained) {
        troughExplained = true;
      pending.push({ at: now() + 8.5, kind: 'say', text:
        'And the picture is not really acceptable here — nobody would ship '
        + 'this. That is the point of the floor, not a flaw in it.' });
      pending.push({ at: now() + 17.0, kind: 'say', text:
        'Some of that is our own doing. Audio is a separate rendition, the '
        + 'same on the bottom rung as the top'
        + (audioKbpsMeasured ? `, about ${audioKbpsMeasured} kilobits either way` : '')
        + '. Down here that is most of the budget, and the picture gets what '
        + 'is left.' });
      pending.push({ at: now() + 26.0, kind: 'say', text:
        'HE-AAC would carry it in a third of that and hand the difference to '
        + 'the picture; licensing is why we are not using it. Either way, what '
        + 'we came to watch is the BEHAVIOUR, not how good 234p can look.' });
      }
      /* Only on the FIRST valley. By the second the audience has seen the
       * shape, and pointing at it again would be padding. */
      if (!secondTourDone) {
        secondTourDone = true;
        pending.push({ at: now() + 36.0, kind: 'tour2' });
      }
    }

    /* Recovery begins — cap rising again past the floor. */
    if (troughDone && !recovering && cap != null && cap > minCapSeen * 1.5) {
      recovering = true;
      mark('recovery');
      lay('side-by-side', { on: 'state_chart' });
      cue('The network limit starts climbing back. Now the question is how fast the player '
        + 'trusts the extra headroom.', 6000, null, { hush: 25 });
      lastCue = Date.now();
    }

    /* Periodic keep-alive so long plateaus are not silent, but not chatty. */
    if (Date.now() - lastCue > 45000 && step != null) {
      cue(`Step ${step} — network limit ${fmtMbps(cap)}, fetching ${rungName(m.fetching_resolution)}, `
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
  lay('web-pip', { on: 'chart' });
  const fin = await api(`/api/v2/players/${pid}`);
  const fm = fin.current_play?.player_metrics || fin.player_metrics || {};
  const downs = shifts.filter((s) => s.dir === 'down').length;
  const ups = shifts.filter((s) => s.dir === 'up').length;
  // A take cut short by RUN_TIMEOUT_MS never completes a cycle, and "Over 0
  // cycles" is a sentence no one should have to hear.
  await say(`${cycle ? `Over ${plural(cycle, 'cycle')}: ` : 'Partway through the first cycle: '}`
    + `${plural(shifts.length, 'variant change')} — ${downs} down, ${ups} back up. `
    + `${plural((fm.stalling_count || 0) - stall0, 'stall')} and `
    + `${plural((fm.buffering_count || 0) - rebuf0, 'rebuffer')} across the whole valley.`, 9000);

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
    /* The ladder itself, not just how many rungs it has.
     *
     * `variants` above is a COUNT, which is enough for narration ("12 variants")
     * and useless for analysis afterwards: anything ladder-relative — was there
     * room for the rung it abandoned, how much of the ladder did that drop skip
     * — needs the peaks, and reconstructing them means re-fetching a manifest
     * that may since have changed. Cheap to record, expensive to recover. */
    ladder: variants.map((v) => ({
      rung: rungName(v.resolution),
      resolution: v.resolution,
      peak_mbps: Math.round((v.bandwidth || 0) / 1e4) / 100,
      avg_mbps: Math.round((v.average_bandwidth || 0) / 1e4) / 100,
    })).filter((v) => v.rung).sort((a, b) => idxOfRung(a.rung) - idxOfRung(b.rung)),
    notable,
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
  if (notable.length) {
    const by = notable.reduce((a, n) => ({ ...a, [n.kind]: (a[n.kind] || 0) + 1 }), {});
    console.log(`  notable: ${Object.entries(by).map(([k, v]) => `${k} x${v}`).join(', ')}`
      + ` (narrated up to ${Object.values(NOTABLE_MAX).join('/')})`);
  }
  // Say what was left out. A silent drop reads as "nothing happened there"
  // when reviewing a take, which is the one thing it must not look like.
  if (hushDropped || budgetDropped) {
    console.log(`  quiet: ${hushDropped} held back over a payoff, `
      + `${budgetDropped} over the ${NARRATION_PER_MIN}/min budget`);
  }
  if (cuesDropped) {
    console.log(`  ${cuesDropped} play-by-play cue(s) skipped — no room to say `
      + `them before the next one. Raise SPEAK_CPS if the voice is faster than `
      + `${SPEAK_CPS} chars/sec.`);
  }
  console.log(`browser: ${videoPath}`);
  if (phonePath) console.log(`phone:   ${phonePath}`);
  // Tear the Appium session down explicitly. Leaving it open wedges the next
  // run with "create session deadline exceeded", which reads like a device
  // fault rather than a leaked session from the run before.
  if (phone) phone.stop();

  console.log('\nNext: set DEMO_DIR and run  python3 narrator_app.py');
})().catch((e) => { console.error(e); process.exit(1); });
