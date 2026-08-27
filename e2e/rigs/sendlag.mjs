// Turn-start send-lag capture: UI-driven composer sends with user-timing
// marks, trace + cpuprofile, alternating reorder / no-reorder conditions.
//   node rigs/sendlag.mjs <url> <outPrefix> --instance <id> --threads "A,B"
import { chromium } from '@playwright/test';
import { writeFile } from 'node:fs/promises';
import {
  parseArgs, makeCli, requireThreads, installReplayScenarios,
  openPanes, awaitRevealDrain, armLoafCounter, sampleHeapLoaf,
} from './riglib.mjs';

const { positional: [url, outPrefix], flags } = parseArgs(process.argv.slice(2));
if (!url || !outPrefix) {
  console.error('usage: node rigs/sendlag.mjs <url> <outPrefix> --instance <id> --threads "A,B"');
  process.exit(2);
}
const cli = makeCli(flags.instance);
const threads = requireThreads(flags, 'sendlag');
if (threads.length < 2) { console.error('need two threads'); process.exit(2); }

await installReplayScenarios(cli, threads, outPrefix);

const browser = await chromium.launch({ headless: true });
const page = await browser.newPage({ viewport: { width: 2560, height: 1400 } });
await page.goto(url, { waitUntil: 'load' });
await page.waitForTimeout(3000);
await openPanes(cli, page, threads);
await page.waitForTimeout(2000);
await armLoafCounter(page);

const cdp = await page.context().newCDPSession(page);
await cdp.send('Profiler.enable');
await cdp.send('Profiler.setSamplingInterval', { interval: 100 });
await cdp.send('Profiler.start');
await browser.startTracing(page, {
  categories: [
    'devtools.timeline',
    'disabled-by-default-devtools.timeline',
    'disabled-by-default-devtools.timeline.stack',
    'blink.user_timing',
  ],
});

const LONG = ('Please review the following considerations carefully. ' +
  'There are a number of edge cases in the scroll controller worth attention. '.repeat(12));

// [paneIdx, label, text] — first send to a pane forces a sidebar reorder
// (bump to top); the immediate second send to the same pane does not.
const plan = [
  [0, 'a-reorder', 'ping one'],
  [0, 'a-top', 'ping two'],
  [1, 'b-reorder', 'ping three'],
  [1, 'b-top', 'ping four'],
  [0, 'a-long-reorder', LONG],
  [0, 'a-short-top', 'ping five'],
];

const results = [];
for (const [paneIdx, label, text] of plan) {
  const box = page.locator('textarea[aria-label="Message Input"]').nth(paneIdx);
  await box.click();
  await box.fill(text);
  await page.waitForTimeout(300);
  await page.evaluate((l) => performance.mark(`ao-send-${l}`), label);
  await page.keyboard.press('Enter');
  await page.waitForTimeout(2500);
  const s = await sampleHeapLoaf(page);
  results.push({ label, loafCount: s.loafCount, loafMax: s.loafMax });
  await awaitRevealDrain(page, { capMs: 150_000 });
  await page.waitForTimeout(500);
}

const trace = await browser.stopTracing();
const { profile } = await cdp.send('Profiler.stop');
await writeFile(`${outPrefix}.json`, trace);
await writeFile(`${outPrefix}.cpuprofile`, JSON.stringify(profile));
console.log(JSON.stringify(results, null, 1));
await browser.close();
