// AO tool rows under the compact layout, on both mock providers: every
// row ao-tool-rows.spec.ts reads on the desktop renders its sentence header
// inside the phone-width thread screen, opens by touch to its decoded
// result, and the refusal shows its message and code. Screenshots of every
// row collapsed and expanded go to the test's output directory. A row whose
// toggle sits at the edge of the "Jump to bottom" chip still opens.

import { test, expect } from './fixtures.js';
import type { Locator } from '@playwright/test';
import { SPAWN_TITLE, TARGET_TITLE, driveAoToolRows, expandRow, expectRowTextWhole, firstTextFits, openLead, placeRow, shootRow, toolRow } from './ao-tool-rows-helpers.js';

for (const provider of ['claude', 'codex'] as const) {
  test(`${provider}: AO tool rows read on the compact thread screen`, async ({ harness, page }) => {
    await driveAoToolRows(harness, provider, `compact-${provider}`);
    await openLead(harness, page);
    const viewport = page.viewportSize()!;
    const fits = async (row: Locator) => {
      const box = (await row.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
    };
    const shot = (name: string, row: Locator) => shootRow(row, test.info().outputPath(`compact-${provider}-${name}.png`));

    const rows: Array<[string, Locator]> = [
      ['spawn', toolRow(page, 'spawn', SPAWN_TITLE)],
      ['send', toolRow(page, 'send', `${TARGET_TITLE} · Rebase onto main`)],
      ['status', toolRow(page, 'status', '1 request')],
      ['search', toolRow(page, 'search', '"Target"')],
      ['show', toolRow(page, 'read', `${TARGET_TITLE} · last answer`)],
      ['loop', toolRow(page, 'loop', /^board · every 10m · next /)],
      ['error', toolRow(page, 'read', 'thread nosuch1 · last 20 turns')],
      ['run-start', toolRow(page, 'start', /^digest · "Weekly summary"/)],
      ['run-status', toolRow(page, 'status', /digest/)],
    ];
    for (const [name, row] of rows) {
      await expect(row, name).toHaveCount(1);
      await fits(row);
      await shot(`${name}-collapsed`, row);
    }

    await expect(rows[0][1].getByTestId('tool-call-card-ao-live-hint')).toHaveText('(answer arrived)', { timeout: 30_000 });
    // The header's own text keeps its room; the hint after it is what the
    // row's truncation cuts.
    expect(await firstTextFits(rows[0][1].getByTestId('tool-call-card-preview')), 'spawn header text clipped beside its hint').toBe(true);
    // What each row's result must say once open. Checked as each row opens:
    // the timeline windows rows that scroll far off screen.
    const opened: Record<string, (row: Locator) => Promise<void>> = {
      spawn: (row) => expect(row.getByTestId('tool-call-card-ao-link')).toHaveText(SPAWN_TITLE),
      send: (row) => expect(row.getByTestId('tool-call-card-ao-link')).toHaveText(TARGET_TITLE),
      status: (row) => expect(row.getByTestId('tool-call-card-ao-line-link')).toHaveText(TARGET_TITLE),
      search: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toContainText(/\d+ threads?|no threads/),
      show: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toContainText('last answer'),
      loop: (row) => expect(row.getByTestId('tool-call-card-ao-outcome')).toContainText('next'),
      error: (row) => expect(row.getByTestId('row-error-code').first()).toHaveText('thread_not_found'),
      'run-start': (row) => expect(row.getByTestId('tool-call-card-ao-link')).toHaveAttribute('data-link-kind', 'run'),
      'run-status': (row) => expect(row.getByTestId('tool-call-card-ao-link')).toHaveAttribute('data-link-kind', 'run'),
    };
    for (const [name, row] of rows) {
      await expandRow(row);
      await opened[name](row);
      await fits(row);
      await expectRowTextWhole(row);
      await shot(`${name}-expanded`, row);
    }
  });
}

// The case expandRow places rows for: Playwright presses the toggle's
// center truncated to 0.01 px, here less than a pixel above the chip.
test('codex: a row whose toggle sits a pixel above the jump chip opens', async ({ harness, page }) => {
  await driveAoToolRows(harness, 'codex', 'compact-chip-edge');
  await openLead(harness, page);
  // The rows above open first, so the timeline can scroll the loop row
  // down to the chip.
  for (const row of [
    toolRow(page, 'spawn', SPAWN_TITLE),
    toolRow(page, 'send', `${TARGET_TITLE} · Rebase onto main`),
    toolRow(page, 'status', '1 request'),
    toolRow(page, 'search', '"Target"'),
    toolRow(page, 'read', `${TARGET_TITLE} · last answer`),
  ]) {
    await expandRow(row);
  }
  const loop = toolRow(page, 'loop', /^board · every 10m · next /);
  await placeRow(loop);
  const chip = page.getByTestId('scroll-to-bottom');
  await expect(chip).toBeVisible();
  // Whole pixels by scrolling the timeline, then the activity run's clip
  // where the timeline stops; the fraction by offsetting the row in place.
  const offset = await loop.evaluate(async (card, chipTop) => {
    const toggle = card.querySelector('[data-testid="tool-call-card-toggle"]')!;
    const timeline = document.querySelector<HTMLElement>('[data-testid="message-timeline-scroll"]')!;
    const clip = card.closest<HTMLElement>('[data-testid="activity-run-clip"]');
    const center = () => {
      const box = toggle.getBoundingClientRect();
      return box.top + box.height / 2;
    };
    const frames = () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    const want = chipTop - 1 + 1 / 64;
    for (let attempt = 0; attempt < 4 && !(want - center() >= 0 && want - center() < 1); attempt++) {
      const move = Math.floor(want - center());
      const before = timeline.scrollTop;
      timeline.scrollTop = before - move;
      const moved = before - timeline.scrollTop;
      if (clip && moved !== move) clip.scrollTop -= move - moved;
      await frames();
    }
    const fraction = want - center();
    card.style.position = 'relative';
    card.style.top = `${fraction}px`;
    await frames();
    return fraction;
  }, (await chip.boundingBox())!.y);
  expect(offset, 'the scrolls left the toggle more than a pixel from its place').toBeGreaterThanOrEqual(0);
  expect(offset, 'the scrolls left the toggle more than a pixel from its place').toBeLessThan(1);
  const box = (await loop.getByTestId('tool-call-card-toggle').boundingBox())!;
  const chipBox = (await chip.boundingBox())!;
  const pressAt = { x: Math.trunc((box.x + box.width / 2) * 100) / 100, y: Math.trunc((box.y + box.height / 2) * 100) / 100 };
  const where = `press at ${pressAt.x},${pressAt.y}; chip at ${chipBox.x},${chipBox.y} ${chipBox.width}x${chipBox.height}`;
  expect(pressAt.y, where).toBeGreaterThan(chipBox.y - 1);
  expect(pressAt.y, where).toBeLessThan(chipBox.y);
  expect(pressAt.x, where).toBeGreaterThan(chipBox.x);
  expect(pressAt.x, where).toBeLessThan(chipBox.x + chipBox.width);
  const found = await page.evaluate(({ x, y }) => document.elementFromPoint(x, y)?.closest('[data-testid]')?.getAttribute('data-testid'), pressAt);
  expect(found, `what elementFromPoint finds at the ${where}`).toBe('tool-call-card-preview');

  await expandRow(loop);
  await expect(loop.getByTestId('tool-call-card-ao-outcome')).toContainText('next');
});
