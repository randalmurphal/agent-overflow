// Hold or release a persistent CSS override in the page, so another probe
// (frames, tiles, layers) can measure UNDER the override. Complements ab,
// which owns its override's lifetime and can only measure allocation.
// usage: probe csshold on --css '<css>' [--allow-user-app]
//        probe csshold off
import { connectPage, done, evaluate, PORT } from './lib/cdp.mjs';
import { fail } from './lib/format.mjs';

const args = process.argv.slice(2);
const mode = args[0];
const idx = args.indexOf('--css');
const CSS = idx >= 0 ? args[idx + 1] : null;
if (mode !== 'on' && mode !== 'off') fail("probe: csshold needs 'on' or 'off' as its first argument");
if (mode === 'on' && !CSS) fail("probe: csshold on needs --css '<css>'");
// Same guard as ab: a held override is visible to whoever is using the app.
if (mode === 'on' && PORT === '9223' && !args.includes('--allow-user-app')) {
  fail('probe: csshold changes what the user sees on port 9223.\n'
    + '       Run it against the soak rig (AO_CDP_PORT=9224) or pass --allow-user-app.');
}

const STYLE_ID = '__ao_perfprobe_csshold';
const p = await connectPage();
if (mode === 'on') {
  await evaluate(p, `(() => { let s = document.getElementById(${JSON.stringify(STYLE_ID)}); if (!s) { s = document.createElement('style'); s.id = ${JSON.stringify(STYLE_ID)}; document.head.appendChild(s); } s.textContent = ${JSON.stringify(CSS)}; })()`);
  console.log(`csshold: override HELD on port ${PORT} (release with: probe csshold off)`);
} else {
  const had = await evaluate(p, `(() => { const s = document.getElementById(${JSON.stringify(STYLE_ID)}); if (s) { s.remove(); return true; } return false; })()`);
  console.log(had ? 'csshold: override released' : 'csshold: nothing held');
}
await done();
