// Sidebar thread groups across the real SQLite -> App RPC -> transport ->
// Svelte path. The first case proves the two drag gestures the spec names
// (onto a group row = move in, onto the list outside any group = ungroup),
// the collapsed member count, and that a member pins inside its group and
// loses the pin on leaving it. A drag that starts on a row's worktree
// sublabel moves the thread instead of selecting the label's text. The
// menu case proves the menu path: New Group…
// from a thread row opens inline rename, the rename persists, a member pins
// from its menu while the group offers no pin, the group section sits above
// the pin blocks behind a divider, and deleting the group returns its
// members to the list unpinned.
// Spec: docs/specs/sidebar-thread-groups.md.
// Collapse keeps only the focused member visible across pane switches,
// backend updates and reload.
import { test, expect, type SeedResult } from './fixtures.js';
import { attachWorktree, seedWorktreeProject } from './worktree-removal-helpers.js';

interface ThreadRow {
  id: string;
  title: string;
  groupId?: string;
  pinnedAt?: number;
  isDraft?: boolean;
}

interface ThreadGroup {
  id: string;
  name: string;
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
  // Groups are not pinnable, and grouping stripped Alpha's pin: a thread
  // starts unpinned in its group and pins there on its own.
  await expect(groupRow.getByTestId('thread-row-pin')).toHaveCount(0);
  const alphaRow = page.getByTestId('thread-row').filter({ hasText: 'Alpha' });
  const alphaPin = alphaRow.getByTestId('thread-row-pin');
  await expect(alphaPin).toHaveAttribute('aria-label', 'Pin Thread');
  await alphaRow.hover();
  await alphaPin.click();
  await expect(alphaPin).toHaveAttribute('data-pin-group', 'front');
  await alphaPin.click({ button: 'right' });
  await expect(alphaPin).toHaveAttribute('data-pin-group', 'back');
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    const alpha = rows.find((row) => row.id === alphaId);
    return [alpha?.groupId, alpha?.pinnedAt != null];
  }).toEqual([group.id, true]);

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
  // Leaving a group clears the pin it held there.
  await expect(alphaPin).toHaveAttribute('aria-label', 'Pin Thread');
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.find((row) => row.id === alphaId)?.pinnedAt ?? null;
  }).toBeNull();
});

test('a drag started on the worktree sublabel moves the thread', async ({ harness, page }) => {
  const project = await seedWorktreeProject(harness, 'groups-sublabel-drag', ['Base row', 'Worktree row'], ['sublabel-drag']);
  const [, worktreeId] = project.threadIds;
  await attachWorktree(harness, worktreeId, 'sublabel-drag');
  const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', project.projectId, 'Sublabel drop');

  await harness.open(page);
  const groupRow = page.getByTestId('thread-group-row');
  const label = page.getByTestId('thread-row-worktree-name');
  await expect(label).toHaveText('sublabel-drag');
  await label.dragTo(groupRow);

  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.find((row) => row.id === worktreeId)?.groupId;
  }).toBe(group.id);
  expect(await page.evaluate(() => window.getSelection()?.toString() ?? '')).toBe('');
});

test('New Group… from a thread row renames inline, pins a member, and deletes back to the list', async ({
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

  // A grouped row keeps its own pin items; the group has none.
  await oneRow.click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Remove from Group' })).toHaveCount(1);
  await page.getByRole('menuitem', { name: 'Pin Thread' }).click();
  await expect(oneRow.getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'front');
  await groupRow.click({ button: 'right' });
  await expect(page.getByRole('menuitem', { name: 'Pin Group' })).toHaveCount(0);
  await page.keyboard.press('Escape');

  // The group section sits above a pinned top-level thread, a divider
  // between them, and a front-burner member is not a numbered jump target.
  const twoRow = page.getByTestId('thread-row').filter({ hasText: 'Two' });
  await twoRow.hover();
  await twoRow.getByTestId('thread-row-pin').click();
  await expect(twoRow.getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'front');
  const list = page.getByTestId('project-thread-list');
  await expect.poll(async () => list.locator('[data-sidebar-group-id], [data-sidebar-thread-id]').evaluateAll(
    (rows) => rows.map((row) => row.textContent?.includes('Release prep') ? 'group' : row.textContent?.includes('Two') ? 'two' : 'one'),
  )).toEqual(['group', 'one', 'two']);
  await expect(list.getByTestId('thread-section-divider')).toHaveCount(1);
  await expect(oneRow).not.toHaveAttribute('data-sidebar-jump-target');
  await expect(twoRow).toHaveAttribute('data-sidebar-jump-target', '');

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
    return rows.find((row) => row.id === oneId)?.groupId ?? null;
  }).toBeNull();
  // The member's in-group pin left with the group; Two keeps its own.
  await expect.poll(async () => {
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    return rows.find((row) => row.id === oneId)?.pinnedAt ?? null;
  }).toBeNull();
  await expect(oneRow.getByTestId('thread-row-pin')).toHaveAttribute('aria-label', 'Pin Thread');
  await expect(twoRow.getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'front');
});

test('a collapsed group keeps only its focused member through focus changes and reload', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', seedProject('group-collapse', ['Alpha', 'Beta', 'Outside']));
  const { projectId, threadIds } = seed.projects[0];
  const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', projectId, 'Focused work');
  await harness.rpc('SetThreadGroup', threadIds.slice(0, 2), group.id);
  await harness.open(page);
  const groupRow = page.getByTestId('thread-group-row');
  const alpha = page.locator(`[data-sidebar-thread-id="${threadIds[0]}"]`);
  const beta = page.locator(`[data-sidebar-thread-id="${threadIds[1]}"]`);
  const members = page.locator('[data-group-member] [data-sidebar-thread-id]');
  await alpha.click();
  await beta.click({ modifiers: ['ControlOrMeta'] });
  const panes = page.locator('section[data-pane-kind="thread"]');
  await expect(panes).toHaveCount(2);
  const alphaPane = panes.filter({ has: page.getByTestId('user-message-summary').filter({ hasText: 'Alpha' }) });
  const betaPane = panes.filter({ has: page.getByTestId('user-message-summary').filter({ hasText: 'Beta' }) });

  await groupRow.getByTestId('thread-group-row-expand').click();
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await expect(groupRow.getByTestId('thread-group-row-count')).toHaveText('2');
  await expect(members).toHaveCount(1);
  await expect(beta).toBeVisible();
  await expect(alpha).toHaveCount(0);

  await alphaPane.getByLabel('Message Input').click();
  await expect(alpha).toBeVisible();
  await expect(beta).toHaveCount(0);
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await betaPane.getByLabel('Message Input').click();
  await expect(beta).toBeVisible();
  await expect(alpha).toHaveCount(0);

  await harness.rpc('RenameThreadGroup', group.id, 'Renamed work');
  await expect(groupRow.getByTestId('thread-group-row-name')).toHaveText('Renamed work');
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await expect(members).toHaveCount(1);
  await page.reload();
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
  await expect(beta).toBeVisible();
  await expect(alpha).toHaveCount(0);

  for (let repeat = 0; repeat < 2; repeat++) {
    await groupRow.getByTestId('thread-group-row-expand').click();
    await expect(members).toHaveCount(2);
    await groupRow.getByTestId('thread-group-row-expand').click();
    await expect(groupRow).toHaveAttribute('data-expanded', 'false');
    await expect(members).toHaveCount(1);
    await expect(beta).toBeVisible();
  }

  await page.locator(`[data-sidebar-thread-id="${threadIds[2]}"]`).click();
  await expect(members).toHaveCount(0);
  await expect(groupRow).toHaveAttribute('data-expanded', 'false');
});


for (const entry of ['button', 'menu'] as const) {
  test(`new thread from group ${entry} keeps membership through draft cleanup and first send`, async ({ harness, page }) => {
    const seed = await harness.rpc<SeedResult>('HarnessSeed', seedProject(`group-draft-${entry}`, []));
    const { projectId } = seed.projects[0];
    const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', projectId, 'Draft work');
    await harness.open(page);
    const groupRow = page.getByTestId('thread-group-row');
    await groupRow.getByTestId('thread-group-row-expand').click();
    if (entry === 'button') {
      await groupRow.hover();
      await groupRow.getByRole('button', { name: 'New Thread in Group' }).click();
    } else {
      await groupRow.click({ button: 'right' });
      await page.getByRole('menuitem', { name: 'New Thread', exact: true }).click();
    }
    await expect(groupRow).toHaveAttribute('data-expanded', 'false');
    const input = page.getByLabel('Message Input');
    await expect(input).toBeVisible();
    expect(await harness.rpc<ThreadRow[]>('HarnessListThreadRows')).toHaveLength(0);
    await input.fill('Draft in this group');
    await expect.poll(async () => {
      const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
      return rows.map((row) => ({ groupId: row.groupId, isDraft: row.isDraft }));
    }).toEqual([{ groupId: group.id, isDraft: true }]);
    await expect(page.getByTestId('thread-row')).toHaveCount(1);
    await expect(groupRow).toHaveAttribute('data-expanded', 'false');
    await input.fill('');
    await expect.poll(() => harness.rpc<ThreadRow[]>('HarnessListThreadRows')).toHaveLength(0);
    await input.fill('Start the grouped thread');
    await page.getByTestId('composer-send').click();
    await expect(page.getByTestId('user-message-summary').filter({ hasText: 'Start the grouped thread' })).toBeVisible();
    await expect(page.getByTestId('assistant-message-body').first()).toBeVisible();
    await expect.poll(async () => {
      const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
      return rows.map((row) => ({ groupId: row.groupId, draft: !!row.isDraft, pinned: row.pinnedAt != null }));
    }).toEqual([{ groupId: group.id, draft: false, pinned: false }]);
  });
}
