// Census of elements carrying a transform / rotate / scale / translate / will-change:transform.
// usage: probe transforms
import { connectPage, done, evaluate } from './lib/cdp.mjs';

const c = await connectPage();
const v = await evaluate(c, `(() => {
  const out = new Map(); let total = 0, nonId = 0;
  const sig = (el, cs) => {
    const parts = [];
    if (cs.transform !== 'none') parts.push('transform=' + (cs.transform === 'matrix(1, 0, 0, 1, 0, 0)' ? 'identity' : (/^matrix\\(1, 0, 0, 1, /.test(cs.transform) ? '2d-translate' : cs.transform.slice(0,40))));
    if (cs.rotate && cs.rotate !== 'none') parts.push('rotate=' + cs.rotate);
    if (cs.scale && cs.scale !== 'none') parts.push('scale=' + cs.scale);
    if (cs.translate && cs.translate !== 'none') parts.push('translate=' + cs.translate);
    if (/transform|translate|rotate|scale/.test(cs.willChange)) parts.push('will-change=' + cs.willChange);
    if (cs.backfaceVisibility === 'hidden') parts.push('backface-hidden');
    if (cs.transformStyle === 'preserve-3d') parts.push('preserve-3d');
    if (cs.position === 'sticky') parts.push('sticky');
    return parts.join(' ');
  };
  for (const el of document.querySelectorAll('*')) {
    total++;
    const cs = getComputedStyle(el);
    const s = sig(el, cs);
    if (!s) continue;
    nonId++;
    const cls = (el.getAttribute('class') || '').split(/\\s+/).filter(c => /translate|rotate|scale|transform|will-change|sticky|composited|marker|pulse|spin|animate/.test(c)).slice(0,4).join('.');
    const key = el.tagName.toLowerCase() + (cls ? '.' + cls : '') + ' :: ' + s;
    out.set(key, (out.get(key) || 0) + 1);
  }
  return { total, nonId, rows: [...out.entries()].sort((a,b) => b[1]-a[1]).slice(0, 40) };
})()`);
console.log(`elements=${v.total} with transform-ish style=${v.nonId}`);
for (const [k, n] of v.rows) console.log(`  ${String(n).padStart(6)}  ${k}`);
await done([c]);
