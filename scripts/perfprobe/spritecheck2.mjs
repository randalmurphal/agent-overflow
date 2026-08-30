// Spike round 2: (a) blob-URL sprite fragments — shared SVGImage doc or not;
// (b) same-document mask reference `mask: url(#id)` to an inline <svg><mask> — renders at all?
// MUTATES the page — soak rig only (AO_CDP_PORT=9224).
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

if ((process.env.AO_CDP_PORT || '9223') === '9223') {
  console.error('spritecheck2 mutates the page; AO_CDP_PORT=9224 (soak) required.');
  process.exit(2);
}

const page = await connectPage();
try {
  await page.send('Memory.enable').catch(() => {});
  const counters = async () => (await page.send('Memory.getDOMCounters')).documents;

  // (a) blob sprite, two fragment views
  const b0 = await counters();
  await evaluate(page, `(() => {
    const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="48" height="24">' +
      '<view id="fa" viewBox="0 0 24 24"/><view id="fb" viewBox="24 0 24 24"/>' +
      '<rect x="2" y="2" width="20" height="20" fill="black"/>' +
      '<circle cx="36" cy="12" r="10" fill="black"/></svg>';
    const uri = URL.createObjectURL(new Blob([svg], { type: 'image/svg+xml' }));
    window.__aoSpriteBlob = uri;
    const host = document.createElement('div');
    host.id = 'ao-spritecheck2';
    host.style.cssText = 'position:fixed;left:-200px;top:0';
    for (const frag of ['fa', 'fb']) {
      const d = document.createElement('div');
      d.className = 'ao-sc2-' + frag;
      d.style.cssText = 'width:24px;height:24px;background:#888;' +
        '-webkit-mask-image:url("' + uri + '#' + frag + '");mask-image:url("' + uri + '#' + frag + '");' +
        '-webkit-mask-size:contain;mask-size:contain';
      host.appendChild(d);
    }
    document.body.appendChild(host);
    return 'blob mounted';
  })()`);
  await sleep(1500);
  const b1 = await counters();

  // (b) same-document <mask> reference on an HTML div
  const painted = await evaluate(page, `(() => {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('width', '0'); svg.setAttribute('height', '0');
    svg.style.position = 'absolute';
    const mask = document.createElementNS(ns, 'mask');
    mask.id = 'ao-sc2-mask';
    mask.setAttribute('maskUnits', 'objectBoundingBox');
    mask.setAttribute('maskContentUnits', 'objectBoundingBox');
    const r = document.createElementNS(ns, 'rect');
    r.setAttribute('x', '0.1'); r.setAttribute('y', '0.1');
    r.setAttribute('width', '0.5'); r.setAttribute('height', '0.8');
    r.setAttribute('fill', 'white');
    mask.appendChild(r); svg.appendChild(mask);
    const host = document.getElementById('ao-spritecheck2');
    host.appendChild(svg);
    const d = document.createElement('div');
    d.id = 'ao-sc2-el';
    d.style.cssText = 'width:24px;height:24px;background:rgb(10,200,30);' +
      '-webkit-mask:url(#ao-sc2-mask);mask:url(#ao-sc2-mask)';
    host.appendChild(d);
    return 'element mask mounted';
  })()`);
  await sleep(800);
  const b2 = await counters();

  // Verify (b) reached the compositor without a pixel readback. The resolved
  // computed style is the observable contract for this resource-only probe.
  const style = await evaluate(page, `(() => {
    const el = document.getElementById('ao-sc2-el');
    const cs = getComputedStyle(el);
    return cs.maskImage || cs.webkitMaskImage;
  })()`);

  await evaluate(page, `(() => {
    document.getElementById('ao-spritecheck2')?.remove();
    if (window.__aoSpriteBlob) { URL.revokeObjectURL(window.__aoSpriteBlob); delete window.__aoSpriteBlob; }
    return 'cleaned';
  })()`);

  console.log(`documents: before=${b0} after-blob-two-fragments=${b1} after-element-mask=${b2}`);
  console.log(`blob two fragments added ${b1 - b0} document(s); element-mask added ${b2 - b1} (expect 0).`);
  console.log(`element mask computed style: ${style}`);
} finally {
  await done([page]);
}
