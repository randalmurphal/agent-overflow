// Verify controller planes stay on WebView2's browser-managed paint path.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

const page = await connectPage();
let code = 0;

try {
  const surfaces = await evaluate(page, `(() => {
    const describe = (kind, selector, required) => ({
      kind,
      selector,
      required,
      elements: [...document.querySelectorAll(selector)].map((element, index) => {
        const style = getComputedStyle(element);
        return {
          index,
          willChange: style.willChange,
          transform: style.transform,
          translate: style.translate,
          rotate: style.rotate,
          scale: style.scale,
          clientHeight: element.clientHeight,
          scrollHeight: element.scrollHeight,
        };
      }),
    });
    return [
      describe('timeline plane', '[data-virtual-row-plane]', true),
      describe('virtual row', '[data-virtual-row]', true),
      describe('activity-run content', '[data-testid="activity-run-clip"] > div', false),
      describe('timeline scroller', '[data-testid="message-timeline-scroll"]', true),
    ];
  })()`);

  const failures = [];
  for (const surface of surfaces) {
    if (surface.required && surface.elements.length === 0) {
      failures.push(`${surface.kind}: no matching element (${surface.selector})`);
    }
    if (surface.kind === 'timeline scroller') continue;
    for (const element of surface.elements) {
      const authored = [
        ['will-change', element.willChange, 'auto'],
        ['transform', element.transform, 'none'],
        ['translate', element.translate, 'none'],
        ['rotate', element.rotate, 'none'],
        ['scale', element.scale, 'none'],
      ].filter(([, actual, expected]) => actual !== expected);
      if (authored.length > 0) {
        failures.push(
          `${surface.kind}[${element.index}] authors compositor state: `
          + authored.map(([name, actual]) => `${name}=${actual}`).join(', '),
        );
      }
    }
  }

  await page.send('DOM.enable');
  const document = await page.send('DOM.getDocument', { depth: 0 });
  const backendOwners = new Map();
  for (const surface of surfaces) {
    const nodes = await page.send('DOM.querySelectorAll', {
      nodeId: document.root.nodeId,
      selector: surface.selector,
    });
    for (let index = 0; index < nodes.nodeIds.length; index += 1) {
      const described = await page.send('DOM.describeNode', { nodeId: nodes.nodeIds[index] });
      backendOwners.set(described.node.backendNodeId, {
        kind: surface.kind,
        index,
        metrics: surface.elements[index],
      });
    }
  }

  let layers = null;
  page.on((event) => {
    if (event.method === 'LayerTree.layerTreeDidChange' && event.params.layers) {
      layers = event.params.layers;
    }
  });
  await page.send('LayerTree.enable');
  for (let attempt = 0; attempt < 30 && !layers; attempt += 1) await sleep(100);
  if (!layers) throw new Error('no LayerTree snapshot arrived');

  const layerOwners = [];
  for (const layer of layers) {
    if (!layer.drawsContent || !layer.backendNodeId) continue;
    const owner = backendOwners.get(layer.backendNodeId);
    if (!owner) continue;
    layerOwners.push({ owner, width: layer.width, height: layer.height });
    if (owner.kind !== 'timeline scroller') {
      failures.push(
        `${owner.kind}[${owner.index}] owns a ${layer.width}x${layer.height} drawing layer`,
      );
      continue;
    }
    const viewportHeight = owner.metrics?.clientHeight ?? 0;
    if (viewportHeight > 0 && layer.height > viewportHeight * 1.5) {
      failures.push(
        `timeline scroller[${owner.index}] owns a content-sized ${layer.width}x${layer.height} layer `
        + `(viewport ${viewportHeight}px, scrollHeight ${owner.metrics.scrollHeight}px)`,
      );
    }
  }

  console.log('WebView2 compositor contract');
  for (const surface of surfaces) {
    console.log(`  ${surface.kind}: ${surface.elements.length}`);
  }
  if (layerOwners.length === 0) {
    console.log('  owned drawing layers: none');
  } else {
    for (const layer of layerOwners) {
      console.log(
        `  owned drawing layer: ${layer.owner.kind}[${layer.owner.index}] ${layer.width}x${layer.height}`,
      );
    }
  }
  if (failures.length > 0) {
    throw new Error(failures.join('\n  '));
  }
} catch (error) {
  console.error(`probe: ${error.message}`);
  code = 1;
}

await done([page], code);
