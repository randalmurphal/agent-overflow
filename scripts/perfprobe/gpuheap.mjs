// GPU-process malloc attribution via OOP heap profiling (memlog). Needs the
// instance launched with AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS="--memlog=gpu
// --memlog-sampling-rate=8192 --memlog-stack-mode=native" (soak rig). Takes one
// detailed memory-infra dump, saves the raw trace beside the other probe
// artifacts, and prints the GPU pid's heaps_v2 attribution: MB by module (pc
// ranges joined to the process module list via powershell) and top stacks.
// usage: probe gpuheap [--raw-only]
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { connectBrowser, done, readStream, sleep } from './lib/cdp.mjs';
import { fail } from './lib/format.mjs';

const RAW_ONLY = process.argv.includes('--raw-only');
const outDir = process.env.AO_PERFPROBE_OUT || path.join(process.env.TEMP || '/tmp', 'ao-perfprobe');
fs.mkdirSync(outDir, { recursive: true });

const b = await connectBrowser();
b.events.length = 0;
await b.send('Tracing.start', {
  traceConfig: { includedCategories: ['disabled-by-default-memory-infra'], memoryDumpConfig: { triggers: [] } },
  transferMode: 'ReturnAsStream',
});
await b.send('Tracing.requestMemoryDump', { levelOfDetail: 'detailed' });
await sleep(2500);
const complete = new Promise((res) => {
  const t = setInterval(() => {
    const e = b.events.find((x) => x.method === 'Tracing.tracingComplete');
    if (e) { clearInterval(t); res(e.params); }
  }, 100);
});
await b.send('Tracing.end');
const { stream } = await complete;
const text = await readStream(b, stream);
const rawPath = path.join(outDir, `gpuheap-${Date.now()}.json`);
fs.writeFileSync(rawPath, text);
console.log(`raw trace: ${rawPath} (${(text.length / 1048576).toFixed(1)}MB)`);

const trace = JSON.parse(text);
// The GPU pid is the one whose dump carries gpu/shared_images without blink_gc.
let gpuPid = null;
const heapsByPid = new Map();
for (const e of trace.traceEvents || []) {
  if (e.ph !== 'v') continue;
  const d = e.args?.dumps;
  if (!d) continue;
  const alloc = d.allocators || {};
  if (!('blink_gc/main' in alloc) && ('gpu/shared_images' in alloc || 'cc/resource_memory' in alloc)) gpuPid = e.pid;
  if (d.heaps_v2) heapsByPid.set(e.pid, d.heaps_v2);
}
if (gpuPid == null) fail('probe: no GPU process dump in the trace');
const heaps = heapsByPid.get(gpuPid);
if (!heaps) {
  console.log(`pids with heaps_v2: ${[...heapsByPid.keys()].join(', ') || 'none'}`);
  fail(`probe: GPU pid ${gpuPid} has no heaps_v2 — was the instance launched with --memlog=gpu?`);
}
if (RAW_ONLY) {
  console.log(`gpu pid ${gpuPid}: heaps_v2 keys ${JSON.stringify(Object.keys(heaps))}`);
  await done([b]);
  process.exit(0);
}

// heaps_v2 shape: maps{nodes,types,strings} interned once per process (they can
// arrive spread over several 'v' events; merge), allocators.malloc.{nodes,counts,sizes}
// referencing map node ids.
const maps = { nodes: [], types: [], strings: [] };
for (const e of trace.traceEvents || []) {
  if (e.ph !== 'v' || e.pid !== gpuPid) continue;
  const h = e.args?.dumps?.heaps_v2;
  if (!h?.maps) continue;
  for (const k of Object.keys(maps)) if (h.maps[k]) maps[k].push(...h.maps[k]);
}
const strings = new Map(maps.strings.map((s) => [s.id, s.string]));
const nodeById = new Map(maps.nodes.map((n) => [n.id, n]));
const frameName = (n) => strings.get(n.name_sid) ?? '';

const entries = heaps.allocators?.malloc;
if (!entries) fail(`probe: heaps_v2 has no malloc allocator (keys: ${Object.keys(heaps.allocators || {})})`);
const { nodes = [], sizes = [], counts = [] } = entries;

// Module ranges for pc→module attribution, via powershell on the live pid.
let modules = [];
try {
  const ps = execFileSync('powershell.exe', ['-NoProfile', '-Command',
    `Get-Process -Id ${gpuPid} | Select-Object -ExpandProperty Modules | ForEach-Object { "{0}|{1}|{2}" -f $_.BaseAddress.ToInt64(), $_.ModuleMemorySize, $_.ModuleName }`,
  ], { encoding: 'utf8', timeout: 30000 });
  modules = ps.trim().split(/\r?\n/).filter(Boolean).map((l) => {
    const [base, size, name] = l.split('|');
    return { base: BigInt(base), end: BigInt(base) + BigInt(size), name };
  }).sort((a, z) => (a.base < z.base ? -1 : 1));
} catch (err) {
  console.log(`module list unavailable (${err.message.split('\n')[0]}); pc frames stay raw`);
}
const moduleOf = (pc) => {
  let lo = 0; let hi = modules.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const m = modules[mid];
    if (pc < m.base) hi = mid - 1;
    else if (pc >= m.end) lo = mid + 1;
    else return m.name;
  }
  return null;
};

// Attribute each sample to its leaf frame's module (skipping non-pc frames),
// and keep the full stack for the top rows.
const byModule = new Map();
const byStack = new Map();
for (let i = 0; i < nodes.length; i++) {
  const size = sizes[i] ?? 0;
  if (!size) continue;
  const stack = [];
  let leafModule = null;
  for (let n = nodeById.get(nodes[i]); n; n = nodeById.get(n.parent)) {
    const name = frameName(n);
    stack.push(name);
    if (leafModule === null && name.startsWith('pc:')) {
      leafModule = moduleOf(BigInt("0x" + name.slice(3))) ?? 'unknown-pc';
    } else if (leafModule === null && name && !name.startsWith('pc:')) {
      leafModule = name; // pseudo/mixed frames carry their own label
    }
    if (stack.length > 24) break;
  }
  const mod = leafModule ?? '<no frames>';
  byModule.set(mod, (byModule.get(mod) ?? 0) + size);
  const key = stack.slice(0, 8).map((f) => (f.startsWith('pc:') ? `${moduleOf(BigInt("0x" + f.slice(3))) ?? '?'}+${f.slice(3)}` : f)).join(' <- ');
  const s = byStack.get(key) || { size: 0, count: 0 };
  s.size += size; s.count += counts[i] ?? 0;
  byStack.set(key, s);
}

const totalMB = [...byModule.values()].reduce((a, v) => a + v, 0) / 1048576;
console.log(`== gpu pid ${gpuPid}: sampled malloc ${totalMB.toFixed(1)}MB by leaf module`);
for (const [mod, size] of [...byModule.entries()].sort((a, z) => z[1] - a[1])) {
  if (size < 262144) continue;
  console.log(`  ${(size / 1048576).toFixed(2).padStart(8)}MB  ${mod}`);
}
console.log('== top stacks over 1MB');
for (const [key, s] of [...byStack.entries()].sort((a, z) => z[1].size - a[1].size).slice(0, 25)) {
  if (s.size < 1048576) break;
  console.log(`  ${(s.size / 1048576).toFixed(2)}MB n=${s.count}  ${key}`);
}
await done([b]);
