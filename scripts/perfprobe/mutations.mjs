// DOM mutation census: childList adds/removes by parent, attribute writes by attribute and element.
// usage: probe mutations [seconds=10]
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

const SECS = +(process.argv[2] || 10);
const c = await connectPage();

// A previous run that died mid-window can leave the observer attached, so clear it first.
await evaluate(c, `(() => {
  if (window.__aoPerfprobeObs) { window.__aoPerfprobeObs.disconnect(); delete window.__aoPerfprobeObs; }
  const st = { adds: 0, removes: 0, attrs: 0, chardata: 0, byParent: new Map(), byAttr: new Map(), samples: [] };
  const obs = new MutationObserver((muts) => {
    for (const m of muts) {
      st.adds += m.addedNodes.length; st.removes += m.removedNodes.length;
      if (m.type === 'characterData') st.chardata++;
      if (m.type === 'attributes') {
        st.attrs++;
        const t = m.target;
        const key = m.attributeName + ' @ ' + t.tagName + '.' + ('' + (t.className || '')).split(' ').slice(0, 3).join('.');
        st.byAttr.set(key, (st.byAttr.get(key) || 0) + 1);
        if (m.attributeName === 'style' && st.samples.length < 5 && Math.random() < 0.05) st.samples.push(t.getAttribute('style')?.slice(0, 150));
      }
      if (m.addedNodes.length || m.removedNodes.length) {
        const t = m.target;
        const key = t.tagName + '.' + ('' + (t.className || '')).split(' ').slice(0, 2).join('.');
        st.byParent.set(key, (st.byParent.get(key) || 0) + m.addedNodes.length + m.removedNodes.length);
      }
    }
  });
  obs.observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
  obs.__st = st;
  window.__aoPerfprobeObs = obs;
  return true;
})()`);

await sleep(SECS * 1000);

const r = await evaluate(c, `(() => {
  const obs = window.__aoPerfprobeObs;
  if (!obs) return null;
  obs.disconnect();
  const st = obs.__st;
  delete window.__aoPerfprobeObs;
  return {
    adds: st.adds, removes: st.removes, attrs: st.attrs, chardata: st.chardata,
    topParents: [...st.byParent.entries()].sort((a, b) => b[1] - a[1]).slice(0, 12),
    topAttrs: [...st.byAttr.entries()].sort((a, b) => b[1] - a[1]).slice(0, 15),
    styleSamples: st.samples,
  };
})()`);
if (!r) { console.error('probe: the mutation observer went missing before the window ended'); await done([c], 1); }

console.log(`== mutations over ${SECS}s: adds ${r.adds} removes ${r.removes} attrs ${r.attrs} (${(r.attrs / SECS).toFixed(0)}/s) characterData ${r.chardata}`);
console.log('-- childList churn by parent (adds + removes)');
for (const [k, n] of r.topParents) console.log(`  ${String(n).padStart(7)}  ${k}`);
console.log('-- attribute writes by attribute and element');
for (const [k, n] of r.topAttrs) console.log(`  ${String(n).padStart(7)}  ${k}`);
if (r.styleSamples.length) {
  console.log('-- sample style attribute values');
  for (const s of r.styleSamples) console.log(`  ${s}`);
}
await done([c]);
