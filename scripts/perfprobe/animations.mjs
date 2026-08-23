// Census of running web animations (CSS animations/transitions/WAAPI) and their targets.
// usage: probe animations
import { connectPage, done, evaluate } from './lib/cdp.mjs';

const c = await connectPage();
const v = await evaluate(c, `(() => {
  const anims = document.getAnimations();
  const out = new Map();
  for (const a of anims) {
    const t = a.effect && a.effect.target;
    const cls = t ? (t.getAttribute('class') || '').split(/\\s+/).filter(x => /anim|spin|pulse|robo|sprite|glide|fade|shimmer|marquee|bounce|ping/.test(x)).slice(0, 3).join('.') : '';
    const name = a.animationName || a.transitionProperty || a.id || a.constructor.name;
    const k = a.constructor.name + ' ' + name + ' on ' + (t ? t.tagName.toLowerCase() + (cls ? '.' + cls : '') : '?') + ' [' + a.playState + ']';
    out.set(k, (out.get(k) || 0) + 1);
  }
  return { total: anims.length, live: document.querySelectorAll('[data-live="true"]').length, rows: [...out.entries()].sort((a, b) => b[1] - a[1]).slice(0, 30) };
})()`);
console.log(`animations=${v.total} data-live=true rows=${v.live}`);
for (const [k, n] of v.rows) console.log(`${String(n).padStart(5)}  ${k}`);
await done([c]);
