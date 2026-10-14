// AO tool rows under the compact layout, on both mock providers: every
// row ao-tool-rows.spec.ts reads on the desktop renders its sentence header
// inside the phone-width thread screen, opens by touch to its decoded
// result, and the refusal shows its message and code. Screenshots of every
// row collapsed and expanded go to the test's output directory.

import { test, expect } from './fixtures.js';
import type { Locator } from '@playwright/test';
import { SPAWN_TITLE, TARGET_TITLE, driveAoToolRows, expandRow, openLead, shootRow, toolRow } from './ao-tool-rows-helpers.js';

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
    const textFits = await rows[0][1].getByTestId('tool-call-card-preview').evaluate((el) => {
      const text = document.createTreeWalker(el, NodeFilter.SHOW_TEXT).nextNode()!;
      const range = document.createRange();
      range.selectNodeContents(text);
      return range.getBoundingClientRect().right <= el.getBoundingClientRect().right + 0.5;
    });
    expect(textFits, 'spawn header text clipped beside its hint').toBe(true);
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
      await row.scrollIntoViewIfNeeded();
      await expandRow(row);
      await opened[name](row);
      await fits(row);
      await shot(`${name}-expanded`, row);
    }
  });
}
