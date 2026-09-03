// Sidebar thread groups across the real SQLite -> App RPC -> transport ->
// Svelte path. The first case proves the two drag gestures the spec names
// (onto a group row = move in, onto the list outside any group = ungroup),
// plus the collapsed member count. The second proves the menu path: New
// Group… from a thread row opens inline rename, the rename persists, the
// group pins, and deleting the group returns its members to the list.
// Spec: docs/specs/sidebar-thread-groups.md.
import { test, expect, type SeedResult } from './fixtures.js';

interface ThreadRow {
  id: string;
  title: string;
  groupId?: string;
  pinnedAt?: number;
}

interface ThreadGroup {
  id: string;
  name: string;
  pinnedAt?: number;
}

function seedProject(name: string, titles: string[]) {
  return {
    projects: [
      {
        name,
        repo: {},
        threads: titles.map((title) => ({
          title,
          turns: [{ userText: title, items: [{ kind: 'assistant_text', summary: 'done' }] }],
        })),
      },
    ],
  };
}

test('drag onto a group moves in, drag onto the list outside it moves out', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>(
    'HarnessSeed',
    seedProject('groups-drag-app', ['Alpha', 'Beta', 'Gamma']),
  );
  const { projectId, threadIds } = seed.projects[0];
  const [alphaId, betaId] = threadIds;
  const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', projectId, 'Port work');
  await harness.rpc('PinThread', alphaId);
  await harness.rpc('SetThreadGroup', [alphaId], group.id);

  await harness.open(page);
  const groupRow = page.getByTestId('thread-group-row');
  await expect(groupRow).toHaveCount(1);
  // Grouping stripped Alpha's pin: the group carries the only pin affordance.
  await expect(groupRow.getByTestId('thread-row-pin')).toHaveAttribute('aria-label', 'Pin Group');
  const alphaRow = page.getByTestId('thread-row').filter({ hasText: 'Alpha' });
  await expect(alphaRow.getByTestId('thread-row-pin')).toHaveCount(0);

  // Drag Beta onto the group row.
  const betaRow = page.getByTestId('thread-row').filter({ hasText: 'Beta' });
  await betaRow.dragTo(groupRow);
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.find((row) => row.id === betaId)?.groupId;
  }).toBe(group.id);

  // Collapse: the count is the member total, and members leave the DOM.
  await groupRow.getByTestId('thread-group-row-expand').click();
  await expect(groupRow.getByTestId('thread-group-row-count')).toHaveText('2');
  await expect(page.getByTestId('thread-row')).toHaveCount(1);
  await groupRow.getByTestId('thread-group-row-expand').click();
  await expect(page.getByTestId('thread-row')).toHaveCount(3);

  // Drag Alpha out: onto a top-level row that is not in any group.
  const gammaRow = page.getByTestId('thread-row').filter({ hasText: 'Gamma' });
  await alphaRow.dragTo(gammaRow);
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.find((row) => row.id === alphaId)?.groupId ?? null;
  }).toBeNull();
  await expect(groupRow.getByTestId('thread-group-row-time')).toBeVisible();
  // Leaving a group does not restore the pin.
  await expect(alphaRow.getByTestId('thread-row-pin')).toHaveAttribute('aria-label', 'Pin Thread');
});

test('New Group… from a thread row renames inline, pins, and deletes back to the list', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>(
    'HarnessSeed',
    seedProject('groups-menu-app', ['One', 'Two']),
  );
  const [oneId] = seed.projects[0].threadIds;

  await harness.open(page);
  const oneRow = page.getByTestId('thread-row').filter({ hasText: 'One' });
  await oneRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Move to Group' }).hover();
  await page.getByRole('menuitem', { name: 'New Group…' }).click();

  const groupRow = page.getByTestId('thread-group-row');
  await expect(groupRow).toHaveCount(1);
  const renameInput = groupRow.getByLabel('Rename Group');
  await expect(renameInput).toBeFocused();
  await renameInput.fill('Release prep');
  await renameInput.press('Enter');
  await expect(groupRow.getByTestId('thread-group-row-name')).toHaveText('Release prep');
  await expect.poll(async () => {
    const groups = await harness.rpc<ThreadGroup[]>('ListThreadGroups');
    return groups.map((g) => g.name);
  }).toEqual(['Release prep']);
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return Boolean(rows.find((row) => row.id === oneId)?.groupId);
  }).toBe(true);

  // Grouped rows lose the pin items; the group gains them.
  await oneRow.click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Pin Thread' })).toHaveCount(0);
  await expect(page.getByRole('menuitem', { name: 'Remove from Group' })).toHaveCount(1);
  await page.keyboard.press('Escape');

  await groupRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Pin Group' }).click();
  await expect(groupRow.getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'front');
  await expect.poll(async () => {
    const groups = await harness.rpc<ThreadGroup[]>('ListThreadGroups');
    return groups[0]?.pinnedAt != null;
  }).toBe(true);

  // Delete returns the member to the top level and keeps the thread.
  await groupRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Delete Group' }).click();
  const confirm = page.getByRole('dialog');
  if (await confirm.count()) {
    await confirm.getByRole('button', { name: 'Delete' }).click();
  }
  await expect(page.getByTestId('thread-group-row')).toHaveCount(0);
  await expect(page.getByTestId('thread-row')).toHaveCount(2);
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.map((row) => row.groupId ?? null);
  }).toEqual([null, null]);
});
