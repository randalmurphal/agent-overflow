// MUTATES the app: clicks every visible "Interrupt current turn" stop button.
// usage: AO_CDP_PORT=9224 probe drivestop
import { connectPage, evaluate, sleep, done } from './lib/cdp.mjs';

if ((process.env.AO_CDP_PORT || '9223') === '9223' && !process.argv.includes('--allow-user-app')) {
  console.error('drivestop: this probe clicks in the app. Run it against the soak rig (AO_CDP_PORT=9224),');
  console.error('           or pass --allow-user-app to drive your own window.');
  process.exit(2);
}

const page = await connectPage();
try {
  for (let round = 0; round < 4; round += 1) {
    const spots = await evaluate(page, `(() => {
      return [...document.querySelectorAll('button[aria-label="Interrupt current turn"]')]
        .map((b) => { const r = b.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })
        .filter((p) => p.x > 0 && p.y > 0);
    })()`);
    if (spots.length === 0) { console.log(round === 0 ? 'no stop buttons visible' : 'all turns stopped'); break; }
    for (const p of spots) {
      for (const type of ['mousePressed', 'mouseReleased']) {
        await page.send('Input.dispatchMouseEvent', { type, x: p.x, y: p.y, button: 'left', clickCount: 1 });
      }
      console.log(`clicked stop at ${Math.round(p.x)},${Math.round(p.y)}`);
      await sleep(500);
    }
    await sleep(1500);
  }
} finally {
  await done([page]);
}
