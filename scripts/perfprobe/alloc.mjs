// V8 sampling heap profile: which JS call sites allocate the most over the window.
// usage: probe alloc [seconds=60]
import { connectPage, done, sleep } from './lib/cdp.mjs';

const SECS = +(process.argv[2] || 60);
const c = await connectPage();
await c.send('HeapProfiler.enable');
await c.send('HeapProfiler.startSampling', { samplingInterval: 16384, includeObjectsCollectedByMajorGC: true, includeObjectsCollectedByMinorGC: true });
await sleep(SECS * 1000);
const { profile } = await c.send('HeapProfiler.stopSampling');
c.close();

const selfBy = new Map(), inclBy = new Map();
let total = 0;
const walk = (node, stack) => {
  const f = node.callFrame;
  const key = `${f.functionName || '(anon)'} ${f.url.replace(/^.*\/src\//, 'src/').replace(/\?.*$/, '')}:${f.lineNumber + 1}`;
  const self = node.selfSize || 0;
  total += self;
  if (self) selfBy.set(key, (selfBy.get(key) || 0) + self);
  const next = stack.concat(key);
  for (const k of new Set(next)) if (self) inclBy.set(k, (inclBy.get(k) || 0) + self);
  for (const ch of node.children || []) walk(ch, next);
};
walk(profile.head, []);
const mb = (b) => (b / 1048576).toFixed(1) + 'MB';
console.log(`sampled allocation total over ${SECS}s: ${mb(total)}`);
console.log('\nTOP SELF (leaf allocation sites):');
for (const [k, v] of [...selfBy.entries()].sort((a, b) => b[1] - a[1]).slice(0, 25)) console.log(`  ${mb(v).padStart(8)}  ${k}`);
console.log('\nTOP INCLUSIVE (who is on the stack for the most allocation):');
for (const [k, v] of [...inclBy.entries()].sort((a, b) => b[1] - a[1]).slice(0, 40)) console.log(`  ${mb(v).padStart(8)}  ${k}`);
await done([c]);
