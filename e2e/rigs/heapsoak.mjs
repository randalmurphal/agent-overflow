// Passive heap curve: attach a page to a soak/harness instance and
// record renderer heap + LoAF counters for hours; one forced GC at the
// end splits live vs collectable. Pairs with `make soak` (autopilot
// streams activity without a driver).
//
//   node heapsoak.mjs <url> <out.jsonl> [--hours N]
import { chromium } from '@playwright/test';
import { parseArgs, armLoafCounter, sampleHeapLoaf, forcedGcSplit, appendJsonl } from './riglib.mjs';

const { positional: [url, outfile], flags } = parseArgs(process.argv.slice(2));
if (!url || !outfile) {
  console.error('usage: node heapsoak.mjs <url> <out.jsonl> [--hours N]');
  process.exit(2);
}
const hours = Number(flags.hours ?? '6');

const browser = await chromium.launch({ headless: true, args: ['--enable-precise-memory-info'] });
const page = await browser.newPage({ viewport: { width: 1600, height: 900 } });
await page.goto(url, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(8000);
await armLoafCounter(page);

const cdp = await page.context().newCDPSession(page);
const t0 = Date.now();
const line = appendJsonl(outfile);
const sample = async (tag) =>
  line({ t: Date.now(), minUp: Math.round((Date.now() - t0) / 60000), tag, ...(await sampleHeapLoaf(page)) });

const SAMPLE_MS = 120_000;
const endAt = t0 + hours * 3600_000;
await sample('start');
while (Date.now() < endAt) {
  await page.waitForTimeout(SAMPLE_MS);
  await sample('tick');
}
line({ t: Date.now(), tag: 'forced-gc', ...(await forcedGcSplit(page, cdp)) });
await browser.close();
console.log('done');
