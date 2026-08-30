// Paint invalidation census: who damages which layer, per client.
// usage: probe paintinv [seconds=10]
// Traces disabled-by-default-blink.invalidation and groups
// PaintInvalidationTracking events by client + reason — the direct
// answer to "why does this layer repaint every frame".
import { connectBrowser, readStream, sleep } from './lib/cdp.mjs';
import { pad } from './lib/format.mjs';

const SECS = +(process.argv[2] || 10);
const b = await connectBrowser();
const cats = ['disabled-by-default-blink.invalidation', 'devtools.timeline'];
await b.send('Tracing.start', { traceConfig: { includedCategories: cats, excludedCategories: ['*'] }, transferMode: 'ReturnAsStream' });
await sleep(SECS * 1000);
const done = b.waitFor('Tracing.tracingComplete');
await b.send('Tracing.end');
const complete = await done;
const data = await readStream(b, complete.stream);
b.close();

const evs = (JSON.parse(data).traceEvents || []);
const inv = evs.filter((e) => /PaintInvalidation/i.test(e.name));
console.log(`== ${inv.length} paint invalidation events over ${SECS}s`);
const by = new Map();
for (const e of inv) {
  const d = e.args?.data || e.args || {};
  const k = `${d.client || d.nodeName || '?'} | ${d.reason || '?'}`;
  by.set(k, (by.get(k) || 0) + 1);
}
for (const [k, n] of [...by.entries()].sort((a, b2) => b2[1] - a[1]).slice(0, 30)) {
  console.log(`${pad(n, 7)}  ${k}`);
}
process.exit(0);
