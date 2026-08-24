// A/B/A experiment: measure Oilpan garbage with and without a CSS override, optionally while scrolling.
// usage: probe ab --css '<css>' [--secs 30] [--scroll] [--allow-user-app]
import { connectBrowser, connectPage, done, evaluate, sleep, PORT } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, isRenderer, blinkClassRows } from './lib/memdump.mjs';
import { fail } from './lib/format.mjs';
import { CHAT_SCROLLER_SELECTOR } from './lib/dom.mjs';

const args = process.argv.slice(2);
const idx = args.indexOf('--css');
const CSS = idx >= 0 ? args[idx + 1] : null;
const sidx = args.indexOf('--secs');
const SECS = sidx >= 0 ? +args[sidx + 1] : 30;
const SCROLL = args.includes('--scroll');
if (!CSS) fail("probe: ab needs --css '<css>' (the override to test against)");
// Same guard as the wrapper: this probe is visible to whoever is using the app.
if (PORT === '9223' && !args.includes('--allow-user-app')) {
  fail('probe: ab injects CSS and can drive synthetic scrolling, so it changes what the user sees on port 9223.\n'
    + '       Run it against the soak rig (AO_CDP_PORT=9224) or pass --allow-user-app.');
}

const STYLE_ID = '__ao_perfprobe_ab';
const b = await connectBrowser();
const p = await connectPage();
await p.send('HeapProfiler.enable');

let rect = null;

async function scrollFor(secs) {
  const t0 = Date.now();
  let n = 0;
  while (Date.now() - t0 < secs * 1000) {
    const dir = Math.floor((Date.now() - t0) / 1500) % 2 === 0 ? -60 : 60;
    await p.send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: rect.x, y: rect.y, deltaX: 0, deltaY: dir });
    n++;
    await sleep(16);
  }
  return n;
}

const renderer = (dump) => {
  const r = Object.values(dump.byPid).find(isRenderer);
  if (!r) throw new Error('no renderer process in the memory dump');
  return r;
};
const classMap = (proc) => new Map(blinkClassRows(proc).map((r) => [r.name, [r.bytes, r.count ?? 0]]));

async function phase(label, css) {
  await evaluate(p, `(() => { let s = document.getElementById(${JSON.stringify(STYLE_ID)}); if (!s) { s = document.createElement('style'); s.id = ${JSON.stringify(STYLE_ID)}; document.head.appendChild(s); } s.textContent = ${JSON.stringify(css)}; })()`);
  await sleep(800);
  // Two collections: the first drops the easy garbage, the second settles what the first freed.
  await p.send('HeapProfiler.collectGarbage');
  await sleep(300);
  await p.send('HeapProfiler.collectGarbage');
  await sleep(300);
  const r0 = renderer(await takeMemoryDump(b, 'detailed'));
  const t0 = Date.now();
  const wheels = SCROLL ? await scrollFor(SECS) : (await sleep(SECS * 1000), 0);
  const r1 = renderer(await takeMemoryDump(b, 'detailed'));
  const secs = (Date.now() - t0) / 1000;
  const a0 = allocatorMB(r0, 'blink_gc/main/allocated_objects');
  const a1 = allocatorMB(r1, 'blink_gc/main/allocated_objects');
  const perMin = ((a1 - a0) * 60 / secs).toFixed(1);
  const wheelNote = SCROLL ? `${wheels} wheel events over ${secs.toFixed(1)}s; ` : '';
  console.log(`${label}: ${wheelNote}allocated_objects ${a0.toFixed(1)} -> ${a1.toFixed(1)}MB over ${secs.toFixed(0)}s = ${perMin} MB/min; committed ${allocatorMB(r0, 'blink_gc')} -> ${allocatorMB(r1, 'blink_gc')}MB`);
  const c0 = classMap(r0), c1 = classMap(r1);
  const deltas = [];
  for (const [k, [b1, n1]] of c1) {
    const [b0, n0] = c0.get(k) || [0, 0];
    if (b1 - b0 > 200000) deltas.push([b1 - b0, n1 - n0, k]);
  }
  deltas.sort((x, y) => y[0] - x[0]);
  for (const [bytes, count, k] of deltas.slice(0, 8)) {
    console.log(`     +${(bytes / 1048576).toFixed(1).padStart(5)}MB ${String(count).padStart(7)}x ${k.slice(0, 90)}`);
  }
}

let code = 0;
try {
  if (SCROLL) {
    rect = await evaluate(p, `(() => { const el = document.querySelector(${JSON.stringify(CHAT_SCROLLER_SELECTOR)}); if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
    if (!rect) throw new Error('--scroll needs a mounted message timeline, found none');
  }
  await phase('A baseline ', '');
  await phase('B override ', CSS);
  await phase('A baseline ', '');
} catch (e) {
  console.error(`probe: ${e.message}`);
  code = 1;
} finally {
  // The override must never outlive the probe, including on a mid-phase failure.
  await p.send('Runtime.evaluate', { expression: `document.getElementById(${JSON.stringify(STYLE_ID)})?.remove()` }).catch(() => {});
  b.close();
  p.close();
}
await done([], code);
