// Which object types sit on each Oilpan space's pages, and how empty those pages are.
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, allocatedObjectsMB, isRenderer } from './lib/memdump.mjs';

const browser = await connectBrowser();
const page = await connectPage();
await page.send('HeapProfiler.enable');
await page.send('HeapProfiler.collectGarbage');
await sleep(1500);

const dump = await takeMemoryDump(browser);
const proc = Object.values(dump.byPid).filter(isRenderer)
  .sort((a, b) => (allocatorMB(b, 'blink_gc') || 0) - (allocatorMB(a, 'blink_gc') || 0))[0];
const size = (a) => (a.size ?? a.effectiveSize ?? 0);

const spaces = new Map();
for (const [n, a] of Object.entries(proc.allocators)) {
  let m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)$/);
  if (m) { (spaces.get(m[1]) || spaces.set(m[1], { committed: 0, live: 0, pages: 0, types: new Map() }).get(m[1])).committed = size(a); continue; }
  m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/page_\d+$/);
  if (m) { const s = spaces.get(m[1]) || spaces.set(m[1], { committed: 0, live: 0, pages: 0, types: new Map() }).get(m[1]); s.pages++; s.live += a.allocatedObjects ?? 0; continue; }
  m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/page_\d+\/types\/(.+?)(?: \(0x[0-9a-f]+\))?$/);
  if (m) {
    const s = spaces.get(m[1]) || spaces.set(m[1], { committed: 0, live: 0, pages: 0, types: new Map() }).get(m[1]);
    const t = s.types.get(m[2]) || { bytes: 0, count: 0, pages: 0 };
    t.bytes += size(a); t.count += a.objectCount ?? 0; t.pages++;
    s.types.set(m[2], t);
  }
}
console.log(`blink_gc ${allocatorMB(proc, 'blink_gc')}MB committed / ${allocatedObjectsMB(proc)}MB live, after a forced memory-reducing GC\n`);
for (const [n, s] of [...spaces].sort((a, b) => b[1].committed - a[1].committed).slice(0, 6)) {
  console.log(`== ${n}  ${(s.committed / 1048576).toFixed(1)}MB committed  ${(s.live / 1048576).toFixed(2)}MB live  ${s.pages} pages  ${(s.live / s.committed * 100).toFixed(1)}% full  (${(s.committed / s.pages / 1024).toFixed(0)}KB/page)`);
  const top = [...s.types].sort((a, b) => b[1].pages - a[1].pages).slice(0, 8);
  for (const [t, v] of top) {
    console.log(`     on ${String(v.pages).padStart(4)}/${String(s.pages).padEnd(4)} pages  ${String(v.count).padStart(6)} live objs  ${(v.bytes / 1024).toFixed(0).padStart(6)}KB  ${t.replace(/^blink::/, '').slice(0, 74)}`);
  }
  console.log('');
}
await done([browser, page]);
