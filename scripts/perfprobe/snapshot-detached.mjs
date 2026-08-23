// Offline: detached DOM census, retainer boundaries and retaining paths from a saved heap snapshot.
// usage: probe snapshot-detached <file.heapsnapshot> [--paths N]
// offline
import { loadSnapshot } from './lib/heapsnapshot.mjs';

const args = process.argv.slice(2);
const file = args[0];
const pIdx = args.indexOf('--paths');
const PATHS = pIdx >= 0 ? +args[pIdx + 1] : 3;

const s = loadSnapshot(file);
console.log(`nodes=${s.nodeCount} edges=${s.edgeCount} strings=${s.stringCount}`);

const groups = new Map();
let detachedTotal = 0, detachedBytes = 0;
const detachedIdx = [];
for (let i = 0; i < s.nodeCount; i++) {
  if (!s.isDetached(i)) continue;
  const nm = s.name(i);
  detachedTotal++;
  const sz = s.selfSize(i);
  detachedBytes += sz;
  const g = groups.get(nm) || { count: 0, size: 0 };
  g.count++; g.size += sz;
  groups.set(nm, g);
  detachedIdx.push(i);
}
console.log(`detached nodes=${detachedTotal} selfBytes=${(detachedBytes / 1048576).toFixed(1)}MB`);
console.log('top detached classes:');
for (const [nm, g] of [...groups.entries()].sort((a, b) => b[1].count - a[1].count).slice(0, 15)) {
  console.log(`  ${g.count}x ${nm} (${(g.size / 1024).toFixed(0)}KB self)`);
}

const { parentNode, parentEdge } = s.retainerParents();
const boundary = new Map();
let unreachable = 0;
for (const i of detachedIdx) {
  if (parentNode[i] === -1) { unreachable++; continue; }
  let cur = i, guard = 0;
  while (guard++ < 500) {
    const p = parentNode[cur];
    if (p === cur) break;
    if (!s.isDetached(p)) {
      const key = `${s.type(p)} "${s.name(p).slice(0, 60)}" --${s.edgeTypeName(parentEdge[cur])}:${s.edgeName(parentEdge[cur]).slice(0, 40)}--> ${s.name(cur).slice(0, 40)}`;
      boundary.set(key, (boundary.get(key) || 0) + 1);
      break;
    }
    cur = p;
  }
}
console.log(`unreachable(weak-only)=${unreachable}`);
console.log('top retainer boundaries (JS side holding a detached tree):');
for (const [k, c] of [...boundary.entries()].sort((a, b) => b[1] - a[1]).slice(0, 25)) console.log(`  ${c}x ${k}`);

const wanted = new Set([...groups.entries()].sort((a, b) => b[1].count - a[1].count).slice(0, PATHS).map(([nm]) => nm));
const printed = new Set();
for (const i of detachedIdx) {
  const nm = s.name(i);
  if (!wanted.has(nm) || printed.has(nm) || parentNode[i] === -1) continue;
  printed.add(nm);
  console.log(`\npath for ${nm}:`);
  let cur = i, guard = 0;
  const lines = [];
  while (guard++ < 40) {
    const p = parentNode[cur];
    lines.push(`  <- ${s.type(p)} "${s.name(p).slice(0, 80)}" via ${s.edgeTypeName(parentEdge[cur])} "${s.edgeName(parentEdge[cur]).slice(0, 60)}"`);
    if (p === 0 || p === cur) break;
    cur = p;
  }
  console.log(lines.join('\n'));
}

const classSize = new Map();
for (let i = 0; i < s.nodeCount; i++) {
  const t = s.type(i);
  const key = (t === 'string' || t === 'concatenated string' || t === 'sliced string') ? '(strings)' : (t === 'code' ? '(code)' : s.name(i));
  const g = classSize.get(key) || { count: 0, size: 0 };
  g.count++; g.size += s.selfSize(i);
  classSize.set(key, g);
}
console.log('\ntop classes by self size:');
for (const [nm, g] of [...classSize.entries()].sort((a, b) => b[1].size - a[1].size).slice(0, 20)) {
  console.log(`  ${(g.size / 1048576).toFixed(1)}MB ${g.count}x ${nm.slice(0, 70)}`);
}
