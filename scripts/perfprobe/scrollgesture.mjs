// Input-gesture scroll on the pane timeline: probe scrollgesture [seconds=15] [index=0] [selector] [osc|down]
// osc oscillates ±1500px and parks the pane above the bottom-follow band
// (odd leg count ends upward); down rides to the bottom, which is the one
// way to RE-ENGAGE bottom-follow from outside (programmatic writes are not
// reader intent in either direction).
// Drives Input.synthesizeScrollGesture (mouse-wheel source) up and down over
// the first matching scroller, so the scroll is a real input gesture instead
// of a JS scrollTop write — the A/B partner for the spring's per-frame write.
// Run `probe frames N` concurrently to count Layerize/Commit under each mode.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

const SECS = +(process.argv[2] || 15);
const IDX = +(process.argv[3] || 0);
const SEL = process.argv[4] || '[data-testid="message-timeline-scroll"]';

const read = async (page) => JSON.parse(await evaluate(page, `JSON.stringify((() => {
  const el = document.querySelectorAll(${JSON.stringify(SEL)})[${IDX}];
  if (!el) return null;
  const r = el.getBoundingClientRect();
  return { x: r.x + r.width / 2, y: r.y + r.height / 2, scrollTop: el.scrollTop,
           range: el.scrollHeight - el.clientHeight, winY: window.scrollY || document.documentElement.scrollTop };
})())`));

const page = await connectPage();
try {
  const start = await read(page);
  if (!start) { console.error(`probe: no element matches ${SEL}`); process.exit(1); }
  console.log(`scroller ${SEL}: scrollTop=${start.scrollTop.toFixed(0)} range=${start.range.toFixed(0)} center=(${start.x.toFixed(0)},${start.y.toFixed(0)})`);
  if (start.range < 500) { console.error('probe: scroller has under 500px of range, nothing to drive'); process.exit(1); }
  const t0 = Date.now();
  const MODE = process.argv[5] || 'osc'; // osc | down (down = ride to the bottom and re-engage follow)
  let dir = MODE === 'down' ? 1 : -1; // osc leads upward (yDistance > 0), so an odd leg count parks the pane AWAY from the bottom-follow re-engage band
  let legs = 0;
  while (Date.now() - t0 < SECS * 1000) {
    const dist = Math.min(1500, start.range - 100);
    await page.send('Input.synthesizeScrollGesture', {
      x: start.x, y: start.y, yDistance: -dir * dist, speed: 1200, gestureSourceType: 'mouse',
    });
    if (MODE !== 'down') dir = -dir;
    legs++;
    await sleep(50);
  }
  const end = await read(page);
  console.log(`${legs} gesture legs over ${((Date.now() - t0) / 1000).toFixed(1)}s; scrollTop ${start.scrollTop.toFixed(0)} -> ${end.scrollTop.toFixed(0)}, window.scrollY ${start.winY} -> ${end.winY}`);
  if (end.scrollTop === start.scrollTop && end.winY !== start.winY) {
    console.error('probe: the gesture scrolled the APP ROOT, not the pane scroller — reposition or fix the selector');
  }
} finally {
  await done([page]);
}
