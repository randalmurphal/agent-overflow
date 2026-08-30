// Offline: size census of a saved heap snapshot — bytes by node type, by constructor, fat nodes + strings with retainer chains.
// usage: probe snapshot-census <file.heapsnapshot> [--top N]
// offline
import { loadSnapshot } from './lib/heapsnapshot.mjs';

const args = process.argv.slice(2);
const file = args[0];
const tIdx = args.indexOf('--top');
const TOP = tIdx >= 0 ? +args[tIdx + 1] : 20;

const s = loadSnapshot(file);
console.log(`nodes=${s.nodeCount} edges=${s.edgeCount} strings=${s.stringCount}`);

const mb = (b) => (b / 1048576).toFixed(2) + 'MB';
const kb = (b) => (b / 1024).toFixed(0) + 'KB';

// -- bytes by node type
const byType = new Map();
let total = 0;
for (let i = 0; i < s.nodeCount; i++) {
  const sz = s.selfSize(i);
  total += sz;
  const t = s.type(i);
  const g = byType.get(t) || { count: 0, size: 0 };
  g.count++; g.size += sz;
  byType.set(t, g);
}
console.log(`total self ${mb(total)}`);
console.log('-- bytes by node type');
for (const [t, g] of [...byType.entries()].sort((a, b) => b[1].size - a[1].size)) {
  console.log(`  ${mb(g.size).padStart(9)}  ${String(g.count).padStart(8)}x  ${t}`);
}

// -- top constructors (object/native nodes) by aggregate self size
const byCtor = new Map();
for (let i = 0; i < s.nodeCount; i++) {
  const t = s.type(i);
  if (t !== 'object' && t !== 'native') continue;
  const nm = `${t}:${s.name(i)}`;
  const g = byCtor.get(nm) || { count: 0, size: 0 };
  g.count++; g.size += s.selfSize(i);
  byCtor.set(nm, g);
}
console.log(`-- top constructors by self size`);
for (const [nm, g] of [...byCtor.entries()].sort((a, b) => b[1].size - a[1].size).slice(0, TOP)) {
  console.log(`  ${mb(g.size).padStart(9)}  ${String(g.count).padStart(8)}x  ${nm.slice(0, 90)}`);
}

// Svelte component/effect retention mostly appears as closures and context
// objects rather than named object constructors. Keep closure counts visible
// beside the native/object census so a compact-renderer change cannot reduce
// DOM nodes while silently retaining the full component program behind them.
const byClosure = new Map();
for (let i = 0; i < s.nodeCount; i++) {
  if (s.type(i) !== 'closure') continue;
  const name = s.name(i) || '(anonymous)';
  const g = byClosure.get(name) || { count: 0, size: 0 };
  g.count++;
  g.size += s.selfSize(i);
  byClosure.set(name, g);
}
console.log('-- top closure names by count');
for (const [name, g] of [...byClosure.entries()]
  .sort((left, right) => right[1].count - left[1].count)
  .slice(0, TOP)) {
  console.log(`  ${String(g.count).padStart(8)}x  ${mb(g.size).padStart(9)}  ${name.slice(0, 100)}`);
}

// -- retainer chain helper (BFS-from-root parents: the chain is one witness, not the only one)
const { parentNode, parentEdge } = s.retainerParents();
function chain(i, depth = 6) {
  const parts = [];
  let cur = i, guard = 0;
  while (guard++ < depth && parentNode[cur] !== -1 && parentNode[cur] !== cur) {
    const p = parentNode[cur];
    parts.push(`${s.name(p) || s.type(p)}.${s.edgeName(parentEdge[cur])}`);
    cur = p;
  }
  return parts.reverse().join(' -> ');
}

// -- fat individual nodes (anything over 256KB self)
console.log('-- fat nodes (>256KB self), with one retainer chain each');
const fat = [];
for (let i = 0; i < s.nodeCount; i++) {
  if (s.selfSize(i) > 262144) fat.push(i);
}
fat.sort((a, b) => s.selfSize(b) - s.selfSize(a));
for (const i of fat.slice(0, TOP)) {
  const nm = s.type(i) === 'string' ? JSON.stringify(s.name(i).slice(0, 70)) : `${s.type(i)} ${s.name(i).slice(0, 60)}`;
  console.log(`  ${kb(s.selfSize(i)).padStart(8)}  ${nm}`);
  console.log(`            ${chain(i)}`);
}

// -- string mass by prefix bucket (what the string bytes ARE)
console.log('-- string bytes by 40-char prefix bucket (top buckets)');
const byPrefix = new Map();
for (let i = 0; i < s.nodeCount; i++) {
  if (s.type(i) !== 'string' && s.type(i) !== 'concatenated string' && s.type(i) !== 'sliced string') continue;
  const sz = s.selfSize(i);
  if (sz < 4096) continue; // small strings are noise for attribution
  const key = s.name(i).slice(0, 40).replace(/\s+/g, ' ');
  const g = byPrefix.get(key) || {
    count: 0,
    size: 0,
    nodes: [],
    samples: [],
    retainers: new Map(),
    sliceRetainers: new Set(),
  };
  g.count++;
  g.size += sz;
  g.nodes.push(i);
  g.samples.push(i);
  g.samples.sort((left, right) => s.selfSize(right) - s.selfSize(left));
  if (g.samples.length > 3) g.samples.length = 3;
  byPrefix.set(key, g);
}
const topPrefixes = [...byPrefix.entries()]
  .sort((a, b) => b[1].size - a[1].size)
  .slice(0, TOP);
const targetPrefix = new Map();
for (const [key, group] of topPrefixes) {
  for (const node of group.nodes) targetPrefix.set(node, key);
}
for (let parent = 0; parent < s.nodeCount; parent++) {
  for (const edge of s.outEdges(parent)) {
    if (edge.type === 'weak') continue;
    const key = targetPrefix.get(edge.to);
    if (key === undefined) continue;
    const retainers = byPrefix.get(key).retainers;
    const label = `${s.type(parent)}:${s.name(parent).slice(0, 70)}.${edge.name}`;
    retainers.set(label, (retainers.get(label) ?? 0) + 1);
    if (s.type(parent) === 'sliced string') {
      byPrefix.get(key).sliceRetainers.add(parent);
    }
  }
}
for (const [k, g] of topPrefixes) {
  const largest = g.samples[0];
  console.log(
    `  ${mb(g.size).padStart(9)}  ${String(g.count).padStart(6)}x  ` +
    `max ${kb(s.selfSize(largest)).padStart(7)}  ${JSON.stringify(k)}`,
  );
  for (const sample of g.samples) {
    console.log(`            ${kb(s.selfSize(sample)).padStart(7)}  ${chain(sample)}`);
  }
  for (const [retainer, count] of [...g.retainers.entries()]
    .sort((left, right) => right[1] - left[1])
    .slice(0, 8)) {
    console.log(`            ${String(count).padStart(7)}x direct  ${retainer}`);
  }
  const sliceChains = new Map();
  for (const slice of g.sliceRetainers) {
    const label = chain(slice, 10);
    sliceChains.set(label, (sliceChains.get(label) ?? 0) + 1);
  }
  for (const [sliceChain, count] of [...sliceChains.entries()]
    .sort((left, right) => right[1] - left[1])
    .slice(0, 8)) {
    console.log(`            ${String(count).padStart(7)}x slice   ${sliceChain}`);
  }
}
