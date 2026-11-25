// The Issues pane's Board screen under the compact layout
// (docs/specs/issues.md, "Compact layout"), through the shipped UI, the
// real backend and the fake Jira: the tools row opens the pane full width
// on the thread screen; the board shows one status at a time, the first
// in-progress one until another is picked from the strip of statuses with
// their counts; the header's menu holds Display and Only mine, and Only
// mine narrows the column and the strip's count to the verified account's
// cards. The desktop screen is issues-board.spec.ts.

import type { Locator } from '@playwright/test';

import { expect, test } from './fixtures.js';
import { connectFakeJiraBoard } from './tracker-helpers.js';

function card(pane: Locator, issueId: string): Locator {
  return pane.locator(`[data-testid="issues-card"][data-issue="${issueId}"]`);
}

test('the compact board shows one status from its strip and keeps Only mine in its menu', async ({ harness, page }) => {
  // The seed's sprint holds WEB-1 (In Progress) and WEB-2 (To Do); WEB-4
  // joins WEB-1, assigned to the verified account.
  await connectFakeJiraBoard(harness, 'compact-issues-board', {
    projects: [],
    boards: [],
    issues: [{ id: '10104', key: 'WEB-4', project: 'WEB', type: '10002', status: '10001', summary: 'Mine in progress', sprints: [1], assignee: 'acc-user1' }],
  });
  await harness.open(page);
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');
  await page.getByTestId('sidebar-tool-issues').click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  const pane = page.getByTestId('issues-pane');
  await expect(pane).toBeVisible();

  // One column at a time: In Progress first, the strip naming all three.
  const tabs = pane.getByTestId('issues-column-tab');
  await expect(tabs).toHaveCount(3, { timeout: 30_000 });
  await expect(tabs.nth(0)).toContainText('To Do');
  await expect(tabs.nth(1)).toContainText('In Progress');
  await expect(tabs.nth(1)).toHaveAttribute('aria-selected', 'true');
  await expect(tabs.nth(1)).toContainText('2');
  await expect(pane.getByTestId('issues-column')).toHaveCount(1);
  await expect(pane.getByTestId('issues-card')).toHaveCount(2);
  await expect(card(pane, '10101')).toBeVisible();
  await expect(card(pane, '10104')).toBeVisible();

  await tabs.nth(0).click();
  await expect(tabs.nth(0)).toHaveAttribute('aria-selected', 'true');
  await expect(pane.getByTestId('issues-card')).toHaveCount(1);
  await expect(card(pane, '10102')).toContainText('Second task');
  await tabs.nth(1).click();

  // The header has no Only mine of its own; the menu holds it with Display.
  await expect(pane.getByTestId('issues-only-mine')).toHaveCount(0);
  await pane.getByTestId('issues-display').click();
  const onlyMine = page.getByTestId('issues-only-mine-item');
  await expect(onlyMine).toHaveAttribute('aria-checked', 'false');
  await expect(page.getByTestId('issues-display-assignee')).toHaveAttribute('aria-checked', 'true');
  await onlyMine.click();
  await expect(onlyMine).toHaveAttribute('aria-checked', 'true');
  await page.keyboard.press('Escape');
  await expect(onlyMine).toHaveCount(0);
  await expect(pane.getByTestId('issues-card')).toHaveCount(1);
  await expect(card(pane, '10104')).toContainText('Mine in progress');
  await expect(tabs.nth(1)).toContainText('1');
});
