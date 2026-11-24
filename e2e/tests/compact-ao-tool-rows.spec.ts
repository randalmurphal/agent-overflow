// AO tool rows under the compact layout, on the Claude mock (the layout is
// the same on both; ao-tool-rows.spec.ts reads the rows on both): every
// row ao-tool-rows.spec.ts reads on the desktop renders its sentence header
// inside the phone-width thread screen, opens by touch to its decoded
// result, and the refusal shows its message and code. expandRow opens a
// row left with its toggle's center less than a pixel above the composer,
// where a tap reaches the composer. With AO_E2E_SCREENSHOTS=1,
// screenshots of every row collapsed and expanded go to the test's output
// directory.

import { test, expect } from './fixtures.js';
import type { Locator } from '@playwright/test';
import { SPAWN_TITLE, TARGET_TITLE, atOverlayEdge, driveAoToolRows, expandRow, expectRowTextWhole, firstTextFits, openLead, shootRow, toolRow } from './ao-tool-rows-helpers.js';

test(`claude: AO tool rows read on the compact thread screen`, async ({ harness, page }) => {
  const provider = 'claude';
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
    // The last row, with every row above it open, as the turn leaves it
    // when the reader has opened the rest: the timeline has the room to
    // bring it down to the composer.
    if (name === 'run-status') await atOverlayEdge(row, page.getByTestId('composer-root'), () => expandRow(row));
    else await expandRow(row);
    await opened[name](row);
    await fits(row);
    await expectRowTextWhole(row);
    await shot(`${name}-expanded`, row);
  }
});
