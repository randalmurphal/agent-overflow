// The tracker tools' timeline rows under the compact layout, on the Claude
// mock (tracker-agent-tools.spec.ts drives both providers on the desktop):
// each row fits the phone-width thread screen, opens by touch to its
// decoded result, and the refusal shows its code.

import type { Locator } from '@playwright/test';

import { test, expect } from './fixtures.js';
import { expandRow, expectRowTextWhole, toolRow } from './ao-tool-rows-helpers.js';
import { TICKET_ROWS, driveTrackerTools, openTicketReader } from './tracker-agent-tools-helpers.js';

test('claude: the tracker tool rows read on the compact thread screen', async ({ harness, page }) => {
  await driveTrackerTools(harness, 'claude', 'compact');
  await openTicketReader(harness, page);
  const viewport = page.viewportSize()!;
  const fits = async (row: Locator) => {
    const box = (await row.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(viewport.width);
  };
  for (const { name, label, preview, opened } of TICKET_ROWS) {
    const row = toolRow(page, label, preview);
    await expect(row, name).toHaveCount(1);
    await fits(row);
    await expandRow(row);
    await opened(row);
    await fits(row);
    await expectRowTextWhole(row);
  }
});
