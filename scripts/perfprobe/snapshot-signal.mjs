// Offline: from a heap snapshot, list every svelte reaction subscribed to a module-scope signal.
// usage: probe snapshot-signal <file.heapsnapshot> <moduleVar>
// offline
import { loadSnapshot } from './lib/heapsnapshot.mjs';
import { fail } from './lib/format.mjs';

const file = process.argv[2];
const VAR = process.argv[3];
if (!VAR) fail('probe: snapshot-signal needs <file> <moduleVar>, for example accounts$1');
const s = loadSnapshot(file);

// The signal lives in a module closure, so it is reachable as a context edge off a Context node.
let signal = -1;
for (let i = 0; i < s.nodeCount && signal < 0; i++) {
  if (s.type(i) !== 'object' || !s.name(i).startsWith('system / Context')) continue;
  for (const e of s.outEdges(i)) if (e.type === 'context' && e.name === VAR) { signal = e.to; break; }
}
if (signal < 0) fail(`probe: no module-scope context variable named ${VAR} in this snapshot`);
console.log(`signal ${VAR}: node ${signal} ${s.name(signal)} props=${s.props(signal).join(',')}`);
console.log('legend: an effect whose fn is null has been destroyed (svelte nulls fn on teardown);');
console.log('        DETACHED marks a DOM node held in the closure context that is out of the document.');

const reactions = s.prop(signal, 'reactions');
if (reactions < 0) { console.log('no reactions array'); process.exit(0); }
const elems = [...s.outEdges(reactions)].filter((e) => e.type === 'element').map((e) => e.to);
console.log(`reactions: ${elems.length}`);

const isNullNode = (i) => i < 0 || /^(null|undefined)$/.test(s.name(i));
const kindOf = (r) => {
  const p = s.props(r);
  return (p.includes('teardown') || p.includes('nodes_start') || p.includes('first')) ? 'effect' : 'derived';
};
const fnLabel = (r) => {
  const fn = s.prop(r, 'fn');
  return isNullNode(fn) ? 'null (destroyed)' : JSON.stringify(s.name(fn) || '(anon)');
};
const ctxOf = (r) => {
  const fn = s.prop(r, 'fn');
  return isNullNode(fn) ? -1 : s.prop(fn, 'context');
};

for (const r of elems) {
  const ctx = ctxOf(r);
  const ctxVars = ctx >= 0 ? [...s.outEdges(ctx)].filter((e) => e.type === 'context').map((e) => e.name) : [];
  const parent = s.prop(r, 'parent');
  const rx = s.prop(r, 'reactions');
  const rxCount = rx >= 0 ? [...s.outEdges(rx)].filter((e) => e.type === 'element').length : 0;
  console.log(`- ${kindOf(r)} fn=${fnLabel(r)} parent=${parent >= 0 ? s.name(parent) + '#' + parent : 'null'} ownReactions=${rxCount} ctx=[${ctxVars.slice(0, 14).join(',')}]`);
}

function ctxDom(r) {
  const ctx = ctxOf(r);
  if (ctx < 0) return 'no-ctx';
  const doms = [...s.outEdges(ctx)].filter((e) => e.type === 'context' && s.type(e.to) === 'native');
  if (!doms.length) return 'no-dom-in-ctx';
  return doms.map((e) => `${e.name}=${s.isDetached(e.to) ? 'DETACHED' : 'attached'}`).join(',');
}
const describe = (r) => `${kindOf(r)}#${r} fn=${fnLabel(r)} parent=${s.prop(r, 'parent') >= 0 ? '#' + s.prop(r, 'parent') : 'null'} dom:${ctxDom(r)}`;

console.log('\n== per-reaction chain (who still depends on it)');
for (const r of elems) {
  console.log(`* ${describe(r)}`);
  let cur = r;
  for (let depth = 0; depth < 8; depth++) {
    const rx = s.prop(cur, 'reactions');
    if (rx < 0) { console.log(`  ${'  '.repeat(depth)}(no reactions)`); break; }
    const ups = [...s.outEdges(rx)].filter((e) => e.type === 'element').map((e) => e.to);
    if (!ups.length) { console.log(`  ${'  '.repeat(depth)}(reactions empty)`); break; }
    for (const u of ups) console.log(`  ${'  '.repeat(depth)}<- ${describe(u)}`);
    cur = ups[0];
  }
}

console.log('\n== parent chain of each reaction (effect ownership)');
for (const r of elems) {
  let cur = r;
  const chain = [];
  for (let d = 0; d < 10; d++) {
    const parent = s.prop(cur, 'parent');
    if (parent < 0) { chain.push('null'); break; }
    const fn = s.prop(parent, 'fn');
    chain.push(`#${parent}:${isNullNode(fn) ? 'destroyed' : (s.name(fn) || '(anon)')}`);
    cur = parent;
  }
  console.log(`* #${r}: ${chain.join(' -> ')}`);
}
