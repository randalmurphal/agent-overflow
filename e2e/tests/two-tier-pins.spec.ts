// Two-tier pins cross the real SQLite -> App RPC -> transport -> Svelte path.
// The first case proves an in-app draft is pinned only after its first real
// send starts. The second proves front/back ordering, the one-boundary divider,
// and both user-facing move gestures against persisted pin_group state.
import { test, expect, type SeedResult } from './fixtures.js';

interface ThreadRow {
  id: string;
  title: string;
  pinnedAt?: number;
  pinGroup?: number;
}

test('first successful send auto-pins a new in-app thread on the front burner', async ({
  harness,
  page,
}) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'auto-pin-app', repo: {} }],
  });

  await harness.open(page);
  await page.getByTestId('project-item-new-thread').first().click();
  await page.getByLabel('Message Input').fill('Start this thread');
  await page.getByTestId('composer-send').click();

  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.map((row) => ({ pinned: row.pinnedAt != null, group: row.pinGroup }));
  }).toEqual([{ pinned: true, group: 0 }]);
  await expect(page.getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'front');
  await harness.waitForEvent('provider:turn_completed');
});

test('front and back blocks move through the context menu and pin right-click', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'two-tier-app',
        repo: {},
        threads: [
          {
            title: 'Front task',
            turns: [{ userText: 'front', items: [{ kind: 'assistant_text', summary: 'front done' }] }],
          },
          {
            title: 'Back task',
            turns: [{ userText: 'back', items: [{ kind: 'assistant_text', summary: 'back done' }] }],
          },
        ],
      },
    ],
  });
  const [frontId, backId] = seed.projects[0].threadIds;
  await harness.rpc('PinThread', frontId);
  await harness.rpc('PinThread', backId);
  await harness.rpc('SetThreadPinGroup', backId, 1);

  await harness.open(page);
  const rows = page.getByTestId('thread-row');
  await expect(rows).toHaveCount(2);
  expect(await page.getByTestId('thread-row-title').allTextContents()).toEqual([
    'Front task',
    'Back task',
  ]);
  await expect(page.getByTestId('thread-pin-group-divider')).toHaveCount(1);

  const backRow = rows.filter({ hasText: 'Back task' });
  await backRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Move to Front Burner' }).click();
  await expect.poll(async () => {
    const stored = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return stored.find((row) => row.id === backId)?.pinGroup;
  }).toBe(0);
  await expect(page.getByTestId('thread-pin-group-divider')).toHaveCount(0);

  await backRow.getByTestId('thread-row-pin').click({ button: 'right' });
  await expect.poll(async () => {
    const stored = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return stored.find((row) => row.id === backId)?.pinGroup;
  }).toBe(1);
  await expect(page.getByTestId('thread-pin-group-divider')).toHaveCount(1);
  await expect(backRow.getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'back');
});
