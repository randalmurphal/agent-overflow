// Find in the thread terminal, desktop. Ctrl+F in the focused terminal is
// the shell's: it reaches the PTY and opens no find bar. The tab strip's
// find button opens a bar between the strip and the terminal with its input
// focused; a query counts the shell's lines, starts at the newest, paints
// them, and Enter / Shift+Enter step with wrap. The count follows output
// that arrives while the bar is open. Escape closes the bar, clears the
// paint and returns focus to the terminal. A second tab has its own find,
// and the first tab's open bar returns with it; hiding and reopening the
// drawer brings the open bar back on the same terminal, searched again.
// Compact and Back: compact-terminal-find.spec.ts. Unit, component and
// real-xterm coverage: terminalFind.svelte.test.ts,
// TerminalSurface.find.test.ts, terminalFind.browser.test.ts.
import { expect, test } from './fixtures.js';
import { terminalInput, terminalTabs } from './terminal-thread-lifetime-flow.js';
import { QUERY, drawer, findRectangles, openDrawerThread, printTangos, replayOf, runInShell } from './terminal-find-flow.js';

test('the terminal finds from its button, and ctrl+f stays the shell\'s', async ({ page }) => {
  test.setTimeout(120_000);
  const setup = await openDrawerThread(page, 'Terminal find');
  try {
    const { host, threadId } = setup;
    await printTangos(page, setup);

    // Ctrl+F in the focused terminal reaches the shell: `cat -v` prints it.
    await runInShell(page, setup, 'cat -v', 'cat -v');
    await terminalInput(page).focus();
    await page.keyboard.press('Control+f');
    await page.keyboard.press('Enter');
    await page.keyboard.press('Control+d');
    await expect.poll(() => replayOf(host, threadId)).toMatch(/\^F/);
    await expect(page.getByTestId('pane-find-bar')).toHaveCount(0);

    // The PTY's size, which opening and closing find must not change.
    const ptySize = async () => {
      const [shell] = await host.rpc<Array<{ rows: number; cols: number }>>('ListTerminals', threadId);
      return { rows: shell.rows, cols: shell.cols };
    };
    const sized = await ptySize();
    await page.getByTestId('terminal-find').click();
    const bar = drawer(page).getByTestId('pane-find-bar');
    await expect(bar).toBeVisible();
    const input = bar.getByTestId('pane-find-input');
    await expect(input).toBeFocused();
    await expect(input).toHaveAttribute('placeholder', 'Find in terminal');
    // Floating over the terminal's top right, below the tab strip, without
    // moving or resizing the terminal.
    const barBox = (await bar.boundingBox())!;
    const tabBox = (await terminalTabs(page).first().boundingBox())!;
    const termBox = (await drawer(page).locator('.xterm').boundingBox())!;
    expect(barBox.y).toBeGreaterThan(tabBox.y);
    expect(barBox.y).toBeGreaterThanOrEqual(termBox.y);
    expect(barBox.x + barBox.width).toBeLessThanOrEqual(termBox.x + termBox.width + 1);
    expect(barBox.width).toBeLessThan(termBox.width);
    expect(await ptySize()).toEqual(sized);

    await page.keyboard.type(QUERY);
    const count = bar.getByTestId('pane-find-count');
    await expect(count).toHaveText('3/3');
    // The three printed lines in view, the newest active.
    await expect(drawer(page).locator('.terminal-find-active')).toHaveCount(1);
    await expect.poll(() => drawer(page).locator('.terminal-find-match').count()).toBeGreaterThanOrEqual(2);
    await page.keyboard.press('Enter');
    await expect(count).toHaveText('1/3');
    await expect(drawer(page).locator('.terminal-find-active')).toHaveCount(1);
    await page.keyboard.press('Shift+Enter');
    await expect(count).toHaveText('3/3');

    // Output while the bar is open: the count follows it, and the active
    // match stays the one it was.
    const [shell] = await host.rpc<Array<{ terminalID: string }>>('ListTerminals', threadId);
    await host.rpc('WriteTerminal', shell.terminalID, Buffer.from("printf '%s-%s\\n' tango w\n").toString('base64'));
    await expect(count).toHaveText('3/4');
    await expect(input).toBeFocused();

    await page.keyboard.press('Escape');
    await expect(bar).toHaveCount(0);
    await expect(terminalInput(page)).toBeFocused();
    expect(await ptySize()).toEqual(sized);
    await expect.poll(() => findRectangles(page).count()).toBe(0);

    // A second tab has its own find; the first tab's comes back with it.
    await page.getByTestId('terminal-find').click();
    await expect(input).toHaveValue(QUERY);
    await expect(count).toHaveText(/^\s*\d\/4$/);
    await page.getByTestId('terminal-open').click();
    await expect(terminalTabs(page)).toHaveCount(2);
    await expect(drawer(page).getByTestId('pane-find-bar')).toHaveCount(0);
    await expect.poll(() => findRectangles(page).count()).toBe(0);
    await terminalTabs(page).first().click();
    await expect(bar).toBeVisible();
    await expect(input).toHaveValue(QUERY);
    await expect(count).toHaveText(/^\s*\d\/4$/);

    // Hidden and reopened, the drawer's open bar is the same terminal's. A
    // reopen activates the newest tab, which has no find open.
    await page.getByTestId('terminal-collapse').click();
    await expect(drawer(page)).toHaveCount(0);
    await page.getByTestId('terminal-toggle').click();
    await expect(terminalTabs(page)).toHaveCount(2);
    await expect(terminalTabs(page).last()).toHaveAttribute('aria-selected', 'true');
    await expect(drawer(page).getByTestId('pane-find-bar')).toHaveCount(0);
    await terminalTabs(page).first().click();
    await expect(bar).toBeVisible();
    await expect(input).toHaveValue(QUERY);
    await expect(count).toHaveText(/^\s*\d\/4$/);
    await expect.poll(() => findRectangles(page).count()).toBeGreaterThan(0);
  } finally {
    await setup.host.close();
  }
});
