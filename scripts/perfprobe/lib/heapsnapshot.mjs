// Reader for a V8 .heapsnapshot: typed-array index over nodes and edges, plus BFS retainer parents.
import { readFileSync } from 'node:fs';
import { fail } from './format.mjs';

export function loadSnapshot(path) {
  if (!path) fail('probe: give the path of a .heapsnapshot file');
  let snap;
  try {
    snap = JSON.parse(readFileSync(path, 'utf8'));
  } catch (e) {
    fail(`probe: cannot read snapshot ${path}: ${e.message}`);
  }
  const meta = snap.snapshot?.meta;
  if (!meta) fail(`probe: ${path} is not a heap snapshot`);
  const nf = meta.node_fields, ef = meta.edge_fields;
  const NF = nf.length, EF = ef.length;
  const nodeTypes = meta.node_types[0], edgeTypes = meta.edge_types[0];
  const nodes = snap.nodes, edges = snap.edges, strings = snap.strings;
  const nodeCount = nodes.length / NF, edgeCount = edges.length / EF;

  const fType = nf.indexOf('type'), fName = nf.indexOf('name');
  const fSelf = nf.indexOf('self_size'), fEdges = nf.indexOf('edge_count');
  const fDetached = nf.indexOf('detachedness');
  const eType = ef.indexOf('type'), eName = ef.indexOf('name_or_index'), eTo = ef.indexOf('to_node');
  const weakType = edgeTypes.indexOf('weak');

  const firstEdge = new Uint32Array(nodeCount + 1);
  { let e = 0; for (let i = 0; i < nodeCount; i++) { firstEdge[i] = e; e += nodes[i * NF + fEdges] * EF; } firstEdge[nodeCount] = e; }

  const name = (i) => strings[nodes[i * NF + fName]] ?? '?';
  const type = (i) => nodeTypes[nodes[i * NF + fType]];
  const selfSize = (i) => nodes[i * NF + fSelf];
  // detachedness is the authoritative field; the "Detached " name prefix only exists on some
  // native nodes, so it is a fallback for snapshots taken before the field existed.
  const isDetached = fDetached >= 0
    ? (i) => nodes[i * NF + fDetached] === 2
    : (i) => name(i).startsWith('Detached ');

  function* outEdges(i) {
    const end = firstEdge[i + 1];
    for (let e = firstEdge[i]; e < end; e += EF) {
      const t = edgeTypes[edges[e + eType]];
      const v = edges[e + eName];
      yield { type: t, name: (t === 'element' || t === 'hidden') ? String(v) : strings[v], to: edges[e + eTo] / NF, edge: e };
    }
  }
  const prop = (i, want) => {
    for (const e of outEdges(i)) {
      if ((e.type === 'property' || e.type === 'context' || e.type === 'internal') && e.name === want) return e.to;
    }
    return -1;
  };
  const props = (i) => [...outEdges(i)].filter((e) => e.type === 'property').map((e) => e.name);
  const describe = (i) => (i < 0 ? 'none'
    : `#${i} ${type(i)} ${JSON.stringify(name(i)).slice(0, 80)}${isDetached(i) ? ' [DETACHED]' : ''}`);

  const edgeTypeName = (e) => edgeTypes[edges[e + eType]];
  const edgeName = (e) => {
    const t = edgeTypes[edges[e + eType]];
    const v = edges[e + eName];
    return (t === 'element' || t === 'hidden') ? `[${v}]` : `${strings[v]}`;
  };

  let retainers = null;
  function retainerParents() {
    if (retainers) return retainers;
    const parentNode = new Int32Array(nodeCount).fill(-1);
    const parentEdge = new Int32Array(nodeCount).fill(-1);
    const queue = new Uint32Array(nodeCount);
    let qh = 0, qt = 0;
    queue[qt++] = 0; parentNode[0] = 0;
    while (qh < qt) {
      const n = queue[qh++];
      for (let e = firstEdge[n]; e < firstEdge[n + 1]; e += EF) {
        if (edges[e + eType] === weakType) continue;
        const to = edges[e + eTo] / NF;
        if (parentNode[to] !== -1) continue;
        parentNode[to] = n; parentEdge[to] = e;
        queue[qt++] = to;
      }
    }
    retainers = { parentNode, parentEdge };
    return retainers;
  }

  return {
    nodeCount, edgeCount, stringCount: strings.length,
    name, type, selfSize, isDetached, outEdges, prop, props, describe,
    edgeTypeName, edgeName, retainerParents,
  };
}
