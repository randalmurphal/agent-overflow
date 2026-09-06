// The compact layout under a real phone descriptor: the same bundle the
// desktop project drives, with the viewport deciding the mode. These
// cases prove the shell's contract (frontend/AGENTS.md § Compact): the
// list is the root screen, opening a thread swaps to the thread screen
// without unmounting either, the header's back button returns, Return
// is a newline, a menu opens as a bottom sheet, a long press or the row's
// own menu button opens a row menu without opening the thread, the chat
// header rolls its actions into one sheet, the composer's densest rung
// keeps the meters and rolls the pickers into one sheet, and a viewport
// that shrinks under a pinned reader (the soft keyboard) keeps the tail
// on screen.
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
          {
            title: 'Long task',
            turns: Array.from({ length: 30 }, (_, i) => ({
              userText: `question ${i + 1}`,
              items: [{ kind: 'assistant_text', summary: `answer ${i + 1}, at some length so the thread outgrows a phone screen many times over.` }],
            })),
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
  await expect(page.getByTestId('thread-row')).toHaveCount(3);
  // The pane strip is mounted but not showing: visibility, not display.
  const host = page.getByTestId('pane-host');
  await expect(host).toBeAttached();
  await expect(host).toBeHidden();
  await expect(host).toHaveAttribute('inert', '');
  await expect(page.getByTestId('sidebar-rail')).toHaveCount(0);
  await expect(page.getByTestId('sidebar-resizer')).toHaveCount(0);
});

// The first real-phone session (Pixel 9a, 2026-09-04) opened a thread and
// could not send: the composer toolbar's densest rung still overflowed a
// phone's width and the clipped control was Send, at the row's right end.
// CDP clicks tap DOM nodes wherever they are, so no interaction test can
// catch a control a FINGER cannot reach — only geometry can, which is
// what this case asserts, in the state the phone hit (a locked thread,
// where the rate-limit meters join the row).
test('every composer control stays inside the phone viewport', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  const toolbar = page.getByTestId('composer-toolbar');
  await expect(toolbar).toBeVisible();

  // The toolbar may not overflow itself: an overflowing flex row clips
  // whatever sits at its end, silently.
  await expect
    .poll(() => toolbar.evaluate((el) => el.scrollWidth - el.clientWidth), {
      message: 'the composer toolbar must fit its width at phone size',
    })
    .toBeLessThanOrEqual(1);

  // And Send in particular is fully on screen.
  const send = page.getByTestId('composer-send');
  await expect(send).toBeVisible();
  const box = (await send.boundingBox())!;
  const viewport = page.viewportSize()!;
  expect(box.x, 'send must not hang off the left edge').toBeGreaterThanOrEqual(0);
  expect(box.x + box.width, 'send must not hang off the right edge').toBeLessThanOrEqual(
    viewport.width,
  );
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

  // Nothing inside the hidden thread screen may still paint: the swap
  // works by inherited `visibility: hidden`, so one inline
  // `visibility: visible` anywhere in the subtree punches through and
  // draws over the list — exactly how the timeline painted over the
  // thread rows on a real phone (2026-09-04).
  const leaks = await page.evaluate(() => {
    const pane = document.querySelector('.compact-screen-thread');
    if (!pane) return ['missing .compact-screen-thread'];
    const out: string[] = [];
    for (const el of pane.querySelectorAll<HTMLElement>('*')) {
      if (el.checkVisibility({ visibilityProperty: true })) {
        out.push(`${el.tagName}.${String(el.className).slice(0, 60)}`);
        if (out.length >= 5) break;
      }
    }
    return out;
  });
  expect(leaks).toEqual([]);

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

test('both New Thread actions reveal the reused composer from the list', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).tap();
  await expect(page.getByTestId('chat-header-title')).toHaveText('First task');

  for (const action of ['plus', 'menu'] as const) {
    await page.getByTestId('compact-back').tap();
    await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
    if (action === 'plus') {
      await page.getByTestId('project-item-new-thread').tap();
    } else {
      await page.getByTestId('project-item-menu').tap();
      await page.getByRole('menuitem', { name: 'New Thread', exact: true }).tap();
    }
    await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
    await expect(page.getByLabel('Message Input')).toBeVisible();
    await expect(page.getByLabel('Message Input')).toHaveValue('');
    await expect(page.getByTestId('chat-header-title')).toHaveText('New Thread');
    await expect(page.locator('section[data-pane-id]')).toHaveCount(1);
  }
});

test('the project header carries its menu, with New Terminal inside', async ({ harness, page }) => {
  await harness.open(page);
  await page.getByTestId('project-item-menu').first().tap();
  const sheet = page.locator('[data-popover-sheet]');
  await expect(sheet.getByRole('menuitem', { name: 'New Terminal' })).toBeVisible();
  await expect(sheet.getByRole('menuitem', { name: 'Rename Project' })).toBeVisible();
});

// The desktop header's row of icon buttons does not fit beside a title on
// a phone, and the first real-phone session (2026-09-04) found it there
// with the title clipped, an open-in-editor button the device cannot
// act on, and a command-palette button standing in for chords the phone
// does not have. Compact rolls the actions into one sheet.
// The header's one button opens a menu that drops from the button, not a
// bottom sheet (owner ruling, 2026-09-04): a control at the top of the
// screen answers where the finger is.
test('the chat header rolls its actions into one dropdown at the button', async ({
  harness,
  page,
}) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  await expect(page.getByTestId('palette-open')).toHaveCount(0);
  await expect(page.getByTestId('terminal-toggle')).toHaveCount(0);
  await expect(page.getByTestId('chat-header-open-editor')).toHaveCount(0);
  // The title gets the width the cluster used to take.
  const title = page.getByTestId('chat-header-title');
  const clipped = await title.evaluate((el) => el.scrollWidth > el.clientWidth + 1);
  expect(clipped, 'the title must not be clipped beside one button').toBe(false);

  const button = page.getByTestId('chat-header-more');
  await button.tap();
  const menu = page.locator('[data-popover]:not([data-popover-sheet])');
  await expect(menu.getByRole('menu', { name: 'Thread actions' })).toBeVisible();
  await expect(menu).toHaveAttribute('data-placement', /^bottom/);
  const buttonBox = await button.boundingBox();
  const menuBox = await menu.boundingBox();
  expect(buttonBox && menuBox).toBeTruthy();
  // Hangs from the button's bottom edge, inside the viewport.
  expect(menuBox!.y).toBeGreaterThanOrEqual(buttonBox!.y + buttonBox!.height - 1);
  expect(menuBox!.y).toBeLessThan(buttonBox!.y + buttonBox!.height + 16);
  expect(menuBox!.x + menuBox!.width).toBeLessThanOrEqual(page.viewportSize()!.width + 1);
  await menu.getByRole('menuitem', { name: /Review changes/ }).tap();
  await expect(menu).toHaveCount(0);
  await expect(page.locator('section[data-pane-kind="review"]')).toBeVisible();
});

// Below the width where even icon-only pickers plus the meters fit, every
// picker but the model folds into one roll-up; the model and the meters
// stay (owner ruling, 2026-09-04: which model answers and the usage and
// context readings are what a phone user reads before sending). Each
// roll-up row opens the picker the chord would.
test('the composer\'s densest rung keeps the model and the meters and rolls the rest up', async ({
  harness,
  page,
}) => {
  await page.setViewportSize({ width: 360, height: 720 });
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'First task' }).click();
  const toolbar = page.getByTestId('composer-toolbar');
  await expect(toolbar).toHaveAttribute('data-density', 'minimal');
  await expect(page.getByTestId('composer-model-menu-trigger')).toBeVisible();
  await expect(page.getByTestId('composer-effort-trigger')).toBeHidden();
  await expect(toolbar.locator('[data-composer-toolbar-meter]').first()).toBeVisible();
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect
    .poll(() => toolbar.evaluate((el) => el.scrollWidth - el.clientWidth))
    .toBeLessThanOrEqual(1);
  // Nothing overlaps: every visible toolbar control ends before the next
  // one starts (the shrunk picker box once let the pickers paint over
  // the meters while the ladder still read the row as fitting).
  const overlaps = await toolbar.evaluate((el) => {
    // One box per control: a meter is its wrapper (its inner button is
    // the same box), every other control is its button.
    const controls = [...el.querySelectorAll<HTMLElement>('button, [data-composer-toolbar-meter]')]
      .filter((c) => c.offsetParent !== null && c.getClientRects().length > 0)
      .filter((c) => !c.parentElement?.closest('button, [data-composer-toolbar-meter]'))
      .map((c) => ({
        id: c.dataset.testid ?? c.getAttribute('aria-label') ?? c.tagName,
        rect: c.getBoundingClientRect(),
      }))
      .sort((a, b) => a.rect.left - b.rect.left);
    const bad: string[] = [];
    for (let i = 1; i < controls.length; i++) {
      if (controls[i].rect.left < controls[i - 1].rect.right - 1) {
        bad.push(`${controls[i - 1].id} over ${controls[i].id}`);
      }
    }
    return bad;
  });
  expect(overlaps, 'toolbar controls must not overlap').toEqual([]);

  await page.getByTestId('composer-pickers-rollup').tap();
  const sheet = page.locator('[data-popover-sheet]');
  await expect(sheet.getByRole('menuitem', { name: 'Model…' })).toHaveCount(0);
  await sheet.getByRole('menuitem', { name: 'Effort…' }).tap();
  // The row opened the picker itself: its (hidden) trigger reports open
  // and a sheet with the picker's menu is up.
  await expect(page.getByTestId('composer-effort-trigger')).toHaveAttribute('aria-expanded', 'true');
  await expect(page.locator('[data-popover-sheet]').getByRole('menu')).toBeVisible();
});

// The soft keyboard shrinks the layout viewport (index.html asks for that
// with `interactive-widget=resizes-content`). The browser keeps scrollTop
// where it was, so without a re-pin the last message slides under the
// composer — what the first real-phone session saw when the composer
// took focus. A viewport resize is the same geometry change, and it is
// what a browser test can produce.
test('a viewport that shrinks under a pinned reader keeps the tail on screen', async ({
  harness,
  page,
}) => {
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Long task' }).click();
  const scroller = page.locator('.pane-scroll-surface').first();
  const gap = () => scroller.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop);
  await expect.poll(gap, { message: 'the opened thread rests at its bottom' }).toBeLessThanOrEqual(1);
  const before = await scroller.evaluate((el) => el.clientHeight);

  await page.setViewportSize({ width: 412, height: 500 });
  await expect.poll(() => scroller.evaluate((el) => el.clientHeight)).toBeLessThan(before);
  await expect
    .poll(gap, { message: 'the tail must follow the viewport\'s new bottom edge' })
    .toBeLessThanOrEqual(1);
});

// The desktop Settings spread (rail beside panel) crammed both columns
// into a phone's width and clipped the panel's controls off the right
// edge (found in the 2026-09-04 screen sweep). The spec rules Settings a
// stacked screen on compact (docs/specs/remote-access.md § The phone
// client): the rail is its own full-width screen, a section drills into
// its page, and the page header's back affordance returns to the rail.
test('Settings is stacked screens on compact, with every control in reach', async ({
  harness,
  page,
}) => {
  await harness.open(page);
  await page.getByText('Settings', { exact: true }).click();

  // The rail is the whole screen, and the page panel is not beside it.
  const rail = page.getByRole('tab', { name: 'Theme' });
  await expect(rail).toBeVisible();
  const viewport = page.viewportSize()!;
  const tabWidth = (await rail.boundingBox())!.width;
  expect(tabWidth, 'the rail must span the phone width').toBeGreaterThan(viewport.width * 0.85);
  await expect(page.getByTestId('settings-page-header')).toBeHidden();

  // Drilling in shows the page alone, back returns to the rail.
  await rail.click();
  await expect(page.getByTestId('settings-page-header')).toBeVisible();
  await expect(rail).toBeHidden();

  // No interactive control on the page may stick out of the viewport —
  // selects included, which is what the two-pane squeeze clipped.
  const clipped = await page.evaluate(() => {
    const panel = document.querySelector('[id^="settings-panel-"]');
    if (!panel) return ['missing settings panel'];
    // The page must not scroll sideways either: a long unbreakable token
    // (a data-dir path in a hint) once forced exactly that.
    if (panel.scrollWidth > panel.clientWidth + 1)
      return [`panel scrolls horizontally: ${panel.scrollWidth} > ${panel.clientWidth}`];
    const vw = innerWidth;
    const out: string[] = [];
    for (const el of panel.querySelectorAll<HTMLElement>('button, select, input, textarea, a, [role="button"]')) {
      if (!el.checkVisibility({ visibilityProperty: true })) continue;
      const b = el.getBoundingClientRect();
      if (b.width === 0 || b.height === 0) continue;
      if (b.left < -1 || b.right > vw + 1) {
        out.push(`${el.tagName}[${el.getAttribute('aria-label') ?? el.id ?? ''}] l=${Math.round(b.left)} r=${Math.round(b.right)} vw=${vw}`);
        if (out.length >= 6) break;
      }
    }
    return out;
  });
  expect(clipped).toEqual([]);

  await page.getByTestId('settings-page-back').click();
  await expect(rail).toBeVisible();
  await expect(page.getByTestId('settings-page-header')).toBeHidden();
});
