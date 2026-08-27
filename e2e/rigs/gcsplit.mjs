// One-shot forced-GC split + DOM census against a live rig instance.
//
// Opens a FRESH page on the rig URL — it cannot reach the page inside a
// running driver's browser. That makes it a cold-page comparator: run it
// beside a long churn/soak curve and the delta between this page's
// used-heap/DOM-count and the aged driver page's latest curve row is the
// uptime cost of the aged page (caches + any leak), while the forced-GC
// split here gives the post-load live floor and the GC pause price at
// that heap size.
//
// Usage (from e2e/ or e2e/rigs/, needs @playwright/test resolvable):
//   node rigs/gcsplit.mjs <rig-url>
import { chromium } from '@playwright/test';
import { forcedGcSplit, sampleHeapLoaf } from './riglib.mjs';

const url = process.argv[2];
if (!url) {
  console.error('usage: node rigs/gcsplit.mjs <rig-url>');
  process.exit(1);
}
const browser = await chromium.launch();
const page = await browser.newPage();
await page.goto(url, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(3000);
const cdp = await page.context().newCDPSession(page);
const pre = await sampleHeapLoaf(page);
const split = await forcedGcSplit(page, cdp);
const census = await page.evaluate(() => {
  const byTag = {};
  for (const el of document.querySelectorAll('*')) byTag[el.tagName] = (byTag[el.tagName] || 0) + 1;
  const top = Object.entries(byTag).sort((a, b) => b[1] - a[1]).slice(0, 12);
  return { total: document.querySelectorAll('*').length, top };
});
console.log(JSON.stringify({ pre, split, census }, null, 1));
await browser.close();
