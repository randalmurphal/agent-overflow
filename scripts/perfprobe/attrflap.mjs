// Attribute oscillation census: does an attribute PROGRESS through values or flap between two?
// usage: probe attrflap <attribute> [seconds=10]
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

// `probe mutations` counts attribute writes but cannot tell a write that
// tracks something real (a marker walking down a list as the reader
// scrolls) from a write that fights itself (two candidates alternating
// every frame). The difference decides whether the cost is inherent or a
// bug, and an alternating marker is usually VISIBLE flicker as well as
// wasted style invalidation. This records the ordered transition
// sequence per element and reports the reversal rate: the fraction of
// transitions that return to the immediately preceding value.

const ATTR = process.argv[2];
const SECS = +(process.argv[3] || 10);
if (!ATTR) {
  console.error('probe: attrflap needs an attribute name, e.g. `probe attrflap data-current`');
  process.exit(2);
}

const c = await connectPage();

await evaluate(c, `(() => {
  if (window.__aoAttrFlap) { window.__aoAttrFlap.disconnect(); delete window.__aoAttrFlap; }
  const attr = ${JSON.stringify(ATTR)};
  const st = { writes: 0, changes: 0, reversals: 0, t0: performance.now(), byEl: new Map(), seq: [] };
  const label = (t) => t.tagName.toLowerCase() + '.' + ('' + (t.className || '')).split(' ').slice(0, 2).join('.');
  const obs = new MutationObserver((muts) => {
    for (const m of muts) {
      if (m.type !== 'attributes' || m.attributeName !== attr) continue;
      st.writes++;
      const now = m.target.getAttribute(attr);
      let e = st.byEl.get(m.target);
      if (!e) { e = { label: label(m.target), writes: 0, n: 0, reversals: 0, prev: m.oldValue, prevPrev: null }; st.byEl.set(m.target, e); }
      e.writes++;
      if (now === m.oldValue) continue;   // same-value rewrite: cost, no change
      st.changes++;
      e.n++;
      if (e.prevPrev !== null && now === e.prevPrev) { e.reversals++; st.reversals++; }
      e.prevPrev = e.prev; e.prev = now;
      if (st.seq.length < 60) st.seq.push(Math.round(performance.now() - st.t0) + 'ms ' + e.label + ' ' + m.oldValue + '->' + now);
    }
  });
  obs.observe(document.documentElement, { subtree: true, attributes: true, attributeOldValue: true, attributeFilter: [attr] });
  obs.__st = st;
  window.__aoAttrFlap = obs;
  return true;
})()`);

await sleep(SECS * 1000);

const r = await evaluate(c, `(() => {
  const obs = window.__aoAttrFlap;
  if (!obs) return null;
  obs.disconnect();
  const st = obs.__st;
  delete window.__aoAttrFlap;
  return {
    writes: st.writes, changes: st.changes, reversals: st.reversals,
    elapsedMs: Math.round(performance.now() - st.t0),
    elCount: st.byEl.size,
    els: [...st.byEl.values()].sort((a, b) => b.writes - a.writes).slice(0, 12).map((e) => [e.label, e.writes, e.n, e.reversals]),
    seq: st.seq,
  };
})()`);
if (!r) { console.error('probe: the mutation observer went missing before the window ended'); await done([c], 1); }

const secs = r.elapsedMs / 1000;
const pct = r.changes ? ((r.reversals / r.changes) * 100).toFixed(0) : '0';
console.log(`== ${ATTR} over ${secs.toFixed(1)}s: ${r.writes} writes, ${r.changes} value changes (${(r.changes / secs).toFixed(1)}/s), ${r.reversals} reversals (${pct}% of changes)`);
console.log(`   same-value rewrites: ${r.writes - r.changes} across ${r.elCount} elements (a diff-only writer reports 0)`);
console.log('-- by element: writes, value changes, reversals');
for (const [label, w, n, rev] of r.els) {
  console.log(`  ${String(w).padStart(6)}  ${String(n).padStart(6)}  ${String(rev).padStart(6)}  ${label}`);
}
if (r.seq.length) {
  console.log('-- first transitions');
  for (const s of r.seq) console.log(`  ${s}`);
}
await done([c]);
