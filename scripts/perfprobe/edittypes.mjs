// Unfiltered: every Oilpan type whose name mentions Command / Undo / Edit, with live counts.
// Answers "what retains the leaf edit commands" by showing which parents are live.
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, isRenderer } from './lib/memdump.mjs';

const browser = await connectBrowser();
const page = await connectPage();
await page.send('Runtime.enable');
await page.send('HeapProfiler.enable');
await page.send('HeapProfiler.collectGarbage'); await sleep(900);
await page.send('HeapProfiler.collectGarbage'); await sleep(900);

const dump = await takeMemoryDump(browser);
const p = Object.values(dump.byPid).filter(isRenderer)
  .sort((a, b) => (allocatorMB(b, 'blink_gc') || 0) - (allocatorMB(a, 'blink_gc') || 0))[0];

const all = new Map();
for (const [n, a] of Object.entries(p.allocators)) {
  const m = n.match(/^blink_gc\/main\/heap\/[A-Za-z0-9]+\/pages\/page_\d+\/types\/(?:blink::)?(.+?)(?: \(0x[0-9a-f]+\))?$/);
  if (!m) continue;
  const t = all.get(m[1]) || { count: 0, pages: 0 };
  t.count += a.objectCount ?? 0; t.pages++; all.set(m[1], t);
}
console.log(`blink_gc ${allocatorMB(p, 'blink_gc')}MB, ${all.size} distinct live types\n`);
const rx = /Command|Undo|Edit|Typing|Text.*Element|Selection/i;
console.log('== editing-related live types ==');
for (const [t, v] of [...all].filter(([t]) => rx.test(t)).sort((a, b) => b[1].count - a[1].count))
  console.log(`  ${String(v.count).padStart(7)} objs  on ${String(v.pages).padStart(4)} pages  ${t}`);
console.log('\n== top 25 live types overall ==');
for (const [t, v] of [...all].sort((a, b) => b[1].count - a[1].count).slice(0, 25))
  console.log(`  ${String(v.count).padStart(7)} objs  on ${String(v.pages).padStart(4)} pages  ${t.slice(0, 70)}`);

const inv = await page.send('Runtime.evaluate', { expression: `JSON.stringify({
  textareas: document.querySelectorAll('textarea').length,
  inputs: document.querySelectorAll('input').length,
  xterm: document.querySelectorAll('.xterm').length,
  xtermTa: document.querySelectorAll('.xterm-helper-textarea').length,
  chars: [...document.querySelectorAll('textarea,input')].reduce((s,e)=>s+(e.value||'').length,0)
})`, returnByValue: true });
console.log('\nDOM:', inv.result.value);
await done([browser, page]);
