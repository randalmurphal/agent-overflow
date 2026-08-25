// Read-only: capture the page to a PNG in the probe out dir. Prints the path.
// usage: probe screenshot [label]
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { connectPage, done } from './lib/cdp.mjs';

const label = process.argv[2] || 'shot';
const page = await connectPage();
try {
  await page.send('Page.enable').catch(() => {});
  const shot = await page.send('Page.captureScreenshot', { format: 'png', fromSurface: true });
  const out = join(process.env.AO_PERFPROBE_OUT || process.env.TEMP || '.', `ao-${label}.png`);
  writeFileSync(out, Buffer.from(shot.data, 'base64'));
  console.log(`saved ${out}`);
} finally {
  await done([page]);
}
