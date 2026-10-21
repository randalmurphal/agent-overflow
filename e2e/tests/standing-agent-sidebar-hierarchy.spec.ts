// The sidebar hierarchy (docs/specs/standing-agents.md rulings 4, 5 and 30
// to 34, docs/specs/sidebar-thread-groups.md "Member bound"), end to end:
// an agent row shows its pinned children above a bound of eight live ones
// with a show-more footer that reveals the rest; a thread the agent spawned
// into another project nests under the agent's row in that project, apart
// from the agent's row in its own; settled idle children and archived ones share
// one `N done` footer, settled first; a show-more footer, the project's own
// included, carries the dot of an unread hidden row; a collapsed agent row
// and a collapsed group carry one rolled-up dot that Mark All Read clears in
// one call, and the children it marks read fold; a group bounds its members
// the same way, its pinned member above the bound. Screenshots are dark mode.
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp, HarnessMockEventData } from '../src/harness.js';
import { awaitTurnCompleted, threadRows, type ThreadRow } from './thread-tools-helpers.js';
import { awaitSidebarSettled, capture } from './wake-rows-helpers.js';
import { doneIds, liveIds, projectItem, railEntry, rowIn, shellIn } from './sidebar-sections-helpers.js';

interface ThreadGroup {
  id: string;
  name: string;
}

type ReadRow = ThreadRow & { lastReadAt?: number; latestTurnCompletedAt?: number; agentId?: string };

async function allRead(harness: HarnessApp, ids: readonly string[]): Promise<boolean> {
  const rows = (await threadRows(harness)) as ReadRow[];
  return ids.every((id) => {
    const r = rows.find((candidate) => candidate.id === id);
    return r !== undefined && (r.latestTurnCompletedAt === undefined || (r.lastReadAt ?? 0) >= r.latestTurnCompletedAt);
  });
}

const turn = (text: string) => [{ userText: text, items: [{ kind: 'assistant_text', summary: 'done' }] }];

test('an agent row and a group bound their members, fold done work, roll up their dot and nest cross-project children', async ({ harness, page }) => {
  test.setTimeout(180_000);
  const workers = Array.from({ length: 9 }, (_, i) => `Worker ${i + 1}`);
  const sweep = Array.from({ length: 10 }, (_, i) => `Sweep ${i + 1}`);
  const home = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'hierarchy-home', repo: {},
      threads: [
        { title: 'Ops lead', assistant: true, turns: turn('hello') },
        ...workers.map((title) => ({ title, spawnedBy: 'Ops lead', turns: turn(title) })),
        { title: 'Pinned worker', spawnedBy: 'Ops lead', turns: turn('pinned') },
        { title: 'Settled worker', spawnedBy: 'Ops lead', turns: turn('settled') },
        { title: 'Archived worker', spawnedBy: 'Ops lead', archived: true, turns: turn('archived') },
        ...sweep.map((title) => ({ title, turns: turn(title) })),
        { title: 'Sweep pinned', turns: turn('sweep pinned') },
      ],
    }],
  });
  const { projectId, threadIds } = home.projects[0];
  const lead = threadIds[0];
  const workerIds = threadIds.slice(1, 1 + workers.length);
  const [pinned, settled, archived] = threadIds.slice(1 + workers.length, 4 + workers.length);
  const sweepIds = threadIds.slice(4 + workers.length, 4 + workers.length + sweep.length);
  const sweepPinned = threadIds[threadIds.length - 1];
  const away = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'hierarchy-away', repo: {},
      threads: [
        { title: 'Away local', turns: turn('local') },
        { title: 'Away worker', spawnedById: lead, turns: turn('away') },
      ],
    }],
  });
  const [awayLocal, awayChild] = away.projects[0].threadIds;
  // A project with one top-level thread past its own bound, every one unread.
  const many = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'hierarchy-many', repo: {},
      threads: Array.from({ length: 9 }, (_, i) => ({ title: `Loose ${i + 1}`, turns: turn(`loose ${i + 1}`) })),
    }],
  });

  // Unread workers stay live; the settled one is read and folds.
  for (const id of [...workerIds, awayChild, sweepIds[0], ...many.projects[0].threadIds]) await harness.rpc('MarkThreadUnread', id);
  await harness.rpc('PinThread', pinned);
  const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', projectId, 'Sweep');
  await harness.rpc('SetThreadGroup', [...sweepIds, sweepPinned], group.id);
  await harness.rpc('PinThread', sweepPinned);

  await page.emulateMedia({ colorScheme: 'dark' });
  // The assistant's card, three projects and an expanded done list: room
  // for every captured row.
  await page.setViewportSize({ width: 1280, height: 1300 });
  await harness.open(page);
  await awaitSidebarSettled(page);
  await expect(page.locator('html')).toHaveClass(/dark/);
  const homeProject = projectItem(page, projectId);
  const awayProject = projectItem(page, away.projects[0].projectId);
  const agentRow = rowIn(homeProject, lead);
  await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');

  // ---- The project's own show-more carries its unread hidden thread's dot.
  const projectMore = page.getByTestId('project-thread-list-show-more');
  await expect(projectMore).toHaveCount(1);
  await expect(projectMore.getByTestId('project-thread-list-hidden-status')).toHaveAttribute('data-status', 'idle');
  await capture(page, 'hierarchy-project-show-more-unread.png', projectMore);
  // Collapsed, the project carries the same dot.
  const manyProject = page.getByTestId('project-item').filter({ has: page.getByTestId('project-item-label').getByText('hierarchy-many', { exact: true }) });
  await manyProject.getByTestId('project-item-chevron').click();
  await expect(projectMore).toHaveCount(0);
  await expect(manyProject.getByTestId('project-item-status-dot')).toHaveAttribute('data-status', 'idle');
  // Off the chevron, whose hover hides the dot.
  await page.mouse.move(700, 450);

  // ---- The agent's bound in its project: the pinned child, then eight
  // live children, then a show-more for the one past the bound.
  const agentMore = homeProject.locator('[data-agent-member] [data-testid="sidebar-container-show-more"]');
  await expect.poll(async () => (await liveIds(homeProject, '[data-agent-member]')).length).toBe(9);
  expect((await liveIds(homeProject, '[data-agent-member]'))[0]).toBe(pinned);
  await expect(agentMore).toHaveText('Show 1 More');
  // The hidden child is unread: the footer carries its dot.
  await expect(agentMore.getByTestId('sidebar-container-hidden-status')).toHaveAttribute('data-status', 'idle');
  const footer = homeProject.getByTestId('thread-row-agent-done');
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('2 done');
  await expect(footer).toHaveAttribute('data-expanded', 'false');
  await capture(page, 'hierarchy-agent-live-children.png', agentRow, agentMore, footer);
  await agentMore.click();
  await expect.poll(async () => (await liveIds(homeProject, '[data-agent-member]')).length).toBe(10);
  await expect(agentMore).toHaveCount(0);

  // ---- The cross-project child nests under the agent's row in its own
  // project, beside that project's own thread; the agent's row at home
  // does not list it.
  const awayAgentRow = rowIn(awayProject, lead);
  await expect(awayAgentRow).toHaveAttribute('data-sidebar-agent', '');
  const awayRow = rowIn(awayProject, awayChild);
  await expect(awayRow).toHaveCount(1);
  expect(await liveIds(awayProject, '[data-agent-member]')).toEqual([awayChild]);
  expect(await liveIds(homeProject, '[data-agent-member]')).not.toContain(awayChild);
  const awayShell = shellIn(awayProject, awayChild);
  await expect(rowIn(awayProject, awayLocal)).toHaveCount(1);
  await expect(railEntry(awayProject, '[data-agent-member]', awayLocal)).toHaveCount(0);
  await capture(page, 'hierarchy-cross-project-child.png', awayAgentRow, awayShell);

  // ---- The done footer lists the settled worker, then the archived one.
  await footer.getByTestId('thread-row-agent-done-toggle').click();
  await expect.poll(() => doneIds(page)).toEqual([settled, archived]);
  await expect(rowIn(homeProject, archived)).toHaveAttribute('data-archived-child', 'true');
  await expect(rowIn(homeProject, settled)).not.toHaveAttribute('data-archived-child', 'true');
  await capture(page, 'hierarchy-done-footer-expanded.png', agentRow, footer, rowIn(homeProject, archived));
  // Unarchived, the settled worker is still done: it moves to the settled
  // part of the list, not to the live rows, and the count stays.
  await harness.rpc('UnarchiveThread', archived);
  await expect(rowIn(homeProject, archived)).not.toHaveAttribute('data-archived-child', 'true');
  await expect.poll(async () => (await doneIds(page)).sort()).toEqual([settled, archived].sort());
  expect(await liveIds(homeProject, '[data-agent-member]')).not.toContain(archived);
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('2 done');
  await footer.getByTestId('thread-row-agent-done-toggle').click();
  await expect.poll(() => doneIds(page)).toEqual([]);
  await capture(page, 'hierarchy-done-footer-collapsed.png', agentRow, footer);

  // ---- Opening the cross-project child opens it in its own project and
  // workspace: the pane names that project, and a message sent from it runs
  // in that project's checkout.
  await awayRow.click();
  await expect(page.getByTestId('chat-header-title')).toHaveText('Away worker');
  await expect(page.getByTestId('chat-header-project')).toHaveText('hierarchy-away');
  const ranIn = harness.waitForEvent<HarnessMockEventData>('harness:mock', (ev) => ev.cwd === away.projects[0].path, 60_000);
  await page.getByLabel('Message Input').fill('one more thing');
  await page.getByTestId('composer-send').click();
  expect((await ranIn).cwd).toBe(away.projects[0].path);
  await awaitTurnCompleted(harness, awayChild);

  // ---- Collapsed, the agent row carries its children's dot.
  const chevron = agentRow.getByTestId('thread-row-agent-expand');
  await chevron.click();
  await expect(agentRow).toHaveAttribute('data-expanded', 'false');
  // Its count is its live children in this project: the pinned one and
  // nine workers, not the two done.
  await expect(agentRow.getByTestId('thread-row-agent-count')).toHaveText('10');
  const agentDot = agentRow.getByTestId('thread-row-status-dot');
  await expect(agentDot).toHaveAttribute('data-rolled-up', '');
  await expect(agentDot).toHaveAttribute('aria-label', 'Completed');
  await capture(page, 'hierarchy-agent-rolled-up-dot.png', agentRow);
  // The away row folds on its own.
  await expect(awayAgentRow).toHaveAttribute('data-expanded', 'true');

  // ---- Mark All Read clears it in one call; the children it marked read
  // settle and fold, the pinned one stays. It covers this row's children:
  // the away child, unread again once the pane leaves it, stays unread.
  await rowIn(awayProject, awayLocal).click();
  await expect(page.getByTestId('chat-header-title')).toHaveText('Away local');
  await harness.rpc('MarkThreadUnread', awayChild);
  expect(await allRead(harness, [awayChild])).toBe(false);
  await agentRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Mark All Read' }).click();
  await expect.poll(() => allRead(harness, [lead, ...workerIds, pinned]), { timeout: 20_000 }).toBe(true);
  expect(await allRead(harness, [awayChild])).toBe(false);
  await expect(agentDot).toHaveCount(0);
  await chevron.click();
  await expect.poll(() => liveIds(homeProject, '[data-agent-member]')).toEqual([pinned]);
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText(`${workers.length + 2} done`);

  // ---- The group: its pinned member above a bound of eight, then a
  // show-more.
  const groupRow = page.getByTestId('thread-group-row');
  const groupMore = page.locator('[data-group-member] [data-testid="sidebar-container-show-more"]');
  await expect.poll(async () => (await liveIds(homeProject, '[data-group-member]')).length).toBe(9);
  expect((await liveIds(homeProject, '[data-group-member]'))[0]).toBe(sweepPinned);
  await expect(groupMore).toHaveText('Show 2 More');
  // Only Sweep 1 is unread and it sorts first: the hidden members are read.
  await expect(groupMore.getByTestId('sidebar-container-hidden-status')).toHaveCount(0);
  await expect(groupRow.getByTestId('thread-group-row-status-dot')).toHaveCount(0);
  await capture(page, 'hierarchy-group-bound.png', groupRow, rowIn(homeProject, sweepPinned), groupMore);
  await groupMore.click();
  await expect.poll(async () => (await liveIds(homeProject, '[data-group-member]')).length).toBe(11);

  // ---- Collapsed, the group carries its unread member's dot; Mark All
  // Read clears it.
  await groupRow.getByTestId('thread-group-row-expand').click();
  const groupDot = groupRow.getByTestId('thread-group-row-status-dot');
  await expect(groupDot).toHaveAttribute('aria-label', 'Completed');
  await capture(page, 'hierarchy-group-rolled-up-dot.png', groupRow);
  await groupRow.click({ button: 'right' });
  await page.getByRole('menuitem', { name: 'Mark All Read' }).click();
  await expect.poll(() => allRead(harness, [...sweepIds, sweepPinned]), { timeout: 20_000 }).toBe(true);
  await expect(groupDot).toHaveCount(0);
});
