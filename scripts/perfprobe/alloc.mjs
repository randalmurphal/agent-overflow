// V8 sampling heap profile: which JS call sites allocate the most over the window.
// usage: probe alloc [seconds=60]
//
// Output: total, top leaf sites, then an ATTRIBUTED TREE (who → who →
// allocation) so a hot native frame ("join", "Set add") is read against
// its callers instead of guessed at. When the build ran with
// AO_SOURCEMAP=1 (hidden maps beside the assets), frames resolve to
// original file:line and anonymized frames get their original name back;
// otherwise raw bundle coordinates print. The raw profile is saved to
// the out dir for offline digging.
import { connectPage, done, sleep } from './lib/cdp.mjs';
import { createFrameResolver } from './lib/sourcemap.mjs';
import { writeFileSync } from 'node:fs';

const SECS = +(process.argv[2] || 60);
// A 64KB sample interval lost the stopSampling response on a three-minute
// four-pane run. Start coarse for low-overhead hotspot discovery; use
// AO_ALLOC_INTERVAL for finer, shorter captures after locating a hot path.
const INTERVAL = +(process.env.AO_ALLOC_INTERVAL || 1048576);
if (!Number.isFinite(SECS) || SECS <= 0) throw new Error('alloc: seconds must be positive and finite');
if (!Number.isSafeInteger(INTERVAL) || INTERVAL <= 0) throw new Error('alloc: AO_ALLOC_INTERVAL must be a positive safe integer');
const savedAt = `alloc-${Date.now()}.heapprofile`;
const metadataPath = `${savedAt}.json`;
const metadata = { seconds: SECS, samplingIntervalBytes: INTERVAL, startedAt: new Date().toISOString(), status: 'running' };
writeFileSync(metadataPath, JSON.stringify(metadata, null, 2));
let c;
let profile;
try {
  c = await connectPage();
  await c.send('HeapProfiler.enable');
  await c.send('HeapProfiler.startSampling', { samplingInterval: INTERVAL, includeObjectsCollectedByMajorGC: true, includeObjectsCollectedByMinorGC: true });
  await sleep(SECS * 1000);
  ({ profile } = await c.send('HeapProfiler.stopSampling'));
  writeFileSync(savedAt, JSON.stringify(profile));
  writeFileSync(metadataPath, JSON.stringify({ ...metadata, status: 'succeeded' }, null, 2));
} catch (error) {
  try {
    writeFileSync(metadataPath, JSON.stringify({ ...metadata, status: 'failed', error: String(error) }, null, 2));
  } catch (writeError) {
    throw new AggregateError([error, writeError], 'alloc: capture failed and failure metadata could not be saved');
  }
  throw error;
} finally {
  c?.close();
  await sleep(50);
}

// One resolver over every bundle URL in the profile.
const urls = [];
(function collect(node) {
  if (node.callFrame?.url) urls.push(node.callFrame.url);
  for (const ch of node.children || []) collect(ch);
})(profile.head);
const resolve = await createFrameResolver(urls);

const frameKey = (f) => {
  const mapped = f.url ? resolve(f.url, f.lineNumber, f.columnNumber) : null;
  const name = f.functionName || mapped?.name || '(anon)';
  if (mapped) return `${name} ${mapped.source}:${mapped.line}`;
  const url = f.url.replace(/^.*\/src\//, 'src/').replace(/^.*\/assets\//, '').replace(/\?.*$/, '');
  return `${name} ${url}:${f.lineNumber + 1}`;
};

// Merge the raw profile tree by frame key so one function's allocation is
// one node per call path, then aggregate totals bottom-up.
function mergeInto(children, node) {
  const key = frameKey(node.callFrame);
  let m = children.get(key);
  if (!m) {
    m = { key, self: 0, total: 0, children: new Map() };
    children.set(key, m);
  }
  m.self += node.selfSize || 0;
  for (const ch of node.children || []) mergeInto(m.children, ch);
}
const rootChildren = new Map();
for (const ch of profile.head.children || []) mergeInto(rootChildren, ch);
let total = profile.head.selfSize || 0;
function sumTotals(node) {
  node.total = node.self;
  for (const ch of node.children.values()) node.total += sumTotals(ch);
  return node.total;
}
for (const ch of rootChildren.values()) total += sumTotals(ch);

const selfBy = new Map();
(function collectSelf(children) {
  for (const node of children.values()) {
    if (node.self) selfBy.set(node.key, (selfBy.get(node.key) || 0) + node.self);
    collectSelf(node.children);
  }
})(rootChildren);

const mb = (b) => (b / 1048576).toFixed(1) + 'MB';
console.log(`estimated allocation total over ${SECS}s: ${mb(total)}   (sample interval: ${INTERVAL} bytes; raw profile: ${savedAt})`);
console.log('\nTOP SELF (leaf allocation sites):');
for (const [k, v] of [...selfBy.entries()].sort((a, b) => b[1] - a[1]).slice(0, 25)) console.log(`  ${mb(v).padStart(8)}  ${k}`);

// Attributed tree: every path carrying at least max(1% of total, 512KB).
// Chains with a single significant child collapse onto one line so deep
// framework stacks read as a sentence, not a staircase.
const THRESHOLD = Math.max(total * 0.01, 512 * 1024);
function printTree(node, depth) {
  const kids = [...node.children.values()].filter((k) => k.total >= THRESHOLD).sort((a, b) => b.total - a.total);
  let label = node.key;
  let cursor = node;
  let below = kids;
  while (
    below.length === 1
    && cursor.self < THRESHOLD
    && below[0].total >= cursor.total - THRESHOLD / 4
  ) {
    cursor = below[0];
    label += `  →  ${cursor.key}`;
    below = [...cursor.children.values()].filter((k) => k.total >= THRESHOLD).sort((a, b) => b.total - a.total);
  }
  const selfNote = cursor.self >= THRESHOLD ? `  [self ${mb(cursor.self)}]` : '';
  console.log(`${'  '.repeat(depth)}${mb(cursor.total).padStart(8)}  ${label}${selfNote}`);
  for (const ch of below) printTree(ch, depth + 1);
}
console.log(`\nATTRIBUTED TREE (paths ≥ ${mb(THRESHOLD)}):`);
for (const ch of [...rootChildren.values()].filter((k) => k.total >= THRESHOLD).sort((a, b) => b.total - a.total)) {
  printTree(ch, 0);
}
await done([c]);
