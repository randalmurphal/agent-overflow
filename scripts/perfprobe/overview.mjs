// One-shot survey: browser processes, page metrics, DOM census, promoted elements, page facts.
// usage: probe overview
import { connectBrowser, connectPage, done, evaluate } from './lib/cdp.mjs';
import { pad } from './lib/format.mjs';
import { SCROLL_SURFACE_SELECTOR } from './lib/dom.mjs';

const b = await connectBrowser();
let procs = null;
try { procs = (await b.send('SystemInfo.getProcessInfo')).processInfo; } catch (e) { console.log('process info unavailable:', e.message); }
b.close();
if (procs) {
  console.log(`== browser processes: ${procs.length}`);
  for (const p of procs.sort((x, y) => y.cpuTime - x.cpuTime)) {
    console.log(`  ${pad(p.id, 7)}  ${pad(p.type, 12)}  cpuTime ${p.cpuTime.toFixed(1)}s`);
  }
}

const c = await connectPage();
await c.send('Performance.enable');
const metrics = Object.fromEntries((await c.send('Performance.getMetrics')).metrics.map((m) => [m.name, m.value]));
console.log('== performance metrics');
console.log(`  Nodes ${metrics.Nodes}  Documents ${metrics.Documents}  Frames ${metrics.Frames}  LayoutObjects ${metrics.LayoutObjects}  JSEventListeners ${metrics.JSEventListeners}`);
console.log(`  JSHeapUsed ${(metrics.JSHeapUsedSize / 1048576).toFixed(1)}MB of ${(metrics.JSHeapTotalSize / 1048576).toFixed(1)}MB  TaskDuration ${metrics.TaskDuration?.toFixed(1)}s  ScriptDuration ${metrics.ScriptDuration?.toFixed(1)}s`);
console.log(`  LayoutCount ${metrics.LayoutCount}  LayoutDuration ${metrics.LayoutDuration?.toFixed(1)}s  RecalcStyleCount ${metrics.RecalcStyleCount}  RecalcStyleDuration ${metrics.RecalcStyleDuration?.toFixed(1)}s`);

try {
  const dc = await c.send('Memory.getDOMCounters');
  console.log(`== dom counters: documents ${dc.documents}  nodes ${dc.nodes}  jsEventListeners ${dc.jsEventListeners}`);
} catch (e) { console.log('== dom counters unavailable:', e.message); }

const dom = await evaluate(c, `(() => {
  const all = document.querySelectorAll('*');
  const byTag = {};
  for (const el of all) byTag[el.tagName] = (byTag[el.tagName] || 0) + 1;
  return {
    total: all.length,
    topTags: Object.entries(byTag).sort((a, b) => b[1] - a[1]).slice(0, 15),
    canvases: [...document.querySelectorAll('canvas')].map((c) => c.width + 'x' + c.height),
    imgs: document.querySelectorAll('img').length,
    videos: document.querySelectorAll('video').length,
  };
})()`);
console.log(`== dom: ${dom.total} elements, ${dom.imgs} img, ${dom.videos} video, ${dom.canvases.length} canvas${dom.canvases.length ? ' [' + dom.canvases.join(' ') + ']' : ''}`);
for (const [tag, n] of dom.topTags) console.log(`  ${pad(n, 7)}  ${tag.toLowerCase()}`);

const promoted = await evaluate(c, `(() => {
  const res = [];
  for (const el of document.querySelectorAll('*')) {
    const cs = getComputedStyle(el);
    const wc = cs.willChange;
    if (!((wc && wc !== 'auto') || cs.transform !== 'none' || cs.backdropFilter !== 'none' || cs.filter !== 'none')) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 1 || r.height < 1) continue;
    const w = Math.max(r.width, el.scrollWidth), h = Math.max(r.height, el.scrollHeight);
    res.push({
      tag: el.tagName.toLowerCase(),
      cls: (el.getAttribute('class') || '').slice(0, 60),
      willChange: wc,
      transform: cs.transform !== 'none',
      filter: cs.filter !== 'none' ? cs.filter.slice(0, 40) : '',
      w: Math.round(r.width), h: Math.round(r.height),
      sw: el.scrollWidth, sh: el.scrollHeight,
      estMB: +(w * h * 4 / 1048576).toFixed(1),
    });
  }
  res.sort((a, b) => b.estMB - a.estMB);
  return { count: res.length, totalEstMB: +res.reduce((s, x) => s + x.estMB, 0).toFixed(1), top: res.slice(0, 25) };
})()`);
console.log(`== promoted-ish elements (will-change / transform / filter): ${promoted.count}, ${promoted.totalEstMB}MB of estimated texture at 4 bytes/px`);
for (const e of promoted.top) {
  console.log(`  ${pad(e.estMB + 'MB', 8)} ${pad(e.w + 'x' + e.h, 11)} scroll ${pad(e.sw + 'x' + e.sh, 11)}  ${e.tag}.${e.cls}  wc=${e.willChange}${e.transform ? ' transform' : ''}${e.filter ? ' filter=' + e.filter : ''}`);
}

const facts = await evaluate(c, `(() => {
  const ents = performance.getEntriesByType('resource');
  return {
    href: location.href,
    dpr: devicePixelRatio,
    vw: innerWidth, vh: innerHeight,
    scrollSurfaces: document.querySelectorAll(${JSON.stringify(SCROLL_SURFACE_SELECTOR)}).length,
    panes: document.querySelectorAll('[data-pane-id]').length,
    scripts: [...document.scripts].map((s) => s.src.split('/').pop()).filter(Boolean),
    heapMB: performance.memory ? +(performance.memory.usedJSHeapSize / 1048576).toFixed(1) : null,
    resourceCount: ents.length,
    resourceBytes: ents.reduce((s, e) => s + (e.transferSize || e.encodedBodySize || 0), 0),
  };
})()`);
console.log('== page');
console.log(`  href ${facts.href}  backend port ${new URL(facts.href).port || '(none)'}`);
console.log(`  viewport ${facts.vw}x${facts.vh} at dpr ${facts.dpr}  scroll surfaces ${facts.scrollSurfaces}  [data-pane-id] ${facts.panes}`);
console.log(`  scripts ${facts.scripts.join(' ') || '(none)'}`);
console.log(`  js heap ${facts.heapMB}MB  resources ${facts.resourceCount} (${(facts.resourceBytes / 1048576).toFixed(1)}MB transferred)`);
await done([c]);
