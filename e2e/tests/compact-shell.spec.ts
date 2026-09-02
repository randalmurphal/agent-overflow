// The compact layout under a real phone descriptor: the same bundle the
// desktop project drives, with the viewport deciding the mode. These
// cases prove the shell's contract (frontend/AGENTS.md § Compact): the
// list is the root screen, opening a thread swaps to the thread screen
// without unmounting either, the header's back button returns, Return
// is a newline, and a menu opens as a bottom sheet.
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
