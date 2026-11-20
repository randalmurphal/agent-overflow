// Promote and demote (docs/decisions.md, "Panes"): a thread pane's header
// control lays it over the whole pane strip and the chord puts it back; the
// section stays the same element, its neighbors and the strip do not move,
// and the timeline keeps the row it was reading at the top through both
// width changes. A tool pane promotes from a double-click on its title and
// restores from its header control. Moving focus to another pane while one
// is promoted demotes first and lands focus there.
import type { Locator, Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';

type Box = { x: number; y: number; width: number; height: number };

async function boxOf(locator: Locator): Promise<Box> {
  const box = await locator.boundingBox();
  if (!box) throw new Error('expected a laid-out element');
  return box;
}

function expectBoxClose(actual: Box, expected: Box): void {
  for (const key of ['x', 'y', 'width', 'height'] as const) expect(Math.abs(actual[key] - expected[key])).toBeLessThan(1);
}

// The pane host's padding box: where a promoted pane sits.
async function stripArea(page: Page): Promise<Box> {
  return await page.getByTestId('pane-host').evaluate((el) => {
    const r = el.getBoundingClientRect();
    return { x: r.left + el.clientLeft, y: r.top + el.clientTop, width: el.clientWidth, height: el.clientHeight };
  });
}

async function stripScrollLeft(page: Page): Promise<number> {
  return await page.getByTestId('pane-host').evaluate((el) => el.scrollLeft);
}

// Settled: promoted (or not) with no clip left animating.
async function expectPromoted(section: Locator, promoted: boolean): Promise<void> {
  await expect(section).toHaveAttribute('data-pane-promoted', String(promoted));
  await expect.poll(() => section.evaluate((el) => (el as HTMLElement).style.clipPath)).toBe('');
}

// The row at the top of a pane's timeline and how far below the top it sits.
// The first row in view and its offset from the top, once the timeline is
// still: the same row at the same offset over five frames in a row, so a
// wheel's scroll and the window's remeasure have landed.
async function readingAnchor(pane: Locator): Promise<{ id: string; offset: number }> {
  return await pane.getByTestId('message-timeline-scroll').evaluate((scroller) => new Promise<{ id: string; offset: number }>((resolve, reject) => {
    const read = () => {
      const top = scroller.getBoundingClientRect().top;
      for (const row of scroller.querySelectorAll<HTMLElement>('[data-item-id]')) {
        const rect = row.getBoundingClientRect();
        if (rect.bottom > top + 1) return { id: row.dataset.itemId ?? '', offset: rect.top - top };
      }
      return { id: '', offset: 0 };
    };
    let last = read();
    let still = 0;
    let frames = 0;
    const step = () => {
      const next = read();
      still = next.id === last.id && next.offset === last.offset ? still + 1 : 0;
      last = next;
      if (still >= 5) return resolve(last);
      if (++frames > 600) return reject(new Error('the timeline did not come to rest'));
      requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }));
}

function turns(count: number): { userText: string; items: { kind: string; summary: string }[] }[] {
  return Array.from({ length: count }, (_, turn) => ({
    userText: `Question ${turn}`,
    items: [{ kind: 'assistant_text', summary: `Answer ${turn}. ${'A line of prose that wraps differently at every width. '.repeat(6)}` }],
  }));
}

test.beforeEach(async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 900 });
});

test('a thread pane promotes over the strip and demotes to its slot, keeping its timeline position', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'pane-promote', repo: {}, threads: [
      { title: 'Long history', provider: 'claude', turns: turns(40) },
      { title: 'Other thread', provider: 'claude', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Other answer.' }] }] },
    ] }],
  });
  const [longId, otherId] = seed.projects[0].threadIds;
  await harness.open(page);
  await page.locator(`[data-sidebar-thread-id="${longId}"]`).click();
  await page.locator(`[data-sidebar-thread-id="${otherId}"]`).click({ modifiers: ['ControlOrMeta'] });
  const panes = page.locator('section[data-pane-kind="thread"]');
  await expect(panes).toHaveCount(2);
  const longPane = panes.filter({ has: page.locator(`[data-thread-id="${longId}"]`) });
  const otherPane = panes.filter({ has: page.locator(`[data-thread-id="${otherId}"]`) });
  await expect(longPane.getByText('Answer 39.', { exact: false })).toBeVisible();

  // Read from the middle of the history.
  const scroller = longPane.getByTestId('message-timeline-scroll');
  await scroller.hover();
  await page.mouse.wheel(0, -2400);
  await expect.poll(() => scroller.evaluate((el) => el.scrollHeight - el.clientHeight - el.scrollTop)).toBeGreaterThan(1500);
  const anchor = await readingAnchor(longPane);
  expect(anchor.id).not.toBe('');

  const slot = await boxOf(longPane);
  const otherBox = await boxOf(otherPane);
  const scrollLeft = await stripScrollLeft(page);
  const node = await longPane.elementHandle();

  await longPane.getByTestId('chat-header').getByTestId('pane-promote').click();
  await expectPromoted(longPane, true);
  expectBoxClose(await boxOf(longPane), await stripArea(page));
  expectBoxClose(await boxOf(page.getByTestId('pane-promoted-slot')), slot);
  expectBoxClose(await boxOf(otherPane), otherBox);
  expect(await stripScrollLeft(page)).toBe(scrollLeft);
  await expect(longPane.getByTestId('chat-header').getByTestId('pane-promote')).toHaveAttribute('aria-label', 'Restore Pane');
  expect((await readingAnchor(longPane)).id).toBe(anchor.id);

  // The chord restores it, from the composer.
  await longPane.getByLabel('Message Input').click();
  await page.keyboard.press('Alt+Shift+Enter');
  await expectPromoted(longPane, false);
  await expect(page.getByTestId('pane-promoted-slot')).toHaveCount(0);
  expectBoxClose(await boxOf(longPane), slot);
  expectBoxClose(await boxOf(otherPane), otherBox);
  expect(await stripScrollLeft(page)).toBe(scrollLeft);
  expect(await longPane.evaluate((el, before) => el === before, node)).toBe(true);
  const after = await readingAnchor(longPane);
  expect(after.id).toBe(anchor.id);
  expect(Math.abs(after.offset - anchor.offset)).toBeLessThan(2);

  // Promoted again, moving focus to the other pane demotes and focuses it.
  await page.keyboard.press('Alt+Shift+Enter');
  await expectPromoted(longPane, true);
  await page.keyboard.press('Alt+l');
  await expectPromoted(longPane, false);
  await expect(otherPane).toHaveAttribute('data-pane-focused', 'true');
});

test('a tool pane promotes from a double-click on its title and restores from its header', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'pane-promote-tool', repo: {}, threads: [
      { title: 'Alpha task', provider: 'claude', turns: [{ userText: 'alpha', items: [{ kind: 'assistant_text', summary: 'Alpha answer.' }] }] },
    ] }],
  });
  await harness.open(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Alpha task' }).click();
  await expect(page.getByText('Alpha answer.')).toBeVisible();
  await page.getByTestId('sidebar-tool-workflows').click({ modifiers: ['ControlOrMeta'] });
  const tool = page.locator('section[data-pane-kind="workflows"]');
  const thread = page.locator('section[data-pane-kind="thread"]');
  await expect(tool.getByTestId('workflows-pane')).toBeVisible();
  const slot = await boxOf(tool);
  const threadBox = await boxOf(thread);

  await tool.getByTestId('tool-pane-title').dblclick();
  await expectPromoted(tool, true);
  expectBoxClose(await boxOf(tool), await stripArea(page));
  expectBoxClose(await boxOf(thread), threadBox);
  // The promoted pane's title is no drag handle.
  await expect(tool.getByTestId('tool-pane-title')).toHaveAttribute('draggable', 'false');

  await tool.getByTestId('tool-pane-header').getByTestId('pane-promote').click();
  await expectPromoted(tool, false);
  expectBoxClose(await boxOf(tool), slot);
  await expect(tool.getByTestId('tool-pane-title')).toHaveAttribute('draggable', 'true');
});
