// Dragging from the sidebar into the pane strip (docs/specs/pull-requests.md,
// "Tools row"), with real Chromium drag events, whose DataTransfer answers
// nothing during dragover: a thread row shows the drop preview while it
// hovers and opens as a new pane at the slot; a tools-row button shows the
// same preview and places its tool at the slot, between two panes or at the
// end, replacing nothing; an open tool dragged elsewhere moves there, with
// no preview over its own slot, and is focused; a press on a tool's icon,
// its label or its padding starts the drag alike (the control is a
// role="button" element, since Firefox starts no drag from a <button>).
import type { Locator, Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';

function sections(page: Page): Locator {
  return page.locator('[data-testid="pane-host"] > section[data-pane-id]');
}

async function stripKinds(page: Page): Promise<string[]> {
  return await sections(page).evaluateAll((els) => els.map((el) => (el as HTMLElement).dataset.paneKind ?? ''));
}

async function focusedKind(page: Page): Promise<string> {
  return await page.locator('section[data-pane-focused="true"]').getAttribute('data-pane-kind') ?? '';
}

function preview(page: Page): Locator {
  return page.getByTestId('pane-thread-drop-preview');
}

async function center(locator: Locator): Promise<{ x: number; y: number }> {
  const box = await locator.boundingBox();
  if (!box) throw new Error('expected a laid-out element');
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}

/** Press on `source` (an element's center, or a point), move to `fraction`
 * across `target`, run `whileOver` with the drag in flight, and drop. */
async function dragInto(
  page: Page,
  source: Locator | { x: number; y: number },
  target: Locator,
  fraction: number,
  whileOver: () => Promise<void>,
): Promise<void> {
  const from = 'x' in source ? source : await center(source);
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  const box = await target.boundingBox();
  if (!box) throw new Error('expected a laid-out drop target');
  await page.mouse.move(box.x + box.width * fraction, box.y + box.height / 2, { steps: 12 });
  await whileOver();
  await page.mouse.up();
}

async function seedThreads(harness: { rpc: <T>(method: string, ...args: unknown[]) => Promise<T> }): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'pane-strip-drop',
      repo: {},
      threads: ['Alpha', 'Beta', 'Gamma'].map((name) => ({
        title: `${name} task`,
        provider: 'claude',
        turns: [{ userText: name.toLowerCase(), items: [{ kind: 'assistant_text', summary: `${name} answer.` }] }],
      })),
    }],
  });
}

/** Alpha and Beta side by side, Beta focused. */
async function twoThreads(page: Page): Promise<void> {
  await page.getByTestId('thread-row').filter({ hasText: 'Alpha task' }).click();
  await expect(page.getByText('Alpha answer.')).toBeVisible();
  await page.getByTestId('thread-row').filter({ hasText: 'Beta task' }).click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByText('Beta answer.')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['thread', 'thread']);
}

test.beforeEach(async ({ page }) => {
  // Wide enough for four panes, so the strip never scrolls under the drag.
  await page.setViewportSize({ width: 2600, height: 900 });
});

test('a thread row previews its slot while it hovers the strip and opens there', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);

  const gamma = page.getByTestId('thread-row').filter({ hasText: 'Gamma task' });
  await dragInto(page, gamma, sections(page).nth(1), 0.25, async () => {
    await expect(preview(page)).toBeVisible();
    await expect(preview(page)).toHaveAttribute('data-insert-index', '1');
  });
  await expect(page.getByText('Gamma answer.')).toBeVisible();
  await expect(preview(page)).toHaveCount(0);
  expect(await stripKinds(page)).toEqual(['thread', 'thread', 'thread']);
  await expect(sections(page).nth(1)).toContainText('Gamma task');
});

test('a tool dropped between two panes or at the end takes that slot, replacing nothing', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);

  await dragInto(page, page.getByTestId('sidebar-tool-pulls'), sections(page).nth(1), 0.25, async () => {
    await expect(preview(page)).toBeVisible();
    await expect(preview(page)).toHaveAttribute('data-insert-index', '1');
  });
  await expect(page.getByTestId('pulls-pane')).toBeVisible();
  await expect(preview(page)).toHaveCount(0);
  expect(await stripKinds(page)).toEqual(['thread', 'pulls', 'thread']);
  expect(await focusedKind(page)).toBe('pulls');
  await expect(page.getByTestId('sidebar-tool-label')).toHaveText('Pull requests');

  await dragInto(page, page.getByTestId('sidebar-tool-workflows'), sections(page).nth(2), 0.9, async () => {
    // The append slot, past the last pane.
    await expect(preview(page)).toHaveAttribute('data-insert-index', '3');
  });
  await expect(page.getByTestId('workflows-pane')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['thread', 'pulls', 'thread', 'workflows']);
  expect(await focusedKind(page)).toBe('workflows');
  // Both threads are still open: the drops replaced nothing.
  await expect(page.getByText('Alpha answer.')).toBeVisible();
  await expect(page.getByText('Beta answer.')).toBeVisible();
});

test('an open tool dragged elsewhere moves there, with no preview over its own slot', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  await page.getByTestId('sidebar-tool-pulls').click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByTestId('pulls-pane')).toBeVisible();
  expect(await stripKinds(page)).toEqual(['thread', 'thread', 'pulls']);
  await sections(page).nth(0).getByTestId('chat-header-title').click();
  expect(await focusedKind(page)).toBe('thread');

  await dragInto(page, page.getByTestId('sidebar-tool-pulls'), sections(page).nth(1), 0.75, async () => {
    // Right of Beta is the tool's own slot: dropping there changes nothing.
    await expect(preview(page)).toHaveCount(0);
    const first = await sections(page).nth(0).boundingBox();
    await page.mouse.move(first!.x + first!.width * 0.25, first!.y + first!.height / 2, { steps: 8 });
    await expect(preview(page)).toHaveAttribute('data-insert-index', '0');
    // The preview is the moved pane's own width, not a new pane's.
    const pullsWidth = (await page.locator('section[data-pane-kind="pulls"]').boundingBox())!.width;
    expect(Math.abs((await preview(page).boundingBox())!.width - pullsWidth)).toBeLessThanOrEqual(2);
  });
  await expect.poll(() => stripKinds(page)).toEqual(['pulls', 'thread', 'thread']);
  expect(await focusedKind(page)).toBe('pulls');
  await expect(page.getByTestId('pulls-pane')).toHaveCount(1);
});

test('a press on a tool\'s icon, label or padding starts the drag', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  const tool = page.getByTestId('sidebar-tool-pulls');
  // Pull requests is the labelled tool: nothing was used yet, then it is.
  await expect(tool.getByTestId('sidebar-tool-label')).toBeVisible();
  const box = (await tool.boundingBox())!;
  // The padding point lies left of the icon, on the control itself.
  expect((await tool.locator(':scope > :first-child').boundingBox())!.x).toBeGreaterThan(box.x + 4);
  const parts: Array<[string, { x: number; y: number }, Locator, number, string[]]> = [
    ['icon', await center(tool.locator(':scope > :first-child')), sections(page).nth(1), 0.25, ['thread', 'pulls', 'thread']],
    ['label', await center(tool.getByTestId('sidebar-tool-label')), sections(page).nth(0), 0.25, ['pulls', 'thread', 'thread']],
    ['padding', { x: box.x + 4, y: box.y + box.height / 2 }, sections(page).nth(2), 0.9, ['thread', 'thread', 'pulls']],
  ];
  for (const [part, at, target, fraction, kinds] of parts) {
    await dragInto(page, at, target, fraction, async () => {
      await expect(preview(page), `a drag from the ${part}`).toBeVisible();
    });
    await expect.poll(() => stripKinds(page), `a drop from the ${part}`).toEqual(kinds);
  }
});
