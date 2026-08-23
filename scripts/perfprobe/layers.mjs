// Composited layer census: count, area, and which elements own the biggest layers.
// usage: probe layers
import { connectPage, done, sleep } from './lib/cdp.mjs';
import { pad, fail } from './lib/format.mjs';

const c = await connectPage();
await c.send('DOM.enable');
await c.send('DOM.getDocument', { depth: 0 });
let layers = null;
c.on((e) => { if (e.method === 'LayerTree.layerTreeDidChange' && e.params.layers) layers = e.params.layers; });
await c.send('LayerTree.enable');
for (let i = 0; i < 30 && !layers; i++) await sleep(100);
if (!layers) fail('probe: no layer tree event arrived (the page may be idle with nothing composited)');
const area = (l) => l.width * l.height;
const drawing = layers.filter((l) => l.drawsContent);
const px = (n) => (n / 1e6).toFixed(1) + 'Mpx';
console.log(`== layers: ${layers.length} total, ${drawing.length} draw content, total drawing area ${px(drawing.reduce((a, l) => a + area(l), 0))} (4 bytes/px at 1x; tiles cover the visible part plus prepaint margin)`);
for (const l of [...drawing].sort((a, b) => area(b) - area(a)).slice(0, 25)) {
  let desc = '(no node)';
  if (l.backendNodeId) {
    try {
      const { object } = await c.send('DOM.resolveNode', { backendNodeId: l.backendNodeId });
      const r = await c.send('Runtime.callFunctionOn', {
        objectId: object.objectId,
        returnByValue: true,
        functionDeclaration: `function(){ const c=(this.getAttribute&&this.getAttribute('class'))||''; return this.tagName.toLowerCase()+(this.id?'#'+this.id:'')+(c?'.'+c.split(/\\s+/).slice(0,5).join('.'):'') + ' wc=' + getComputedStyle(this).willChange; }`,
      });
      desc = r.result.value;
    } catch (e) { desc = '(resolve failed: ' + e.message + ')'; }
  }
  console.log(`${pad(l.width + 'x' + l.height, 11)} ${pad(px(area(l)), 9)}  ${desc}`);
}
const reasons = new Map();
for (const l of layers) {
  try {
    const r = await c.send('LayerTree.compositingReasons', { layerId: l.layerId });
    for (const k of r.compositingReasonIds || r.compositingReasons || []) reasons.set(k, (reasons.get(k) || 0) + 1);
  } catch {}
}
console.log('-- compositing reasons (layer count)');
for (const [k, n] of [...reasons.entries()].sort((a, b) => b[1] - a[1])) console.log(`${pad(n, 5)}  ${k}`);
await done([c]);
