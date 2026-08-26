// Resolve trace backendNodeIds to elements: probe describenodes <id> [...]
// Paint/invalidation trace events carry data.nodeId (a Blink backend node
// id); this names the element so a per-frame repainter can be attributed.
import { connectPage, done } from './lib/cdp.mjs';

const ids = process.argv.slice(2).map(Number).filter((n) => Number.isFinite(n) && n > 0);
if (!ids.length) {
  console.error('usage: probe describenodes <backendNodeId> [...]');
  process.exit(2);
}

const page = await connectPage();
try {
  await page.send('DOM.enable');
  await page.send('DOM.getDocument', { depth: 0 });
  for (const id of ids) {
    try {
      const { node } = await page.send('DOM.describeNode', { backendNodeId: id });
      const attrs = {};
      const a = node.attributes || [];
      for (let i = 0; i < a.length; i += 2) attrs[a[i]] = a[i + 1];
      const label = [
        node.nodeName.toLowerCase(),
        attrs.id ? `#${attrs.id}` : '',
        attrs.class ? `.${attrs.class.split(/\s+/).slice(0, 6).join('.')}` : '',
        attrs['data-testid'] ? `[data-testid=${attrs['data-testid']}]` : '',
      ].join('');
      console.log(`${id}: ${label}`);
    } catch (e) {
      console.log(`${id}: (${e.message})`);
    }
  }
} finally {
  await done([page]);
}
