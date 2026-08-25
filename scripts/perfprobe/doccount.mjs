// Read-only: Memory.getDOMCounters — documents/nodes/listeners. The documents
// count is the SVGImage-document tripwire: one real page + a handful of
// transients is healthy; ~58 means per-icon data-URI mask images are back.
import { connectPage, done } from './lib/cdp.mjs';

const page = await connectPage();
try {
  await page.send('Memory.enable').catch(() => {});
  const c = await page.send('Memory.getDOMCounters');
  console.log(`documents=${c.documents} nodes=${c.nodes} jsEventListeners=${c.jsEventListeners}`);
} finally {
  await done([page]);
}
