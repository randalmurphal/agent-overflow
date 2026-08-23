// Offline: dump the svelte signal fields of specific heap-snapshot nodes, with arrays expanded.
// usage: probe snapshot-node <file.heapsnapshot> <nodeId...>
// offline
import { loadSnapshot } from './lib/heapsnapshot.mjs';
import { fail } from './lib/format.mjs';

const [file, ...ids] = process.argv.slice(2);
if (!ids.length) fail('probe: snapshot-node needs <file> <nodeId...>');
const s = loadSnapshot(file);

for (const id of ids.map(Number)) {
  console.log(`\n== ${s.describe(id)}`);
  for (const k of ['v', 'fn', 'parent', 'ctx', 'deps', 'reactions', 'effects', 'nodes', 'teardown', 'ac']) {
    const p = s.prop(id, k);
    if (p < 0) { console.log(`  ${k}: <absent>`); continue; }
    let extra = '';
    if (s.type(p) === 'array' || s.name(p) === 'Array') {
      const els = [...s.outEdges(p)].filter((e) => e.type === 'element').map((e) => s.describe(e.to));
      extra = ` len=${els.length}\n      ${els.join('\n      ')}`;
    }
    if (s.type(p) === 'object' && s.name(p) === 'Object' && k === 'nodes') {
      extra = ' ' + [...s.outEdges(p)].filter((e) => e.type === 'property').map((e) => `${e.name}=${s.describe(e.to)}`).join(', ');
    }
    console.log(`  ${k}: ${s.describe(p)}${extra}`);
  }
  const fn = s.prop(id, 'fn');
  if (fn >= 0) {
    const ctx = s.prop(fn, 'context');
    if (ctx >= 0) {
      const vars = [...s.outEdges(ctx)].filter((e) => e.type === 'context').map((e) => `${e.name}=${s.describe(e.to)}`);
      console.log(`  ctx vars: ${vars.join(' | ')}`);
    }
  }
}
