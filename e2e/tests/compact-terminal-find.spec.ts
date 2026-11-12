// Find in the thread terminal under the compact layout: the tab strip's
// find button has the compact hit target, the bar floats over the top of the
// terminal with the on-screen key row still under it, and a query counts the shell's
// lines. The browser's Back (the ladder utils/stepBack.ts runs) closes the
// bar first and leaves the terminal open, whether focus is in the bar or
// back in the terminal; the next Back closes the terminal. Desktop
// coverage: terminal-find.spec.ts. The key row while find has focus is the
// second test; its Paste, through the real clipboard, the third.
import { expect, test } from './fixtures.js';
import { terminalInput } from './terminal-thread-lifetime-flow.js';
import { QUERY, drawer, openDrawerThread, printTangos, replayOf } from './terminal-find-flow.js';

test('Back closes the terminal\'s find before the terminal', async ({ page }) => {
  test.setTimeout(120_000);
  const setup = await openDrawerThread(page, 'Phone terminal find');
  try {
    const screen = page.locator('html');
    await expect(screen).toHaveAttribute('data-compact-screen', 'thread');
    await printTangos(page, setup);
    const keyRow = page.getByTestId('terminal-key-row');
    await expect(keyRow).toBeVisible();

    const button = page.getByTestId('terminal-find');
    const box = (await button.boundingBox())!;
    expect(box.height).toBeGreaterThanOrEqual(36);
    expect(box.width).toBeGreaterThanOrEqual(36);
    await button.click();
    const bar = drawer(page).getByTestId('pane-find-bar');
    await expect(bar).toBeVisible();
    await expect(bar).toBeInViewport({ ratio: 1 });
    await expect(bar.getByTestId('pane-find-input')).toBeFocused();
    await page.keyboard.type(QUERY);
    await expect(bar.getByTestId('pane-find-count')).toHaveText('3/3');
    await expect(keyRow).toBeVisible();
    const barBox = (await bar.boundingBox())!;
    expect((await keyRow.boundingBox())!.y).toBeGreaterThan(barBox.y + barBox.height);

    await page.goBack();
    await expect(bar).toHaveCount(0);
    await expect(drawer(page)).toBeVisible();
    await expect(screen).toHaveAttribute('data-compact-screen', 'thread');

    // From the terminal itself, too.
    await button.click();
    await expect(bar).toBeVisible();
    await terminalInput(page).focus();
    await page.goBack();
    await expect(bar).toHaveCount(0);
    await expect(drawer(page)).toBeVisible();

    await page.goBack();
    await expect(drawer(page)).toHaveCount(0);
    await expect(screen).toHaveAttribute('data-compact-screen', 'thread');
  } finally {
    await setup.host.close();
  }
});

// While the find input has focus the key row is the query's: a character
// key types into it, Esc closes the bar, the other keys do nothing, and the
// shell receives none of it. Back in the terminal the row is the shell's.
test('the key row types into the find query, never the shell, while find has focus', async ({ page }) => {
  test.setTimeout(120_000);
  const setup = await openDrawerThread(page, 'Phone terminal find keys');
  try {
    await printTangos(page, setup);
    const key = (id: string) => page.getByTestId(`terminal-key-${id}`);
    // What the shell printed after the tangos, without its prompt, whose
    // `sh-5.2$` would match the dash below (Up would recall the printf line).
    const after = async () => (await replayOf(setup.host, setup.threadId))
      .split('tango-z').at(-1)!
      .replace(/sh-[\d.]+\$/g, '');

    await page.getByTestId('terminal-find').click();
    const bar = drawer(page).getByTestId('pane-find-bar');
    const input = bar.getByTestId('pane-find-input');
    await expect(input).toBeFocused();
    await page.keyboard.type('tango');
    await key('dash').click();
    await expect(input).toHaveValue(QUERY);
    await expect(input).toBeFocused();
    await expect(bar.getByTestId('pane-find-count')).toHaveText('3/3');
    for (const id of ['up', 'tab', 'ctrl']) {
      await expect(key(id)).toHaveAttribute('aria-disabled', 'true');
      // Playwright waits on aria-disabled; the tap still lands on a phone.
      await key(id).click({ force: true });
    }
    await expect(input).toHaveValue(QUERY);
    await expect(input).toBeFocused();

    await key('esc').click();
    await expect(bar).toHaveCount(0);
    await expect(terminalInput(page)).toBeFocused();
    // The shell saw none of it: no character, no recalled line.
    const quiet = await after();
    expect(quiet).not.toMatch(/[-~/]|tango|printf/);

    await expect(key('slash')).toHaveAttribute('aria-disabled', 'false');
    await key('slash').click();
    await expect.poll(after).toContain('/');
    expect(await after()).not.toMatch(/[-~]|printf/);
  } finally {
    await setup.host.close();
  }
});

// The key row's Paste reads the real clipboard: into the query while find
// has focus, and into the shell once the terminal has it back.
test('the key row pastes into the find query, never the shell, while find has focus', async ({ page }) => {
  test.setTimeout(120_000);
  const setup = await openDrawerThread(page, 'Phone terminal find paste');
  try {
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: new URL(page.url()).origin });
    await printTangos(page, setup);
    const after = async () => (await replayOf(setup.host, setup.threadId)).split('tango-z').at(-1)!;
    await page.evaluate(() => navigator.clipboard.writeText('tango-'));

    await page.getByTestId('terminal-find').click();
    const bar = drawer(page).getByTestId('pane-find-bar');
    const input = bar.getByTestId('pane-find-input');
    await expect(input).toBeFocused();
    const paste = page.getByTestId('terminal-key-paste');
    await paste.click();
    await expect(input).toHaveValue(QUERY);
    await expect(input).toBeFocused();
    await expect(bar.getByTestId('pane-find-count')).toHaveText('3/3');
    expect(await after()).not.toContain('tango');

    await page.getByTestId('terminal-key-esc').click();
    await expect(bar).toHaveCount(0);
    await expect(terminalInput(page)).toBeFocused();
    expect(await after()).not.toContain('tango');
    // The same tap with the terminal focused reaches the shell: the
    // clipboard is real, so the query's paste is a routing result.
    await paste.click();
    await expect.poll(after).toContain('tango-');
  } finally {
    await setup.host.close();
  }
});
