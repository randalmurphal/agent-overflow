// Thread-switch coldload profiler: open N heavy threads sequentially in
// ONE pane (each first open is a cold load — tail window fetch, mount,
// restore), capturing cpuprofile + timeline trace + in-page busy meter.
// Thread ids/titles come from the instance (a clone root carries real
// ones — pick its heaviest with `ao-harness threads`).
//
//   node coldload.mjs <url> <outPrefix> --instance <id> --threads "id1,id2,..."
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import { parseArgs, makeCli, requireThreads } from './riglib.mjs';

const { positional: [url, outPrefix], flags } = parseArgs(process.argv.slice(2));
if (!url || !outPrefix) {
  console.error('usage: node coldload.mjs <url> <outPrefix> --instance <id> --threads "id1,id2,..."');
  process.exit(2);
}
const cli = makeCli(flags.instance);
const threads = requireThreads(flags, 'coldload');

const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 2560, height: 1400 } });
await page.goto(url, { waitUntil: 'load' });
await page.waitForTimeout(4000);

// Mount the first thread OUTSIDE the profile: pane creation, xterm etc.
// pollute the first open. Every later open is a pure switch in the same pane.
await cli('ui', 'open', '--thread', threads[0]);
await page.waitForTimeout(2500);

const cdp = await page.context().newCDPSession(page);
await cdp.send('Profiler.enable');
await cdp.send('Profiler.setSamplingInterval', { interval: 100 });

const traceEvents = [];
cdp.on('Tracing.dataCollected', (p) => traceEvents.push(...p.value));
const traceDone = new Promise((r) => cdp.on('Tracing.tracingComplete', r));
await cdp.send('Tracing.start', {
  traceConfig: {
    includedCategories: ['devtools.timeline', 'disabled-by-default-devtools.timeline', 'disabled-by-default-devtools.timeline.stack'],
  },
  transferMode: 'ReportEvents',
});
await cdp.send('Profiler.start');
await cli('perf', 'start', '--budgets', '6,8,16');

const marks = [];
for (let i = 1; i < threads.length; i++) {
  const t = threads[i];
  const m0 = Date.now();
  const ok = await cli('ui', 'open', '--thread', t).then(() => true, () => false);
  await page.waitForTimeout(1800);
  marks.push({ thread: t, atMs: m0, ok });
}

const perf = await cli('perf', 'stop', '-o', 'json').then((r) => r.stdout, () => 'null');
const { profile } = await cdp.send('Profiler.stop');
await cdp.send('Tracing.end');
await traceDone;

fs.writeFileSync(`${outPrefix}.cpuprofile`, JSON.stringify(profile));
fs.writeFileSync(`${outPrefix}-trace.json`, JSON.stringify({ traceEvents }));
fs.writeFileSync(`${outPrefix}-perf.json`, perf);
fs.writeFileSync(`${outPrefix}-marks.json`, JSON.stringify(marks));
console.log('samples:', profile.samples?.length, 'traceEvents:', traceEvents.length);
await browser.close();
