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
  const g = byPrefix.get(key) || { count: 0, size: 0 };
  g.count++; g.size += sz;
  byPrefix.set(key, g);
}
for (const [k, g] of [...byPrefix.entries()].sort((a, b) => b[1].size - a[1].size).slice(0, TOP)) {
  console.log(`  ${mb(g.size).padStart(9)}  ${String(g.count).padStart(6)}x  ${JSON.stringify(k)}`);
}
