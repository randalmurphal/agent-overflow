// Read-only: connect to the page target, evaluate, hold the socket, evaluate again.
// usage: probe pageping [holdSeconds=10] — times when a page session gets closed under it.
import { connectPage, evaluate, sleep, done } from './lib/cdp.mjs';

const HOLD = +(process.argv[2] || 10);
const t0 = Date.now();
const page = await connectPage();
try {
  const v = await evaluate(page, 'document.title');
  console.log(`+${Date.now() - t0}ms connected, title ${JSON.stringify(v)}`);
  for (let held = 0; held < HOLD; held += 5) {
    await sleep(Math.min(5, HOLD - held) * 1000);
    await evaluate(page, '1');
    console.log(`+${Date.now() - t0}ms still alive`);
  }
} catch (e) {
  console.log(`+${Date.now() - t0}ms DIED: ${e.message}`);
  process.exit(1);
} finally {
  await done([page]).catch(() => {});
}
