// Take a V8 heap snapshot of the page and save it for the offline snapshot-* probes.
// usage: probe heapsnapshot [label]
import { createWriteStream, mkdirSync } from 'node:fs';
import { connectPage, done } from './lib/cdp.mjs';

const LABEL = process.argv[2] || 'run';
const OUT = process.env.AO_PERFPROBE_OUT || '.';
mkdirSync(OUT, { recursive: true });
const file = `${OUT}\\heap-${LABEL}.heapsnapshot`;

const c = await connectPage();
const out = createWriteStream(file);
let bytes = 0;
let closed;
const flushed = new Promise((r) => { closed = r; });
c.on((m) => {
  if (m.method !== 'HeapProfiler.addHeapSnapshotChunk') return;
  bytes += m.params.chunk.length;
  out.write(m.params.chunk);
});
await c.send('HeapProfiler.enable');
await c.send('HeapProfiler.takeHeapSnapshot', { reportProgress: false, treatGlobalObjectsAsRoots: true, captureNumericValue: false });
out.end(() => closed());
await flushed;
console.log(`${file}  ${bytes} bytes`);
await done([c]);
