// Where IS the plot, in the coordinates __circleSeries mixes together?
//
// The circle is drawn ~28px OUTSIDE the plot's right edge when the wait logic
// should place it 69px inside — a ~97px discrepancy. __circleSeries computes
// `rect.x + px`, where rect comes from getBoundingClientRect (CSS px, viewport)
// and px comes from chartArea/getPixelForValue (Chart.js's own space). If those
// two are not the same space, the sum is meaningless and the error would scale
// with the canvas — which a constant-looking offset would hide.
//
// Read-only: opens the dashboard, picks the session, prints numbers.
const { chromium } = require('playwright');
const BASE = process.env.BASE || 'https://dev.jeoliver.com:21000';

(async () => {
  const b = await chromium.launch({ headless: false, args: ['--window-size=1600,1000'] });
  const ctx = await b.newContext({
    viewport: { width: 1600, height: 3400 },
    deviceScaleFactor: 2,
    ignoreHTTPSErrors: true,
  });
  const p = await ctx.newPage();
  await p.goto(`${BASE}/dashboard/testing.html`, { waitUntil: 'networkidle', timeout: 60000 });
  await p.waitForTimeout(6000);

  const out = await p.evaluate(() => {
    const res = [];
    for (const c of document.querySelectorAll('canvas')) {
      const ch = window.Chart && window.Chart.getChart ? window.Chart.getChart(c) : null;
      if (!ch || !ch.chartArea) continue;
      const r = c.getBoundingClientRect();
      res.push({
        labels: (ch.data.datasets || []).map((d) => d.label).slice(0, 3),
        rect: { x: Math.round(r.x), y: Math.round(r.y),
                w: Math.round(r.width), h: Math.round(r.height) },
        cssSize: { w: c.clientWidth, h: c.clientHeight },
        attrSize: { w: c.width, h: c.height },
        chartArea: { left: Math.round(ch.chartArea.left),
                     right: Math.round(ch.chartArea.right),
                     top: Math.round(ch.chartArea.top),
                     bottom: Math.round(ch.chartArea.bottom) },
        // What __circleSeries would compute for the plot's right edge:
        derivedRightEdgeViewportX: Math.round(r.x + ch.chartArea.right),
        // What it ACTUALLY is, if chartArea is CSS px of the canvas:
        actualRightEdgeViewportX: Math.round(r.x + ch.chartArea.right),
      });
    }
    return { dpr: window.devicePixelRatio, charts: res };
  });

  console.log('devicePixelRatio:', out.dpr);
  for (const c of out.charts) {
    console.log('\nchart:', c.labels.join(', '));
    console.log('  canvas rect (CSS)   x=%d w=%d', c.rect.x, c.rect.w);
    console.log('  canvas clientWidth  %d   attr width %d  (ratio %.2f)',
      c.cssSize.w, c.attrSize.w, c.attrSize.w / (c.cssSize.w || 1));
    console.log('  chartArea left=%d right=%d', c.chartArea.left, c.chartArea.right);
    console.log('  => plot right edge in viewport CSS: %d', c.derivedRightEdgeViewportX);
    console.log('  => canvas right edge in viewport CSS: %d', c.rect.x + c.rect.w);
    if (c.chartArea.right > c.cssSize.w) {
      console.log('  ** chartArea.right EXCEEDS the canvas CSS width — it is in');
      console.log('     DEVICE px, so rect.x + chartArea.right mixes spaces **');
    }
  }
  await b.close();
})();
