// Samples every scroll surface's scrollTop/scrollHeight over a window: what is moving while "idle".
import { connectPage, evaluate, sleep, done } from './lib/cdp.mjs';
import { SCROLL_SURFACE_SELECTOR } from './lib/dom.mjs';

const SECS = +(process.argv[2] || 10);
const page = await connectPage();
await page.send('Runtime.enable');

const SAMPLE = `(() => {
  const els = [...document.querySelectorAll('*')].filter((e) => {
    const s = getComputedStyle(e);
    return (s.overflowY === 'auto' || s.overflowY === 'scroll') && e.scrollHeight > e.clientHeight + 1;
  });
  return JSON.stringify(els.map((e, i) => ({
    i, tag: e.tagName + '.' + [...e.classList].slice(0, 3).join('.'),
    top: +e.scrollTop.toFixed(2), h: e.scrollHeight, ch: e.clientHeight,
  })));
})()`;

const first = JSON.parse(await evaluate(page, SAMPLE));
const series = new Map(first.map((r) => [r.i, { tag: r.tag, tops: [r.top], hs: [r.h] }]));
const n = SECS * 5;
for (let k = 0; k < n; k++) {
  await sleep(200);
  for (const r of JSON.parse(await evaluate(page, SAMPLE))) {
    const s = series.get(r.i);
    if (s) { s.tops.push(r.top); s.hs.push(r.h); }
  }
}
console.log(`${series.size} scrollable surfaces sampled 5x/s for ${SECS}s\n`);
console.log('changes  topRange        heightRange   surface');
for (const [, s] of series) {
  const chg = s.tops.filter((v, i) => i && v !== s.tops[i - 1]).length;
  const hchg = s.hs.filter((v, i) => i && v !== s.hs[i - 1]).length;
  const rng = `${Math.min(...s.tops).toFixed(1)}..${Math.max(...s.tops).toFixed(1)}`;
  const hrng = `${Math.min(...s.hs)}..${Math.max(...s.hs)}`;
  if (chg || hchg) console.log(`${String(chg).padStart(4)}/${String(n).padEnd(4)} ${rng.padEnd(16)} ${hrng.padEnd(14)} (${hchg} h-changes)  ${s.tag.slice(0, 50)}`);
}
console.log('\n(surfaces with no change omitted)');
await done([page]);
