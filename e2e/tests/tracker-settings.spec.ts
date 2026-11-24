// Settings > Issues against the fake Jira: a person adds a Jira connection,
// puts the token in the key file the page names (as pasting it in an
// editor would), checks it and sees who it is signed in as; another
// window shows the connection without a reload. The person lists the
// site's board on a project through the picker, and after a reload both
// pages show the same state. The token page and the editor open only on
// the computer itself, which the harness refuses (external opens are
// isolated), so the spec writes the file; their calls are covered by
// TrackerConnectionsSettings.test.ts.

import { writeFile } from 'node:fs/promises';
import type { Page } from '@playwright/test';
import { expect, test, type SeedResult } from './fixtures.js';
import { JIRA_ACCOUNT, JIRA_SITE, JIRA_TOKEN, seedFakeJira } from './tracker-helpers.js';

async function openIssuesPage(page: Page, name: 'Connections' | 'Boards'): Promise<void> {
  if (!(await page.getByRole('tablist', { name: 'Settings Sections' }).isVisible())) {
    await page.getByTestId('sidebar-settings-button').click();
  }
  await page.getByRole('tablist', { name: 'Settings Sections' }).getByRole('tab', { name, exact: true }).click();
}

test('a Jira connection is set up in Settings and its board listed on a project', async ({ harness, page, context }) => {
  await seedFakeJira(harness);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', { projects: [{ name: 'tracker-settings', repo: {} }] });
  const projectId = seed.projects[0].projectId;

  await harness.open(page);
  const other = await context.newPage();
  await harness.open(other);
  await openIssuesPage(page, 'Connections');
  await openIssuesPage(other, 'Connections');
  await expect(other.getByTestId('settings-tracker-connections-empty')).toBeVisible();

  await page.getByTestId('tracker-connection-add').click();
  await page.getByTestId('tracker-connection-add-site').fill(JIRA_SITE);
  await page.getByTestId('tracker-connection-add-account').fill(JIRA_ACCOUNT);
  await page.getByTestId('tracker-connection-add-submit').click();

  const row = page.locator('li[data-kind="jira"]');
  await expect(row.getByTestId('tracker-connection-status')).toHaveText('Needs a token');
  await expect(row.getByTestId('tracker-connection-key-state')).toHaveText('The key file is empty.');
  const keyPath = (await row.getByTestId('tracker-connection-key-path').textContent()) ?? '';
  expect(keyPath).not.toBe('');
  await writeFile(keyPath, JIRA_TOKEN, { mode: 0o600 });
  await row.getByTestId('tracker-connection-setup-check').click();
  await expect(row.getByTestId('tracker-connection-status')).toContainText('Signed in as User 1');
  await expect(row.getByTestId('tracker-connection-setup')).toHaveCount(0);

  // The other window follows tracker:connections_updated.
  await expect(other.locator('li[data-kind="jira"]').getByTestId('tracker-connection-status')).toContainText('Signed in as User 1');

  await openIssuesPage(page, 'Boards');
  await page.getByTestId('settings-tracker-boards-project').selectOption(projectId);
  await expect(page.getByTestId('settings-tracker-boards-empty')).toBeVisible();
  await page.getByTestId('tracker-board-add-open').click();
  await page.getByTestId('tracker-board-add-project').filter({ hasText: 'WEB' }).click();
  await page.getByTestId('tracker-board-add-board').filter({ hasText: 'WEB board' }).click();
  await expect(page.getByTestId('tracker-board-name')).toHaveText(['WEB board']);
  await expect(page.getByTestId('tracker-board-opens-first')).toBeVisible();

  await page.reload();
  await openIssuesPage(page, 'Boards');
  await page.getByTestId('settings-tracker-boards-project').selectOption(projectId);
  await expect(page.getByTestId('tracker-board-name')).toHaveText(['WEB board']);
  await expect(page.getByTestId('tracker-board-opens-first')).toBeVisible();
  await openIssuesPage(page, 'Connections');
  await expect(page.locator('li[data-kind="jira"]').getByTestId('tracker-connection-status')).toContainText('Signed in as User 1');
  await other.close();
});
