// Oilpan live vs garbage (forced GC), then churn by blink class over a window (MB/min per class).
// usage: probe churn [seconds=30]
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, allocatedObjectsMB, isRenderer, blinkClassRows } from './lib/memdump.mjs';
import { fail } from './lib/format.mjs';

const SECS = +(process.argv[2] || 30);
const b = await connectBrowser();
const p = await connectPage();
await p.send('HeapProfiler.enable');

const renderer = async () => {
  const { byPid } = await takeMemoryDump(b, 'detailed');
  const r = Object.values(byPid).find(isRenderer);
  if (!r) fail('probe: no renderer process in the memory dump');
  return r;
};
const line = (label, r) => `${label}: renderer private=${r.privMB}MB blink_gc committed=${allocatorMB(r, 'blink_gc')}MB allocated=${allocatedObjectsMB(r)}MB v8=${allocatorMB(r, 'v8')}MB`;

// Before and after the forced GC splits the renderer into live and not-yet-collected garbage.
console.log(line('before gc', await renderer()));

// The class rows count live objects plus unswept garbage, so a window that starts right after a
// full GC and ends before the next one measures allocation by class. collectGarbage pauses the
// renderer for a few hundred ms; it is not visible but say so when the user is mid-work.
await p.send('HeapProfiler.collectGarbage');
await sleep(1000);
const r0 = await renderer();
console.log(line('t0 (after gc)', r0));
await sleep(SECS * 1000);
const r1 = await renderer();
console.log(line(`t1 (+${SECS}s)`, r1));
if (allocatedObjectsMB(r1) < allocatedObjectsMB(r0)) {
  console.log('a GC ran inside the window, so the class diff is not a churn measure; rerun with a shorter window');
  await done([b, p], 1);
}

const before = new Map(blinkClassRows(r0).map((r) => [r.name, r]));
const rows = [];
for (const r of blinkClassRows(r1)) {
  const prev = before.get(r.name);
  const bytes = r.bytes - (prev?.bytes || 0);
  if (bytes > 50000) rows.push({ bytes, count: (r.count ?? 0) - (prev?.count ?? 0), name: r.name });
}
rows.sort((x, y) => y.bytes - x.bytes);
const total = rows.reduce((s, r) => s + r.bytes, 0);
const perMin = (bytes) => (bytes / 1048576 * 60 / SECS).toFixed(2);
console.log(`classes that grew by more than 50KB: ${(total / 1048576).toFixed(1)}MB over ${SECS}s = ${perMin(total)} MB/min`);
for (const r of rows.slice(0, 45)) {
  console.log(`  ${perMin(r.bytes).padStart(7)} MB/min ${String(r.count).padStart(8)}x ${r.name.slice(0, 120)}`);
}
await done([b, p]);
