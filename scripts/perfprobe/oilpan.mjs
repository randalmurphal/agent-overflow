// Oilpan page accounting: committed vs live per space, page fill histogram, across forced GCs.
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, allocatedObjectsMB, isRenderer } from './lib/memdump.mjs';

const browser = await connectBrowser();
const page = await connectPage();
await page.send('HeapProfiler.enable');

async function snap(label) {
  const dump = await takeMemoryDump(browser);
  const proc = Object.values(dump.byPid).filter(isRenderer)
    .sort((a, b) => (allocatorMB(b, 'blink_gc') || 0) - (allocatorMB(a, 'blink_gc') || 0))[0];
  const size = (a) => (a.size ?? a.effectiveSize ?? 0);

  const spaces = new Map();
  const pages = [];
  for (const [n, a] of Object.entries(proc.allocators)) {
    let m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)$/);
    if (m) { spaces.set(m[1], { committed: size(a), live: a.allocatedObjects ?? 0 }); continue; }
    m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/(page_\d+)$/);
    if (m) pages.push({ space: m[1], committed: size(a), live: a.allocatedObjects ?? 0 });
  }
  console.log(`\n== ${label}   private ${proc.privMB}MB   blink_gc ${allocatorMB(proc, 'blink_gc')}MB committed / ${allocatedObjectsMB(proc)}MB live   v8 ${allocatorMB(proc, 'v8')}MB   malloc ${allocatorMB(proc, 'malloc')}MB   partition_alloc ${allocatorMB(proc, 'partition_alloc')}MB   cc ${allocatorMB(proc, 'cc')}MB   gpu ${allocatorMB(proc, 'gpu')}MB`);
  if (spaces.size) {
    console.log('   space                 committed      live    fill    pages');
    for (const [n, s] of [...spaces].sort((a, b) => b[1].committed - a[1].committed)) {
      const cnt = pages.filter((p) => p.space === n).length;
      console.log(`   ${n.padEnd(20)} ${(s.committed / 1048576).toFixed(1).padStart(8)}MB ${(s.live / 1048576).toFixed(1).padStart(8)}MB ${s.committed ? ((s.live / s.committed) * 100).toFixed(0).padStart(6) : '     -'}% ${String(cnt).padStart(8)}`);
    }
  }
  if (pages.length) {
    const buckets = [0, 5, 10, 25, 50, 75, 101];
    const hist = new Array(buckets.length - 1).fill(0);
    const held = new Array(buckets.length - 1).fill(0);
    for (const p of pages) {
      const pct = p.committed ? (p.live / p.committed) * 100 : 0;
      for (let i = 0; i < hist.length; i++) if (pct >= buckets[i] && pct < buckets[i + 1]) { hist[i]++; held[i] += p.committed; break; }
    }
    console.log(`   ${pages.length} pages, ${(pages.reduce((s, p) => s + p.committed, 0) / 1048576).toFixed(1)}MB committed, ${(pages.reduce((s, p) => s + p.live, 0) / 1048576).toFixed(1)}MB live`);
    console.log('   fill      pages   committed held');
    for (let i = 0; i < hist.length; i++) {
      console.log(`   ${String(buckets[i]).padStart(3)}-${String(buckets[i + 1] - 1).padStart(3)}% ${String(hist[i]).padStart(7)} ${(held[i] / 1048576).toFixed(1).padStart(9)}MB`);
    }
  }
  return proc;
}

await snap('as found');
for (let i = 1; i <= 2; i++) {
  await page.send('HeapProfiler.collectGarbage');
  await sleep(1500);
  await snap(`after forced memory-reducing GC #${i}`);
}
await done([browser, page]);
