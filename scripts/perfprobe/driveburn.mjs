// MUTATES the app: mounts a thread in a pane (ctrl+click) if needed, then sends a prompt there.
// usage: AO_CDP_PORT=9224 probe driveburn [thread-title] [prompt] [--mount-only]
// Built for the soak rig's multi-pane burn leg: opening the second seeded thread beside the
// armed one and prompting it makes BOTH panes stream the installed scenario at once.
import { connectPage, evaluate, sleep, done } from './lib/cdp.mjs';

if ((process.env.AO_CDP_PORT || '9223') === '9223' && !process.argv.includes('--allow-user-app')) {
  console.error('driveburn: this probe clicks and types in the app. Run it against the soak rig');
  console.error('           (AO_CDP_PORT=9224), or pass --allow-user-app to drive your own window.');
  process.exit(2);
}

const args = process.argv.slice(2);
const mountOnly = args.includes('--mount-only');
const positional = args.filter((arg) => arg !== '--mount-only' && arg !== '--allow-user-app');
const title = positional[0] || 'Soak: idle thread';
const prompt = positional[1] || 'Run the burn here too.';

// The composer textarea living in the pane whose header carries `title`.
// Stay inside the pane section. Climbing toward body makes the shared layout
// match any sidebar title and silently drives the first composer instead.
const findComposer = `(() => {
  const panes = [...document.querySelectorAll('section[data-pane-id]')];
  for (const pane of panes) {
    const heading = pane.querySelector('[data-testid="chat-header-title"]');
    if (heading?.textContent?.trim() !== ${JSON.stringify(title)}) continue;
    const area = pane.querySelector('textarea');
    if (!area) return null;
    const r = area.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) {
      return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
    }
  }
  return null;
})()`;

const page = await connectPage();
try {
  let composer = await evaluate(page, findComposer);
  if (!composer) {
    // Not mounted: ctrl+click the sidebar row (modifiers bit 2 = Ctrl) =
    // openThreadInNewPane per ThreadRow.svelte.
    const row = await evaluate(page, `(() => {
      const nodes = [...document.querySelectorAll('aside [role="button"], aside a, aside div, aside li')];
      const hit = nodes.filter((el) => el.childElementCount < 8
        && el.textContent.includes(${JSON.stringify(title)}))
        .sort((a, b) => a.textContent.length - b.textContent.length)[0];
      if (!hit) return null;
      const r = hit.getBoundingClientRect();
      return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
    })()`);
    if (!row) throw new Error(`driveburn: no sidebar row containing ${JSON.stringify(title)}`);
    for (const type of ['mousePressed', 'mouseReleased']) {
      await page.send('Input.dispatchMouseEvent', {
        type, x: row.x, y: row.y, button: 'left', clickCount: 1, modifiers: 2,
      });
    }
    await sleep(1500);
    composer = await evaluate(page, findComposer);
    if (!composer) throw new Error(`driveburn: pane for ${JSON.stringify(title)} did not mount`);
    console.log('mounted thread in new pane');
  }

  if (mountOnly) {
    console.log('mount-only: composer left untouched');
  } else {
    // A real click focuses the composer, then real input events carry the text.
    for (const type of ['mousePressed', 'mouseReleased']) {
      await page.send('Input.dispatchMouseEvent', {
        type, x: composer.x, y: composer.y, button: 'left', clickCount: 1,
      });
    }
    await sleep(200);
    await page.send('Input.insertText', { text: prompt });
    await sleep(300);
    for (const type of ['keyDown', 'keyUp']) {
      await page.send('Input.dispatchKeyEvent', {
        type, key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13,
        text: '\r', unmodifiedText: '\r',
      });
    }
    await sleep(1000);
    const rest = await evaluate(page, `document.activeElement?.value ?? ''`);
    console.log(rest === '' ? 'prompt sent (composer cleared)' : `composer still holds: ${JSON.stringify(rest)}`);
  }
} finally {
  await done([page]);
}
