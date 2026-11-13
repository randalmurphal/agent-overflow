// Dragging from the sidebar into the pane strip (docs/specs/pull-requests.md,
// "Tools row"), with real Chromium drag events, whose DataTransfer answers
// nothing during dragover: a thread row shows the drop preview while it
// hovers and opens as a new pane at the slot; the preview is a bar on the
// seam the pane lands in plus the lit half of the pane under the pointer,
// so the two halves of a pane read differently among panes of one width,
// and the first and last seams stay whole inside the strip; a tools-row
// button shows the same preview and places its tool at the slot, between
// two panes or at the end, replacing nothing; an open tool dragged
// elsewhere moves there, with no preview over its own slot, and is focused;
// a pane dragged by its title shows the same preview, none over the slot
// it holds, and moves to a pane's far half or a divider; a press on a
// tool's icon, its label or its padding starts the drag alike (the control
// is a role="button" element, since Firefox starts no drag from a
// <button>); a drag that leaves the strip over a pane's content, or is
// cancelled with Escape, takes its preview along and places nothing; a
// pane, a thread row and a tool held over a pane are over the strip's
// shield, not the pane, so the browser scrolls no timeline under them, and
// the panes take the pointer again when the drag ends; none of them, nor a
// project row, is text a text box takes; text dragged out of one composer
// lands in another pane's composer.
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
  return page.getByTestId('pane-drop-seam');
}

function litHalf(page: Page): Locator {
  return page.getByTestId('pane-drop-half');
}

async function box(locator: Locator): Promise<{ left: number; right: number; width: number }> {
  const rect = await locator.boundingBox();
  if (!rect) throw new Error('expected a laid-out element');
  return { left: rect.x, right: rect.x + rect.width, width: rect.width };
}

/** With a drag in flight, hold the pointer at `fraction` across `target`
 * until the preview names `insertIndex`. Chromium delivers no dragover for
 * some synthetic moves, the last of a run included, so the pointer steps a
 * pixel either way until one arrives. */
async function hoverUntilSlot(page: Page, target: Locator, fraction: number, insertIndex: number): Promise<void> {
  const rect = await target.boundingBox();
  if (!rect) throw new Error('expected a laid-out drop target');
  const x = rect.x + rect.width * fraction;
  const y = rect.y + rect.height / 2;
  await page.mouse.move(x, y, { steps: 6 });
  let nudge = 1;
  await expect(async () => {
    nudge = -nudge;
    await page.mouse.move(x + nudge, y);
    await expect(preview(page)).toHaveAttribute('data-insert-index', String(insertIndex), { timeout: 250 });
  }).toPass();
}

async function stripTitles(page: Page): Promise<string[]> {
  return await sections(page).evaluateAll((els) => els.map((el) => el.querySelector('[data-testid="chat-header-title"]')?.textContent?.trim() ?? ''));
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

/** Three threads, each ending in "<Name> answer."; `earlierTurns` before it
 * make a timeline far taller than its pane. */
async function seedThreads(harness: { rpc: <T>(method: string, ...args: unknown[]) => Promise<T> }, earlierTurns = 0): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'pane-strip-drop',
      repo: {},
      threads: ['Alpha', 'Beta', 'Gamma'].map((name) => ({
        title: `${name} task`,
        provider: 'claude',
        turns: [
          ...Array.from({ length: earlierTurns }, (_, turn) => ({
            userText: `${name.toLowerCase()} ${turn}`,
            items: [{ kind: 'assistant_text', summary: `${name} earlier ${turn}. `.repeat(12) }],
          })),
          { userText: name.toLowerCase(), items: [{ kind: 'assistant_text', summary: `${name} answer.` }] },
        ],
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

test('the preview is a bar on the seam and the lit half of the pane, different for each half', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  await page.getByTestId('sidebar-tool-pulls').click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByTestId('pulls-pane')).toBeVisible();
  const host = await box(page.getByTestId('pane-host'));
  const [first, middle, last] = [await box(sections(page).nth(0)), await box(sections(page).nth(1)), await box(sections(page).nth(2))];
  // Three panes of one width: the case an outline as wide as a pane cannot tell apart.
  expect(Math.abs(first.width - middle.width)).toBeLessThanOrEqual(2);
  expect(Math.abs(last.width - middle.width)).toBeLessThanOrEqual(2);

  const seamCenter = async (): Promise<number> => {
    const seam = await box(preview(page));
    expect(seam.width).toBe(4);
    return (seam.left + seam.right) / 2;
  };
  const gamma = page.getByTestId('thread-row').filter({ hasText: 'Gamma task' });
  await dragInto(page, gamma, sections(page).nth(1), 0.25, async () => {
    // Left half of the middle pane: the seam between the first two.
    await expect(preview(page)).toHaveAttribute('data-insert-index', '1');
    expect(Math.abs(await seamCenter() - middle.left)).toBeLessThanOrEqual(1);
    await expect(litHalf(page)).toHaveAttribute('data-seam-side', 'left');
    let lit = await box(litHalf(page));
    expect(Math.abs(lit.left - middle.left)).toBeLessThanOrEqual(1);
    expect(Math.abs(lit.width - middle.width / 2)).toBeLessThanOrEqual(1);

    // Right half of the same pane: the seam on its other side.
    await hoverUntilSlot(page, sections(page).nth(1), 0.75, 2);
    expect(Math.abs(await seamCenter() - middle.right)).toBeLessThanOrEqual(1);
    await expect(litHalf(page)).toHaveAttribute('data-seam-side', 'right');
    lit = await box(litHalf(page));
    expect(Math.abs(lit.right - middle.right)).toBeLessThanOrEqual(1);
    expect(Math.abs(lit.width - middle.width / 2)).toBeLessThanOrEqual(1);

    // The strip's two outer seams keep their whole bar inside the strip.
    await hoverUntilSlot(page, sections(page).nth(0), 0.25, 0);
    expect((await box(preview(page))).left).toBeGreaterThanOrEqual(host.left);
    expect((await box(preview(page))).left).toBeLessThanOrEqual(host.left + 1);
    await hoverUntilSlot(page, sections(page).nth(2), 0.75, 3);
    expect((await box(preview(page))).right).toBeLessThanOrEqual(host.right);
    expect((await box(preview(page))).right).toBeGreaterThanOrEqual(last.right - 4);
    // The bar adds nothing to scroll to.
    expect(await page.getByTestId('pane-host').evaluate((el) => el.scrollWidth - el.clientWidth)).toBe(0);

    await hoverUntilSlot(page, sections(page).nth(1), 0.75, 2);
  });
  await expect(page.getByText('Gamma answer.')).toBeVisible();
  await expect(preview(page)).toHaveCount(0);
  await expect(litHalf(page)).toHaveCount(0);
  expect(await stripKinds(page)).toEqual(['thread', 'thread', 'thread', 'pulls']);
  await expect(sections(page).nth(2)).toContainText('Gamma task');
});

test('a pane dragged by its title previews its slot and moves there, also onto a divider', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  await page.getByTestId('thread-row').filter({ hasText: 'Gamma task' }).click({ modifiers: ['ControlOrMeta'] });
  await expect(page.getByText('Gamma answer.')).toBeVisible();
  expect(await stripTitles(page)).toEqual(['Alpha task', 'Beta task', 'Gamma task']);
  const title = (name: string): Locator => page.locator('section[data-pane-id]').filter({ hasText: `${name} answer.` }).getByTestId('chat-header-title');

  await dragInto(page, title('Alpha'), sections(page).nth(1), 0.25, async () => {
    // Beta's near half is the slot Alpha already holds.
    await expect(preview(page)).toHaveCount(0);
    await hoverUntilSlot(page, sections(page).nth(1), 0.75, 2);
    await expect(litHalf(page)).toHaveAttribute('data-seam-side', 'right');
  });
  await expect.poll(() => stripTitles(page)).toEqual(['Beta task', 'Alpha task', 'Gamma task']);
  await expect(preview(page)).toHaveCount(0);

  // Onto the divider between the first two panes.
  await dragInto(page, title('Gamma'), sections(page).nth(0), 0.25, async () => {
    await hoverUntilSlot(page, page.locator('[data-pane-gap-index="1"]'), 0.5, 1);
  });
  await expect.poll(() => stripTitles(page)).toEqual(['Beta task', 'Gamma task', 'Alpha task']);
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

test('a drag that leaves the strip takes its preview along and places nothing', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  // Over a pane's content, then back over the sidebar, and released there.
  await dragInto(page, page.getByTestId('sidebar-tool-workflows'), sections(page).nth(1), 0.25, async () => {
    await expect(preview(page)).toBeVisible();
    const back = await center(page.getByTestId('sidebar'));
    await page.mouse.move(back.x, back.y, { steps: 8 });
    await expect(preview(page)).toHaveCount(0);
  });
  expect(await stripKinds(page)).toEqual(['thread', 'thread']);
  await expect(page.getByTestId('workflows-pane')).toHaveCount(0);
});

test('a drag cancelled with Escape takes its preview along and places nothing', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  const tool = page.getByTestId('sidebar-tool-workflows');
  // Over a pane's content, then Escape.
  const from = await center(tool);
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  const box = (await sections(page).nth(1).boundingBox())!;
  await page.mouse.move(box.x + box.width * 0.25, box.y + box.height / 2, { steps: 12 });
  await expect(preview(page)).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(preview(page)).toHaveCount(0);
  await page.mouse.up();
  expect(await stripKinds(page)).toEqual(['thread', 'thread']);
  await expect(page.getByTestId('workflows-pane')).toHaveCount(0);
});

test('a pane, a thread row and a tool held over a pane are over the shield, and scroll nothing in it', async ({ harness, page }) => {
  await seedThreads(harness, 40);
  await harness.open(page);
  await twoThreads(page);
  const timeline = sections(page).nth(0).getByTestId('message-timeline-scroll');
  const shield = page.getByTestId('pane-drag-shield');
  // Alpha rests at its end, far from its top.
  await expect.poll(() => timeline.evaluate((el) => el.scrollTop)).toBeGreaterThan(1000);
  const sources: Array<[string, Locator]> = [
    ['a pane title', sections(page).nth(1).getByTestId('chat-header-title')],
    ['a thread row', page.getByTestId('thread-row').filter({ hasText: 'Gamma task' })],
    ['a tool', page.getByTestId('sidebar-tool-pulls')],
  ];
  for (const [what, source] of sources) {
    const top = await timeline.evaluate((el) => el.scrollTop);
    const at = (await timeline.boundingBox())!;
    // Just inside the timeline's top edge, where the browser scrolls a
    // scroller under a held drag.
    const x = at.x + at.width / 2;
    const y = at.y + 8;
    const from = await center(source);
    await page.mouse.move(from.x, from.y);
    await page.mouse.down();
    // The shield comes up on the drag's first dragover over the strip. A
    // real drag delivers one as it crosses in; Chromium delivers none for
    // some synthetic moves (hoverUntilSlot), and a drag resting at the edge
    // with no shield yet is scrolled. So bring the shield up mid-pane, away
    // from every edge, and go to the edge over it.
    const midY = at.y + at.height / 2;
    await page.mouse.move(x, midY, { steps: 12 });
    let nudge = 1;
    await expect(async () => {
      nudge = -nudge;
      await page.mouse.move(x + nudge, midY);
      await expect(shield, what).toBeVisible({ timeout: 250 });
    }).toPass();
    await shield.evaluate((el) => { el.dataset.held = 'yes'; });
    await page.mouse.move(x, y, { steps: 6 });
    // An uncovered timeline scrolls within the first half second of such a
    // hold; nothing moving is only shown by waiting it out.
    for (let step = 0; step < 30; step += 1) {
      await page.mouse.move(x + (step % 2), y);
      await page.waitForTimeout(50);
    }
    expect(await timeline.evaluate((el) => el.scrollTop), what).toBe(top);
    // One shield for the whole hold, and it is what the pointer is over.
    await expect(shield, what).toHaveAttribute('data-held', 'yes');
    expect(await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.getAttribute('data-testid'), [x, y]), what).toBe('pane-drag-shield');
    await page.keyboard.press('Escape');
    await expect(shield, what).toHaveCount(0);
    await page.mouse.up();
  }
  expect(await stripKinds(page)).toEqual(['thread', 'thread']);
  // The panes take the pointer again.
  const composer = sections(page).nth(0).getByLabel('Message Input');
  await composer.click();
  await expect(composer).toBeFocused();
});

test('a thread row, a pane title and a project row are not text: a text box takes none of them', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  await page.getByRole('button', { name: /^Sort Projects/ }).click();
  await page.getByRole('menuitemradio', { name: 'Manual' }).or(page.getByRole('menuitem', { name: 'Manual' })).click();
  const search = page.getByTestId('sidebar-thread-search');
  const sources: Array<[string, Locator]> = [
    ['a thread row', page.getByTestId('thread-row').filter({ hasText: 'Gamma task' })],
    ['a pane title', sections(page).nth(1).getByTestId('chat-header-title')],
    ['a project row', page.getByTestId('project-item-label')],
  ];
  for (const [what, source] of sources) {
    const dragStarted = page.evaluate(() => new Promise<string[]>((resolve) => {
      document.addEventListener('dragstart', (event) => resolve([...(event.dataTransfer?.types ?? [])]), { once: true });
    }));
    await dragInto(page, source, search, 0.5, async () => {});
    // The drag ran, and carried no plain text.
    expect(await dragStarted, what).not.toContain('text/plain');
    await expect(search, what).toHaveValue('');
  }
  expect(await stripKinds(page)).toEqual(['thread', 'thread']);
});

test('text dragged out of one composer lands in another pane\'s composer', async ({ harness, page }) => {
  await seedThreads(harness);
  await harness.open(page);
  await twoThreads(page);
  const from = sections(page).nth(0).getByLabel('Message Input');
  const to = sections(page).nth(1).getByLabel('Message Input');
  await from.fill('carry this over');
  await from.press('ControlOrMeta+a');
  const text = (await from.boundingBox())!;
  // A press on the selected words, near the start of the line.
  await dragInto(page, { x: text.x + 30, y: text.y + text.height / 2 }, to, 0.5, async () => {
    // Dragged text places no pane: no shield, no preview.
    await expect(page.getByTestId('pane-drag-shield')).toHaveCount(0);
    await expect(preview(page)).toHaveCount(0);
  });
  await expect(to).toHaveValue('carry this over');
  await expect(from).toHaveValue('');
  // The composer holds the dropped text as its draft: it sends.
  await sections(page).nth(1).getByTestId('composer-send').click();
  await expect(sections(page).nth(1).getByTestId('message-timeline-scroll')).toContainText('carry this over');
  await expect(to).toHaveValue('');
});
