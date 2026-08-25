// Spike round 3: does `mask: url(#id)` on an HTML div actually PAINT the mask?
// Mounts a visible 24px div (green, masked to its left half) at the viewport corner,
// screenshots, and samples pixels inside vs outside the mask. Soak rig only.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

if ((process.env.AO_CDP_PORT || '9223') === '9223') {
  console.error('spritecheck3 mutates the page; AO_CDP_PORT=9224 (soak) required.');
  process.exit(2);
}

const page = await connectPage();
try {
  await page.send('Page.enable').catch(() => {});
  await evaluate(page, `(() => {
    const ns = 'http://www.w3.org/2000/svg';
    const host = document.createElement('div');
    host.id = 'ao-sc3';
    host.style.cssText = 'position:fixed;left:0;top:0;z-index:2147483647;width:24px;height:24px;background:#fff';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('width', '0'); svg.setAttribute('height', '0');
    svg.style.position = 'absolute';
    const mask = document.createElementNS(ns, 'mask');
    mask.id = 'ao-sc3-mask';
    mask.setAttribute('maskUnits', 'objectBoundingBox');
    mask.setAttribute('maskContentUnits', 'objectBoundingBox');
    const r = document.createElementNS(ns, 'rect');
    r.setAttribute('x', '0'); r.setAttribute('y', '0');
    r.setAttribute('width', '0.5'); r.setAttribute('height', '1');
    r.setAttribute('fill', 'white');
    mask.appendChild(r); svg.appendChild(mask);
    host.appendChild(svg);
    const d = document.createElement('div');
    d.style.cssText = 'width:24px;height:24px;background:rgb(10,200,30);' +
      '-webkit-mask:url(#ao-sc3-mask);mask:url(#ao-sc3-mask)';
    host.appendChild(d);
    document.body.appendChild(host);
    return 'mounted';
  })()`);
  await sleep(600);
  const shot = await page.send('Page.captureScreenshot', { format: 'png', fromSurface: true });
  const verdict = await evaluate(page, `new Promise((resolve) => {
    const img = new Image();
    img.onload = () => {
      const dpr = window.devicePixelRatio || 1;
      const c = document.createElement('canvas');
      c.width = Math.ceil(24 * dpr); c.height = Math.ceil(24 * dpr);
      const ctx = c.getContext('2d');
      ctx.drawImage(img, 0, 0);
      const px = (x, y) => Array.from(ctx.getImageData(Math.round(x * dpr), Math.round(y * dpr), 1, 1).data);
      resolve(JSON.stringify({ inside: px(6, 12), outside: px(18, 12) }));
    };
    img.onerror = () => resolve('"decode failed"');
    img.src = 'data:image/png;base64,${shot.data}';
  })`);
  await evaluate(page, `document.getElementById('ao-sc3')?.remove(), 'cleaned'`);
  const v = JSON.parse(verdict);
  console.log(`pixel inside mask (expect green ~[10,200,30]): ${JSON.stringify(v.inside ?? v)}`);
  console.log(`pixel outside mask (expect white/background): ${JSON.stringify(v.outside ?? v)}`);
  if (Array.isArray(v.inside)) {
    const greenIn = v.inside[1] > 150 && v.inside[0] < 80;
    const greenOut = v.outside[1] > 150 && v.outside[0] < 80;
    console.log(greenIn && !greenOut ? 'VERDICT: element mask paints correctly.'
      : !greenIn && !greenOut ? 'VERDICT: nothing painted — mask reference failed (element hidden).'
      : greenIn && greenOut ? 'VERDICT: mask ignored — painted unmasked.'
      : 'VERDICT: inverted/unexpected.');
  }
} finally {
  await done([page]);
}
