// What a connection row says about the token in its key file, against the
// fake Jira, which accepts JIRA_TOKEN and refuses any other: a check is
// the row's sign-in only while the file holds the token it verified, and
// a failure to read the file is history once the file holds a token. The
// key file is written as an editor or a move would write it. The row
// states and their sentences are trackerConnectionStatus.test.ts; the
// binding of each outcome to its token is internal/trackerapp.

import { rename, utimes, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import { expect, test } from './fixtures.js';
import { JIRA_ACCOUNT, JIRA_SITE, JIRA_TOKEN, openIssuesPage, seedFakeJira, type TrackerConnection } from './tracker-helpers.js';

async function addJira(harness: Parameters<typeof seedFakeJira>[0]): Promise<TrackerConnection> {
  await seedFakeJira(harness);
  return harness.rpc<TrackerConnection>('AddTrackerConnection', { kind: 'jira', site: JIRA_SITE, account: JIRA_ACCOUNT });
}

test('a token moved over the checked one, with an older time, is not signed in', async ({ harness, page }) => {
  const jira = await addJira(harness);
  await writeFile(jira.keyFilePath, JIRA_TOKEN, { mode: 0o600 });
  const verified = await harness.rpc<TrackerConnection>('VerifyTrackerConnection', jira.id);
  expect(verified.problem ?? null).toBeNull();

  // A token saved an hour ago elsewhere, moved into place: a rename keeps
  // its modification time, which is older than the check.
  const saved = path.join(path.dirname(jira.keyFilePath), 'saved-token');
  await writeFile(saved, 'another-jira-token-0002', { mode: 0o600 });
  const hourAgo = new Date(Date.now() - 3600_000);
  await utimes(saved, hourAgo, hourAgo);
  await rename(saved, jira.keyFilePath);

  await harness.open(page);
  await openIssuesPage(page, 'Connections');
  const row = page.locator('li[data-kind="jira"]');
  await expect(row.getByTestId('tracker-connection-status')).toHaveText('Check needed');
  await expect(row.getByTestId('tracker-connection-detail')).toHaveText(
    'It was last checked as User 1 (user1@example.com). Check the connection to confirm the token in the key file.',
  );
  await expect(row.getByTestId('tracker-connection-setup')).toBeVisible();
});

test('a check of an empty key file is history once the file holds a token', async ({ harness, page }) => {
  const jira = await addJira(harness);
  const empty = await harness.rpc<TrackerConnection>('VerifyTrackerConnection', jira.id);
  expect(empty.problem?.message).toBe('the Jira API token file is empty');
  await writeFile(jira.keyFilePath, JIRA_TOKEN, { mode: 0o600 });

  await harness.open(page);
  await openIssuesPage(page, 'Connections');
  const row = page.locator('li[data-kind="jira"]');
  await expect(row.getByTestId('tracker-connection-status')).toHaveText('Key file changed since the last failure');
  await expect(row.getByTestId('tracker-connection-detail')).toHaveText(
    'Check the connection to confirm the token in the key file. Before the change: the Jira API token file is empty.',
  );
  await row.getByTestId('tracker-connection-setup-check').click();
  await expect(row.getByTestId('tracker-connection-status')).toContainText('Signed in as User 1');
});
