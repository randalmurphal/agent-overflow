// Oilpan + v8 page-level accounting: every blink_gc and v8 allocator node with ALL provider attrs.
// Splits committed pages into used vs slack (pool / fragmentation) — the floor question.
// usage: probe blinkpages
import { connectBrowser, done } from './lib/cdp.mjs';
import { takeMemoryDump, isRenderer } from './lib/memdump.mjs';

const b = await connectBrowser();
const { byPid } = await takeMemoryDump(b, 'detailed');
const r = Object.values(byPid).find(isRenderer);
if (!r) {
  console.log('no renderer dump');
} else {
  console.log(`renderer pid ${r.pid}: private=${r.privMB}MB`);
  const names = Object.keys(r.allocators)
    .filter((n) => (n.startsWith('blink_gc') || n.startsWith('v8') || n.startsWith('partition_alloc') || n.startsWith('malloc')) && !n.startsWith('blink_objects'))
    .sort();
  for (const n of names) {
    const raw = r.allocators[n].raw || {};
    const parts = Object.entries(raw).map(([k, v]) => {
      const num = parseInt(v.value || '0', 16);
      return v.units === 'bytes' ? `${k}=${(num / 1048576).toFixed(2)}MB` : `${k}=${num}`;
    });
    if (parts.length) console.log(`${n}  ${parts.join(' ')}`);
  }
}
await done([b]);
