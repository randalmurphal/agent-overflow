// Checkerboard hunt: park a pane deep in history with a violent input fling,
// then take the instant jump-to-bottom path and screenshot the frames right
// after the teleport, when the destination tiles may not be rastered yet.
// usage: probe jumpbottom [index=0] [label=jump]
// Clicks [data-testid="nav-rail-jump-latest"] when the rail renders it
// (the real user affordance, scrollToItem -> instant scrollToIndex); falls
// back to an instant scrollTop write to the bottom (same compositor story:
// one-frame teleport into unrasterized territory; the write is not reader
// intent so follow stays disengaged either way).
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

const IDX = +(process.argv[2] || 0);
const LABEL = process.argv[3] || 'jump';
const SEL = '[data-testid="message-timeline-scroll"]';

const read = async (page) => JSON.parse(await evaluate(page, `JSON.stringify((() => {
  const el = document.querySelectorAll(${JSON.stringify(SEL)})[${IDX}];
  if (!el) return null;
  const r = el.getBoundingClientRect();
  return { x: r.x + r.width / 2, y: r.y + r.height / 2, scrollTop: el.scrollTop,
           range: el.scrollHeight - el.clientHeight };
})())`));

const page = await connectPage();
try {
  await page.send('Page.enable').catch(() => {});
  const start = await read(page);
  if (!start) { console.error(`probe: no element matches ${SEL}[${IDX}]`); process.exit(1); }
  if (start.range < 4000) { console.error(`probe: only ${start.range.toFixed(0)}px of range, too short for a deep jump`); process.exit(1); }

  // Park deep: two hard upward flings (positive yDistance scrolls up).
  for (let i = 0; i < 2; i++) {
    await page.send('Input.synthesizeScrollGesture', {
      x: start.x, y: start.y, yDistance: Math.min(6000, start.range - 100), speed: 12000, gestureSourceType: 'mouse',
    });
  }
  await sleep(500); // let raster catch up at the parked position
  const parked = await read(page);
  console.log(`parked: scrollTop ${start.scrollTop.toFixed(0)} -> ${parked.scrollTop.toFixed(0)} (range ${parked.range.toFixed(0)})`);

  // The teleport.
  const how = await evaluate(page, `(() => {
    const btn = document.querySelectorAll('[data-testid="nav-rail-jump-latest"]')[${IDX}];
    if (btn) { btn.click(); return 'nav-rail-jump-latest click'; }
    const el = document.querySelectorAll(${JSON.stringify(SEL)})[${IDX}];
    el.scrollTop = el.scrollHeight;
    return 'scrollTop teleport (no jump arrow rendered)';
  })()`);
  console.log(`jump via ${how}`);

  const outDir = process.env.AO_PERFPROBE_OUT || process.env.TEMP || '.';
  for (const at of [60, 160, 400]) {
    await sleep(at === 60 ? 60 : at === 160 ? 100 : 240);
    const shot = await page.send('Page.captureScreenshot', { format: 'png', fromSurface: true });
    const out = join(outDir, `ao-${LABEL}-t${at}.png`);
    writeFileSync(out, Buffer.from(shot.data, 'base64'));
    console.log(`saved ${out}`);
  }
  const end = await read(page);
  console.log(`landed: scrollTop ${end.scrollTop.toFixed(0)} of range ${end.range.toFixed(0)}`);
} finally {
  await done([page]);
}
