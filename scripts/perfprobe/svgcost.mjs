// Per-SVG-root paint-property and geometry-mapper cost: counts the objects a scaled <svg> mints.
import { connectBrowser, connectPage, evaluate, done } from './lib/cdp.mjs';
import { takeMemoryDump, blinkClassRows, allocatorMB, allocatedObjectsMB, isRenderer } from './lib/memdump.mjs';

const browser = await connectBrowser();
const page = await connectPage();
await page.send('Runtime.enable');
await page.send('HeapProfiler.enable');
await page.send('HeapProfiler.collectGarbage');
await new Promise((r) => setTimeout(r, 500));

const dom = JSON.parse(await evaluate(page, `(() => {
  const svgs = [...document.querySelectorAll('svg')];
  const scaled = svgs.filter((s) => {
    const vb = s.viewBox?.baseVal;
    if (!vb || !vb.width) return false;
    const r = s.getBoundingClientRect();
    return Math.abs(r.width - vb.width) > 0.01 || Math.abs(r.height - vb.height) > 0.01;
  });
  const sizes = {};
  for (const s of svgs) { const r = s.getBoundingClientRect(); const k = Math.round(r.width) + 'x' + Math.round(r.height); sizes[k] = (sizes[k]||0)+1; }
  const kinds = {};
  for (const s of svgs) {
    const cls = [...s.classList];
    const lucide = cls.find((c) => c.startsWith('lucide-') && c !== 'lucide-icon');
    const k = lucide ? 'lucide' : (cls.join('.') || s.parentElement?.tagName?.toLowerCase() || 'bare') ;
    kinds[k] = (kinds[k]||0)+1;
  }
  return JSON.stringify({ svgs: svgs.length, scaled: scaled.length, sizes, kinds, elements: document.querySelectorAll('*').length });
})()`));

const dump = await takeMemoryDump(browser);
const proc = Object.values(dump.byPid).filter(isRenderer)
  .sort((a, b) => (allocatorMB(b, 'blink_gc') || 0) - (allocatorMB(a, 'blink_gc') || 0))[0];
const rows = blinkClassRows(proc);
const get = (n) => rows.find((r) => r.name === n) || { bytes: 0, count: 0 };

console.log(`dom: ${dom.svgs} <svg>, ${dom.scaled} rendered at a size != their viewBox, ${dom.elements} elements`);
console.log(`sizes: ${Object.entries(dom.sizes).sort((a,b)=>b[1]-a[1]).slice(0,8).map(([k,v])=>`${v}@${k}`).join('  ')}`);
console.log(`kinds: ${Object.entries(dom.kinds).sort((a,b)=>b[1]-a[1]).slice(0,10).map(([k,v])=>`${v} ${k.slice(0,40)}`).join('  |  ')}`);
console.log(`blink_gc committed ${allocatorMB(proc, 'blink_gc')} MB, live ${allocatedObjectsMB(proc)} MB (after a forced GC)\n`);
const NAMES = [
  'blink::GeometryMapperTransformCache::PlaneRootTransform',
  'blink::GeometryMapperTransformCache',
  'blink::TransformPaintPropertyNode',
  'blink::LayoutSVGRoot',
  'blink::SMILTimeContainer',
  'blink::ComputedStyleBase::StyleSVGData',
  'SVGAnimatedLength',
  'blink::SVGLength',
  'blink::HeapVectorBacking<cppgc::internal::BasicMember<blink::SVGSVGElement, cppgc::internal::StrongMemberTag, cppgc::internal::DijkstraWriteBarrierPolicy>>',
];
let total = 0;
for (const n of NAMES) {
  const r = get(n);
  total += r.bytes;
  console.log(`  ${(r.bytes / 1024).toFixed(0).padStart(7)} KB ${String(r.count).padStart(7)}  ${n.replace(/^blink::/, '').slice(0, 60)}`);
}
console.log(`  ${(total / 1024).toFixed(0).padStart(7)} KB           TOTAL of the SVG-root classes above`);
await done([browser, page]);
