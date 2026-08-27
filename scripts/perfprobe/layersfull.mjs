// Full composited-layer census: EVERY layer with size, compositing reasons,
// and owning element — for attributing Overlap/squash artifacts to code.
// usage: probe layersfull
import { connectPage, done, sleep } from './lib/cdp.mjs';
import { pad, fail } from './lib/format.mjs';

const c = await connectPage();
await c.send('DOM.enable');
await c.send('DOM.getDocument', { depth: 0 });
let layers = null;
c.on((e) => { if (e.method === 'LayerTree.layerTreeDidChange' && e.params.layers) layers = e.params.layers; });
await c.send('LayerTree.enable');
for (let i = 0; i < 30 && !layers; i++) await sleep(100);
if (!layers) fail('probe: no layer tree event arrived');
const px = (n) => (n / 1e6).toFixed(2) + 'Mpx';
const describe = async (l) => {
  if (!l.backendNodeId) return '(no node)';
  try {
    const { object } = await c.send('DOM.resolveNode', { backendNodeId: l.backendNodeId });
    const r = await c.send('Runtime.callFunctionOn', {
      objectId: object.objectId,
      returnByValue: true,
      functionDeclaration: `function(){ const c=(this.getAttribute&&this.getAttribute('class'))||''; const rect=this.getBoundingClientRect(); return this.tagName.toLowerCase()+(this.id?'#'+this.id:'')+(c?'.'+c.split(/\\s+/).slice(0,6).join('.'):'') + '  el=' + Math.round(rect.width)+'x'+Math.round(rect.height); }`,
    });
    return r.result.value;
  } catch (e) { return '(resolve failed)'; }
};
console.log(`== ${layers.length} layers (${layers.filter((l) => l.drawsContent).length} draw content)`);
for (const l of [...layers].sort((a, b) => b.width * b.height - a.width * a.height)) {
  let reasons = [];
  try {
    const r = await c.send('LayerTree.compositingReasons', { layerId: l.layerId });
    reasons = r.compositingReasonIds || r.compositingReasons || [];
  } catch {}
  const desc = await describe(l);
  console.log(`${pad(l.width + 'x' + l.height, 11)} ${pad(px(l.width * l.height), 9)} ${l.drawsContent ? 'draw' : '    '} [${reasons.join(',')}] ${desc}`);
}
await done([c]);
