// Spike round 4: the EXACT production form — `mask: var(--mask-icon) center / contain no-repeat`
// where --mask-icon is url(#id) to a mask-type:alpha <mask> holding black lucide-style strokes.
// A parse failure would kill every icon in the app, so this must paint before the conversion ships.
// Soak rig only.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

if ((process.env.AO_CDP_PORT || '9223') === '9223') {
  console.error('spritecheck4 mutates the page; AO_CDP_PORT=9224 (soak) required.');
  process.exit(2);
}

const page = await connectPage();
try {
  await page.send('Page.enable').catch(() => {});
  await evaluate(page, `(() => {
    const ns = 'http://www.w3.org/2000/svg';
    const host = document.createElement('div');
    host.id = 'ao-sc4';
    host.style.cssText = 'position:fixed;left:0;top:0;z-index:2147483647;width:24px;height:24px;background:#fff';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('width', '0'); svg.setAttribute('height', '0');
    svg.style.position = 'absolute';
    const mask = document.createElementNS(ns, 'mask');
    mask.id = 'ao-sc4-mask';
    mask.setAttribute('maskUnits', 'objectBoundingBox');
    mask.setAttribute('maskContentUnits', 'objectBoundingBox');
    mask.style.maskType = 'alpha';
    // lucide-style content: 24-unit space, black stroke, scaled to the unit square
    const g = document.createElementNS(ns, 'g');
    g.setAttribute('transform', 'scale(0.0416666667)');
    g.setAttribute('fill', 'none');
    g.setAttribute('stroke', 'black');
    g.setAttribute('stroke-width', '2');
    g.setAttribute('stroke-linecap', 'round');
    const line = document.createElementNS(ns, 'line');
    line.setAttribute('x1', '4'); line.setAttribute('y1', '12');
    line.setAttribute('x2', '20'); line.setAttribute('y2', '12');
    g.appendChild(line); mask.appendChild(g); svg.appendChild(mask);
    host.appendChild(svg);
    const d = document.createElement('div');
    d.style.setProperty('--mask-icon', 'url(#ao-sc4-mask)');
    d.style.cssText += ';width:24px;height:24px;background-color:rgb(10,200,30);' +
      '-webkit-mask:var(--mask-icon) center / contain no-repeat;mask:var(--mask-icon) center / contain no-repeat';
    host.appendChild(d);
    document.body.appendChild(host);
    const cs = getComputedStyle(d);
    return 'mask=' + (cs.maskImage || cs.webkitMaskImage);
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
      resolve(JSON.stringify({ onStroke: px(12, 12), offStroke: px(12, 4) }));
    };
    img.onerror = () => resolve('"decode failed"');
    img.src = 'data:image/png;base64,${shot.data}';
  })`);
  await evaluate(page, `document.getElementById('ao-sc4')?.remove(), 'cleaned'`);
  const v = JSON.parse(verdict);
  console.log(`pixel on the stroke (expect green): ${JSON.stringify(v.onStroke ?? v)}`);
  console.log(`pixel off the stroke (expect white): ${JSON.stringify(v.offStroke ?? v)}`);
  if (Array.isArray(v.onStroke)) {
    const g1 = v.onStroke[1] > 150 && v.onStroke[0] < 80;
    const g2 = v.offStroke[1] > 150 && v.offStroke[0] < 80;
    console.log(g1 && !g2 ? 'VERDICT: production form paints (alpha mask, var + position/size longhands).'
      : !g1 && !g2 ? 'VERDICT: nothing painted — shorthand or alpha mask-type failed.'
      : 'VERDICT: mask ignored or inverted.');
  }
} finally {
  await done([page]);
}
