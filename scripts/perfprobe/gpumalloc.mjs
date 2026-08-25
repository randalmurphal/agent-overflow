// GPU-process allocator detail: every node with ALL provider attrs, plus the
// same view for the browser (manager) process. The malloc/partitions and
// win_heap slices never got below-top-level attribution — this prints whatever
// the provider exposes (bucket rows included when present).
// usage: probe gpumalloc
import { connectBrowser, done } from './lib/cdp.mjs';
import { takeMemoryDump, isGpu, isRenderer, role } from './lib/memdump.mjs';

const b = await connectBrowser();
const { byPid } = await takeMemoryDump(b, 'detailed');
const procs = Object.values(byPid).filter((p) => isGpu(p) || (!isRenderer(p) && !isGpu(p)));
for (const p of procs) {
  console.log(`== pid ${p.pid} ${role(p)}: private=${p.privMB}MB`);
  const names = Object.keys(p.allocators).sort();
  for (const n of names) {
    const a = p.allocators[n];
    const raw = a.raw || {};
    const parts = Object.entries(raw).map(([k, v]) => {
      const num = parseInt(v.value || '0', 16);
      return v.units === 'bytes' ? `${k}=${(num / 1048576).toFixed(2)}MB` : `${k}=${num}`;
    });
    const size = a.effectiveSize ?? a.size;
    if ((size ?? 0) < 65536 && !parts.some((s) => /MB/.test(s) && parseFloat(s.split('=')[1]) >= 0.06)) continue;
    console.log(`${n}  ${parts.join(' ')}`);
  }
}
await done([b]);
