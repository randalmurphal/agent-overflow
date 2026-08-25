// Spike: does Blink share ONE SVGImage document across mask-image url(...#fragA) / url(...#fragB)?
// Mounts two masked divs pointing at two <view> fragments of one data-URI sprite, diffs the
// Memory.getDOMCounters document count. MUTATES the page — soak rig only (AO_CDP_PORT=9224).
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';

if ((process.env.AO_CDP_PORT || '9223') === '9223') {
  console.error('spritecheck mutates the page; refuse the user app. AO_CDP_PORT=9224 (soak) required.');
  process.exit(2);
}

const page = await connectPage();
try {
  await page.send('Memory.enable').catch(() => {});
  const counters = async () => (await page.send('Memory.getDOMCounters')).documents;

  const before = await counters();

  // One sprite, two views. Encoded once; the two references differ only in fragment.
  await evaluate(page, `(() => {
    const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="48" height="24">' +
      '<view id="fa" viewBox="0 0 24 24"/><view id="fb" viewBox="24 0 24 24"/>' +
      '<rect x="2" y="2" width="20" height="20" fill="black"/>' +
      '<circle cx="36" cy="12" r="10" fill="black"/></svg>';
    const uri = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
    const host = document.createElement('div');
    host.id = 'ao-spritecheck';
    host.style.cssText = 'position:fixed;left:-200px;top:0';
    for (const frag of ['fa', 'fb']) {
      const d = document.createElement('div');
      d.style.cssText = 'width:24px;height:24px;background:#888;' +
        '-webkit-mask-image:url("' + uri + '#' + frag + '");mask-image:url("' + uri + '#' + frag + '");' +
        '-webkit-mask-size:contain;mask-size:contain';
      host.appendChild(d);
    }
    document.body.appendChild(host);
    return 'mounted';
  })()`);
  await sleep(1500);
  const after = await counters();

  // Control: a THIRD reference with a distinct data URI (whitespace tweak) must add one document.
  await evaluate(page, `(() => {
    const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="48" height="24" >' +
      '<view id="fa" viewBox="0 0 24 24"/>' +
      '<rect x="2" y="2" width="20" height="20" fill="black"/></svg>';
    const uri = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
    const d = document.createElement('div');
    d.style.cssText = 'width:24px;height:24px;background:#888;' +
      '-webkit-mask-image:url("' + uri + '#fa");mask-image:url("' + uri + '#fa")';
    document.getElementById('ao-spritecheck').appendChild(d);
    return 'control mounted';
  })()`);
  await sleep(1500);
  const control = await counters();

  await evaluate(page, `document.getElementById('ao-spritecheck')?.remove(), 'cleaned'`);

  console.log(`documents: before=${before} after-two-fragments=${after} after-distinct-control=${control}`);
  console.log(`two fragments of one sprite added ${after - before} document(s); a distinct URI added ${control - after}.`);
  console.log((after - before) <= 1 && (control - after) >= 1
    ? 'VERDICT: shared — one resource document per sprite URL, fragments free.'
    : 'VERDICT: NOT shared (or control failed) — read the numbers above.');
} finally {
  await done([page]);
}
