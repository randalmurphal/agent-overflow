// Detailed memory-infra dump: per process private footprint and allocator breakdown.
// usage: probe memdump [--renderer] [--classes] [--gpu] [--trigger-blink-gc-mb <N> [--max-sec <S>]]
import { connectBrowser, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, allocatedObjectsMB, isRenderer, isGpu, role, blinkClassRows, ccTileTotal } from './lib/memdump.mjs';
import { pad, mb, fail } from './lib/format.mjs';

const args = process.argv.slice(2);
const has = (f) => args.includes(f);
const num = (f, def) => { const i = args.indexOf(f); return i >= 0 ? +args[i + 1] : def; };
const WANT_RENDERER = has('--renderer'), WANT_CLASSES = has('--classes'), GPU_ONLY = has('--gpu');
const TRIGGER = num('--trigger-blink-gc-mb', 0), MAXSEC = num('--max-sec', 600);

const eff = (a) => a.effectiveSize ?? a.size;

function printProcess(proc) {
  console.log(`== pid ${proc.pid} ${role(proc)}: private ${proc.privMB}MB`);
  const top = [];
  const second = [];
  for (const [name, a] of Object.entries(proc.allocators)) {
    const bytes = eff(a);
    if (bytes == null) continue;
    const parts = name.split('/');
    if (parts.length === 1) top.push([name, bytes]);
    else if (parts.length === 2 && bytes > 2 * 1048576) second.push([name, bytes]);
  }
  top.sort((x, y) => y[1] - x[1]);
  second.sort((x, y) => y[1] - x[1]);
  console.log('-- top-level allocators');
  for (const [name, bytes] of top) console.log(`  ${pad(mb(bytes) + 'MB', 9)}  ${name}`);
  if (second.length) {
    console.log('-- second-level allocators over 2MB');
    for (const [name, bytes] of second) console.log(`  ${pad(mb(bytes) + 'MB', 9)}  ${name}`);
  }
}

function printRendererSubtree(proc) {
  const rows = [];
  for (const [name, a] of Object.entries(proc.allocators)) {
    if (!/^(blink_gc|cc|v8|malloc|partition_alloc|blink_objects|font_caches|web_cache|gpu)/.test(name)) continue;
    const size = a.size, e = a.effectiveSize, alloc = a.allocatedObjects;
    if ((size ?? 0) < 262144 && (e ?? 0) < 262144 && (alloc ?? 0) < 262144) continue;
    rows.push(`${name}  size=${size != null ? mb(size) : '-'}MB eff=${e != null ? mb(e) : '-'}MB alloc_objs=${alloc != null ? mb(alloc) : '-'}MB`);
  }
  console.log(`-- renderer subtree rows over 256KB (pid ${proc.pid})`);
  console.log(rows.sort().join('\n'));
}

function printClasses(proc, limit = 40) {
  console.log(`renderer pid ${proc.pid}: private=${proc.privMB}MB blink_gc=${allocatorMB(proc, 'blink_gc')} allocated_objects=${allocatedObjectsMB(proc)} cc=${allocatorMB(proc, 'cc')} v8=${allocatorMB(proc, 'v8')} v8_old=${allocatorMB(proc, 'v8/main/heap/old_space')} malloc=${allocatorMB(proc, 'malloc')}`);
  const rows = blinkClassRows(proc);
  const total = rows.reduce((s, r) => s + r.bytes, 0);
  console.log(`top blink object classes (live + unswept garbage): ${mb(total)}MB over ${rows.length} classes`);
  for (const r of rows.slice(0, limit)) {
    console.log(`  ${pad(mb(r.bytes), 6)}MB ${r.count != null ? pad(r.count, 8) + 'x ' : ''}${r.name.slice(0, 110)}`);
  }
  const tiles = ccTileTotal(proc);
  console.log(`cc tiles: ${tiles.count} resources, ${mb(tiles.bytes)}MB`);
}

const b = await connectBrowser();

if (TRIGGER > 0) {
  // Light dumps are cheap enough to poll; only the crossing takes the expensive detailed dump.
  const t0 = Date.now();
  let fired = false;
  while ((Date.now() - t0) / 1000 < MAXSEC) {
    const { byPid } = await takeMemoryDump(b, 'light');
    const r = Object.values(byPid).find(isRenderer);
    if (!r) { console.log('no renderer dump'); await sleep(5000); continue; }
    const alloc = allocatedObjectsMB(r);
    console.log(`${new Date().toISOString()} blink_gc committed=${allocatorMB(r, 'blink_gc')} allocated_objects=${alloc} cc=${allocatorMB(r, 'cc')} v8=${allocatorMB(r, 'v8')}`);
    if (alloc !== null && alloc >= TRIGGER) {
      const det = await takeMemoryDump(b, 'detailed');
      const dr = Object.values(det.byPid).find(isRenderer);
      if (!dr) break;
      console.log(`DETAILED: committed=${allocatorMB(dr, 'blink_gc')} allocated_objects=${allocatedObjectsMB(dr)}`);
      printClasses(dr, 45);
      fired = true;
      break;
    }
    await sleep(5000);
  }
  if (!fired) console.log(`did not cross ${TRIGGER}MB within ${MAXSEC}s`);
  await done([b]);
}

const { byPid } = await takeMemoryDump(b, 'detailed');
b.close();
const procs = Object.values(byPid);
if (!procs.length) fail('probe: the memory dump came back empty');

if (GPU_ONLY) {
  const gpus = procs.filter(isGpu);
  if (!gpus.length) fail('probe: no GPU process in the dump');
  for (const proc of gpus) printProcess(proc);
  await done([b]);
}

for (const proc of procs.sort((x, y) => (y.privMB ?? 0) - (x.privMB ?? 0))) printProcess(proc);

if (WANT_RENDERER || WANT_CLASSES) {
  for (const proc of procs.filter(isRenderer)) {
    if (WANT_RENDERER) printRendererSubtree(proc);
    if (WANT_CLASSES) printClasses(proc);
  }
}
await done([b]);
