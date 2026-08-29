// Offline: find objects retaining a named string field and report the rope's flat-prefix mass.
// usage: probe snapshot-string-rope <file.heapsnapshot> [propertyName]
// offline
import { loadSnapshot } from './lib/heapsnapshot.mjs';

const [file, property = 'cachedText'] = process.argv.slice(2);
const snapshot = loadSnapshot(file);
const { parentNode, parentEdge } = snapshot.retainerParents();

function chain(node, depth = 6) {
  const parts = [];
  let current = node;
  for (let index = 0; index < depth; index++) {
    const parent = parentNode[current];
    if (parent < 0 || parent === current) break;
    parts.push(`${snapshot.name(parent) || snapshot.type(parent)}.${snapshot.edgeName(parentEdge[current])}`);
    current = parent;
  }
  return parts.reverse().join(' -> ');
}

function ropeStats(root) {
  const seen = new Set();
  const stack = [{ node: root, depth: 0 }];
  let flatBytes = 0;
  let flatNodes = 0;
  let ropeNodes = 0;
  let maxDepth = 0;
  while (stack.length > 0) {
    const { node, depth } = stack.pop();
    if (seen.has(node)) continue;
    seen.add(node);
    maxDepth = Math.max(maxDepth, depth);
    const type = snapshot.type(node);
    if (type === 'concatenated string' || type === 'sliced string') {
      ropeNodes++;
      for (const edge of snapshot.outEdges(node)) {
        if (
          edge.type === 'internal' &&
          (edge.name === 'first' || edge.name === 'second' || edge.name === 'parent')
        ) stack.push({ node: edge.to, depth: depth + 1 });
      }
      continue;
    }
    if (type === 'string') {
      flatNodes++;
      flatBytes += snapshot.selfSize(node);
    }
  }
  return { flatBytes, flatNodes, ropeNodes, maxDepth };
}

const rows = [];
for (let owner = 0; owner < snapshot.nodeCount; owner++) {
  const root = snapshot.prop(owner, property);
  if (root < 0) continue;
  rows.push({ owner, root, ...ropeStats(root) });
}
rows.sort((left, right) => right.flatBytes - left.flatBytes);

console.log(`property=${property} owners=${rows.length}`);
for (const row of rows.slice(0, 50)) {
  console.log(
    `${(row.flatBytes / 1024).toFixed(0).padStart(8)}KB  ` +
    `${String(row.flatNodes).padStart(5)} flat  ${String(row.ropeNodes).padStart(5)} rope  ` +
    `depth ${String(row.maxDepth).padStart(4)}  ${snapshot.describe(row.owner)}`,
  );
  console.log(`            ${chain(row.owner)}`);
}
