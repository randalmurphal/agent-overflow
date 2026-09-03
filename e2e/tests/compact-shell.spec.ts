// The compact layout under a real phone descriptor: the same bundle the
// desktop project drives, with the viewport deciding the mode. These
// cases prove the shell's contract (frontend/AGENTS.md § Compact): the
// list is the root screen, opening a thread swaps to the thread screen
// without unmounting either, the header's back button returns, Return
// is a newline, a menu opens as a bottom sheet, a long press or the row's
// own menu button opens a row menu without opening the thread, and the
// header carries the command palette.
import type { Locator, Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';

test.beforeEach(async ({ harness }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'compact-app',
        repo: {},
        threads: [
          {
            title: 'First task',
            turns: [{ userText: 'one', items: [{ kind: 'assistant_text', summary: 'one done' }] }],
          },
          {
            title: 'Second task',
            turns: [{ userText: 'two', items: [{ kind: 'assistant_text', summary: 'two done' }] }],
          },
        ],
      },
    ],
  });
});

test('the viewport picks compact and the list is the root screen', async ({ harness, page }) => {
  await harness.open(page);
  await expect(page.locator('html')).toHaveClass(/layout-compact/);
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
  await expect(page.getByTestId('sidebar')).toBeVisible();
  await expect(page.getByTestId('thread-row')).toHaveCount(2);
  // The pane strip is mounted but not showing: visibility, not display.
  const host = page.getByTestId('pane-host');
  await expect(host).toBeAttached();
  await expect(host).toBeHidden();
  await expect(host).toHaveAttribute('inert', '');
  await expect(page.getByTestId('sidebar-rail')).toHaveCount(0);
  await expect(page.getByTestId('sidebar-resizer')).toHaveCount(0);
});

test('opening a thread swaps to the thread screen and back returns to the list', async ({
  harness,
  page,
}) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.getByTestId('pane-host')).toBeVisible();
  await expect(page.getByTestId('chat-header-title')).toHaveText('First task');
  await expect(page.getByTestId('sidebar')).toBeHidden();
  await expect(page.getByTestId('sidebar')).toHaveAttribute('inert', '');
  await expect(page.getByTestId('pane-close')).toHaveCount(0);
  await expect(page.getByTestId('pane-divider')).toHaveCount(0);
  // Desktop affordances that have no phone equivalent are gone.
  await expect(page.getByTestId('message-nav-rail')).toHaveCount(0);
  const pane = page.locator('section[data-pane-id]').first();
  const widths = await pane.evaluate((el) => [
    el.getBoundingClientRect().width,
    el.parentElement!.getBoundingClientRect().width,
  ]);
  expect(widths[0]).toBe(widths[1]);

  await page.getByTestId('compact-back').click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
  await expect(page.getByTestId('sidebar')).toBeVisible();
  // Still mounted with the thread in it: the way back is a visibility flip.
  await expect(page.getByTestId('chat-header-title')).toBeAttached();
  await expect(page.getByTestId('chat-header-title')).toHaveText('First task');

  // Tapping the already-open thread reveals it again.
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
});

test('Return inserts a newline and Send is the way to send', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Second task' }).click();
  const input = page.getByLabel('Message Input');
  await input.fill('first line');
  await input.press('Enter');
  await input.type('second line');
  await expect(input).toHaveValue('first line\nsecond line');
  // Nothing was sent: the thread still has its one turn.
  await expect(page.getByTestId('composer-send')).toBeEnabled();
  await expect(page.getByTestId('composer-interrupt')).toHaveCount(0);
});

test('a menu opens as a bottom sheet', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  await page.getByTestId('composer-model-menu-trigger').click();
  const sheet = page.locator('[data-popover-sheet]');
  await expect(sheet).toBeVisible();
  await expect(sheet).toHaveAttribute('data-placement', 'sheet');
  const box = await sheet.evaluate((el) => {
    const r = el.getBoundingClientRect();
    return { left: r.left, right: r.right, bottom: r.bottom, vw: innerWidth, vh: innerHeight };
  });
  expect(box.left).toBe(0);
  expect(box.right).toBe(box.vw);
  expect(box.bottom).toBe(box.vh);
});

/**
 * A held touch, the way a device produces one: raw touch events through
 * CDP, so the app's own long-press detector (utils/longPressContextMenu.ts)
 * is what turns it into a menu. Playwright's `tap` is a tap, and no engine
 * under emulation raises `contextmenu` for a hold on its own.
 */
async function longPress(page: Page, target: Locator): Promise<void> {
  const box = await target.boundingBox();
  if (!box) throw new Error('long-press target is not visible');
  const x = box.x + box.width / 2;
  const y = box.y + box.height / 2;
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
    await page.waitForTimeout(700);
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  } finally {
    await cdp.detach();
  }
}

test('a long press on a thread row opens its menu as a sheet and leaves the thread closed', async ({
  harness,
  page,
}) => {
  await harness.open(page);
  const row = page.getByTestId('thread-row').filter({ hasText: 'First task' });
  await longPress(page, row);
  const sheet = page.locator('[data-popover-sheet]');
  await expect(sheet).toBeVisible();
  await expect(sheet.getByRole('menu', { name: 'Thread Actions' })).toBeVisible();
  await expect(sheet.getByRole('menuitem', { name: 'Rename Thread' })).toBeVisible();
  // The release did not open the thread under the sheet, and the sheet
  // survived the release.
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
  await page.keyboard.press('Escape');
  await expect(sheet).toHaveCount(0);
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
});

test('the row menu button is the visible way into the same menu', async ({ harness, page }) => {
  await harness.open(page);
  const row = page.getByTestId('thread-row').filter({ hasText: 'Second task' });
  const button = row.getByTestId('thread-row-menu');
  await expect(button).toBeVisible();
  const size = await button.boundingBox();
  expect(size!.width).toBeGreaterThanOrEqual(36);
  expect(size!.height).toBeGreaterThanOrEqual(36);
  await button.tap();
  const sheet = page.locator('[data-popover-sheet]');
  await expect(sheet.getByRole('menuitem', { name: 'Rename Thread' })).toBeVisible();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
});

test('the project header carries its menu, with New Terminal inside', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('project-item-menu').first().tap();
  const sheet = page.locator('[data-popover-sheet]');
  await expect(sheet.getByRole('menuitem', { name: 'New Terminal' })).toBeVisible();
  await expect(sheet.getByRole('menuitem', { name: 'Rename Project' })).toBeVisible();
});

test('the chat header opens the command palette', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  await page.getByTestId('palette-open').tap();
  await expect(page.getByTestId('command-palette-input')).toBeVisible();
});
