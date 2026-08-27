// Active-use heap dose-response: N panes on real threads, looping
// concurrent replays for hours, heap + LoAF + DOM samples after every
// round, forced-GC live/garbage split at the end. This is the rig for
// the uptime-scaling GC-pause class: heap growth per hour of heavy use.
//
//   node churn.mjs <url> <out.jsonl> --instance <id> --threads "A,B,C" [--hours N]
import { chromium } from '@playwright/test';
import {
  parseArgs, makeCli, requireThreads, installReplayScenarios,
  openPanes, armLoafCounter, sampleHeapLoaf, awaitRevealDrain,
  forcedGcSplit, appendJsonl,
} from './riglib.mjs';

const { positional: [url, outfile], flags } = parseArgs(process.argv.slice(2));
if (!url || !outfile) {
  console.error('usage: node churn.mjs <url> <out.jsonl> --instance <id> --threads "A,B,C" [--hours N]');
  process.exit(2);
}
const hours = Number(flags.hours ?? '6');
const cli = makeCli(flags.instance);
const threads = requireThreads(flags, 'churn');
await installReplayScenarios(cli, threads, '/tmp/ao-churn');

const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] });
const page = await browser.newPage({ viewport: { width: 2560, height: 1400 } });
await page.goto(url, { waitUntil: 'load' });
await page.waitForTimeout(4000);
await openPanes(cli, page, threads);
await armLoafCounter(page);

const cdp = await page.context().newCDPSession(page);
const t0 = Date.now();
const line = appendJsonl(outfile);
const sample = async (tag, extra = {}) =>
  line({ t: Date.now(), minUp: Math.round((Date.now() - t0) / 60000), tag, ...(await sampleHeapLoaf(page)), ...extra });

await sample('start');
const endAt = t0 + hours * 3600_000;
let round = 0;
while (Date.now() < endAt) {
  round++;
  const results = await Promise.all(threads.map((t) =>
    cli('send', '--thread', t, '--wait', '--timeout', '240s', 'replay')
      .then(() => 'ok', (e) => 'fail:' + (e.message ?? '').slice(0, 60)),
  ));
  await awaitRevealDrain(page, { capMs: 60_000 });
  await sample('round', { round, sends: results.every((r) => r === 'ok') ? 'ok' : results.join(',') });
  await page.waitForTimeout(10_000);
}

line({ t: Date.now(), tag: 'forced-gc', ...(await forcedGcSplit(page, cdp)) });
await browser.close();
console.log('done');
