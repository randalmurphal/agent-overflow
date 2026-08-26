// Hold a media-feature emulation on the app page for N seconds, then
// clear it: probe emulate reduced-motion <secs>. CDP media overrides die
// with the session, so this stays connected for the window — run it in
// the background and run the measuring probe (churn/alloc) beside it;
// it is Emulation-only, safe next to a tracing session.
import { connectPage, done, sleep } from './lib/cdp.mjs';

const feature = process.argv[2];
const secs = Number(process.argv[3] ?? '60');
if (feature !== 'reduced-motion' || !Number.isFinite(secs) || secs <= 0) {
  console.error('usage: probe emulate reduced-motion <secs>');
  process.exit(2);
}

const page = await connectPage();
try {
  await page.send('Emulation.setEmulatedMedia', {
    features: [{ name: 'prefers-reduced-motion', value: 'reduce' }],
  });
  console.log(`prefers-reduced-motion: reduce held for ${secs}s`);
  await sleep(secs * 1000);
  await page.send('Emulation.setEmulatedMedia', { features: [] });
  console.log('cleared');
} finally {
  await done([page]);
}
