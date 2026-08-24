// Per-page sole-occupancy for Blink editing undo commands.
// Question: if the composer's edit commands were released, how many Oilpan
// pages would have nothing live left on them and could actually decommit?
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, allocatedObjectsMB, isRenderer } from './lib/memdump.mjs';

const EDIT = /Command$|^blink::(Insert|Delete|Replace|Typing|CompositeEdit|SimpleEdit|Apply)/;

const browser = await connectBrowser();
const page = await connectPage();
await page.send('HeapProfiler.enable');
await page.send('HeapProfiler.collectGarbage');
await sleep(1500);
await page.send('HeapProfiler.collectGarbage');
await sleep(1500);

const dump = await takeMemoryDump(browser);
const proc = Object.values(dump.byPid).filter(isRenderer)
  .sort((a, b) => (allocatorMB(b, 'blink_gc') || 0) - (allocatorMB(a, 'blink_gc') || 0))[0];
const size = (a) => (a.size ?? a.effectiveSize ?? 0);

// page key -> { space, live, types: Map<type,{bytes,count}> }
const pages = new Map();
const spaceCommitted = new Map();
for (const [n, a] of Object.entries(proc.allocators)) {
  let m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)$/);
  if (m) { spaceCommitted.set(m[1], size(a)); continue; }
  m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/(page_\d+)$/);
  if (m) {
    const k = `${m[1]}/${m[2]}`;
    const p = pages.get(k) || { space: m[1], live: 0, committed: size(a), types: new Map() };
    p.live = a.allocatedObjects ?? 0; p.committed = size(a); pages.set(k, p); continue;
  }
  m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/(page_\d+)\/types\/(.+?)(?: \(0x[0-9a-f]+\))?$/);
  if (m) {
    const k = `${m[1]}/${m[2]}`;
    const p = pages.get(k) || { space: m[1], live: 0, committed: 0, types: new Map() };
    const t = p.types.get(m[3]) || { bytes: 0, count: 0 };
    t.bytes += size(a); t.count += a.objectCount ?? 0;
    p.types.set(m[3], t); pages.set(k, p);
  }
}

console.log(`blink_gc ${allocatorMB(proc,'blink_gc')}MB committed / ${allocatedObjectsMB(proc)}MB live, after two forced memory-reducing GCs`);
console.log(`${pages.size} pages parsed across ${spaceCommitted.size} spaces\n`);

// Every edit-command-ish type actually present
const allEdit = new Map();
for (const p of pages.values())
  for (const [t, v] of p.types) if (EDIT.test(t)) {
    const e = allEdit.get(t) || { bytes: 0, count: 0, pages: 0 };
    e.bytes += v.bytes; e.count += v.count; e.pages++; allEdit.set(t, e);
  }
console.log('== edit-command types present ==');
if (!allEdit.size) console.log('  (none)');
for (const [t, v] of [...allEdit].sort((a,b)=>b[1].pages-a[1].pages))
  console.log(`  ${String(v.pages).padStart(4)} pages  ${String(v.count).padStart(6)} objs  ${(v.bytes/1024).toFixed(0).padStart(6)}KB  ${t.replace(/^blink::/,'')}`);

// Sole occupancy
let hostPages = 0, soleBytes = 0, solePages = 0, sharedPages = 0, sharedOtherBytes = 0;
const coTenants = new Map();
for (const p of pages.values()) {
  const edits = [...p.types].filter(([t]) => EDIT.test(t));
  if (!edits.length) continue;
  hostPages++;
  const others = [...p.types].filter(([t]) => !EDIT.test(t));
  const otherBytes = others.reduce((s, [, v]) => s + v.bytes, 0);
  if (otherBytes === 0) { solePages++; soleBytes += p.committed; }
  else {
    sharedPages++; sharedOtherBytes += otherBytes;
    for (const [t, v] of others) {
      const c = coTenants.get(t) || { bytes: 0, pages: 0 };
      c.bytes += v.bytes; c.pages++; coTenants.set(t, c);
    }
  }
}
console.log(`\n== sole occupancy ==`);
console.log(`  pages holding >=1 edit command : ${hostPages}`);
console.log(`  ...holding ONLY edit commands  : ${solePages}  -> ${(soleBytes/1048576).toFixed(1)}MB would decommit`);
console.log(`  ...shared with other live objs : ${sharedPages}  (${(sharedOtherBytes/1024).toFixed(0)}KB of co-tenants pins them)`);
{
  const host = [...pages.values()].filter(p => [...p.types].some(([t]) => EDIT.test(t)));
  const liveSum = host.reduce((s, p) => s + p.live, 0);
  const bySpace = new Map();
  for (const p of host) { const b = bySpace.get(p.space) || {n:0,c:0,l:0}; b.n++; b.c+=p.committed; b.l+=p.live; bySpace.set(p.space,b); }
  console.log(`\n== how empty are those pages ==`);
  console.log(`  live bytes on all ${host.length}: ${(liveSum/1024).toFixed(0)}KB total, ${(liveSum/host.length).toFixed(0)}B/page mean, ${(liveSum/host.reduce((s,p)=>s+p.committed,0)*100).toFixed(2)}% full`);
  const ls = host.map(p=>p.live).sort((a,b)=>a-b);
  console.log(`  live/page  min ${ls[0]}B  p50 ${ls[ls.length>>1]}B  p95 ${ls[Math.floor(ls.length*0.95)]}B  max ${ls[ls.length-1]}B`);
  for (const [sp,b] of [...bySpace].sort((a,b)=>b[1].c-a[1].c))
    console.log(`  ${sp}: ${b.n} pages, ${(b.c/1048576).toFixed(1)}MB committed, ${(b.l/1024).toFixed(0)}KB live`);
}
if (coTenants.size) {
  console.log(`\n== what pins the shared pages ==`);
  for (const [t, v] of [...coTenants].sort((a,b)=>b[1].pages-a[1].pages).slice(0, 12))
    console.log(`  on ${String(v.pages).padStart(4)} pages  ${(v.bytes/1024).toFixed(0).padStart(7)}KB  ${t.replace(/^blink::/,'').slice(0,74)}`);
}
await done([browser, page]);
