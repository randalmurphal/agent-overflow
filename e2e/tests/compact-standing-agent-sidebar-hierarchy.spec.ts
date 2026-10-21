// The sidebar hierarchy under the compact layout
// (docs/specs/standing-agents.md rulings 30 to 34,
// docs/specs/sidebar-thread-groups.md "Member bound"): an agent's and a
// group's show-more footers and the agent's row in another project fit a
// phone width and reveal on a tap; Mark All Read is on the agent row's and
// the group row's menu buttons and clears the collapsed rows' rolled-up dot.
// Screenshots are dark mode.
import type { Locator } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { threadRows, type ThreadRow } from './thread-tools-helpers.js';
import { awaitSidebarSettled, capture } from './wake-rows-helpers.js';
import { liveIds, projectItem, rowIn, shellIn } from './sidebar-sections-helpers.js';

interface ThreadGroup {
  id: string;
  name: string;
}

type ReadRow = ThreadRow & { lastReadAt?: number; latestTurnCompletedAt?: number };

async function expectFits(target: Locator, viewportWidth: number): Promise<void> {
  const box = (await target.boundingBox())!;
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(viewportWidth);
  expect(await target.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
}

async function allRead(harness: HarnessApp, ids: readonly string[]): Promise<boolean> {
  const rows = (await threadRows(harness)) as ReadRow[];
  return ids.every((id) => {
    const r = rows.find((candidate) => candidate.id === id);
    return r !== undefined && (r.latestTurnCompletedAt === undefined || (r.lastReadAt ?? 0) >= r.latestTurnCompletedAt);
  });
}

const turn = (text: string) => [{ userText: text, items: [{ kind: 'assistant_text', summary: 'done' }] }];

test('bounded agent and group rows, a cross-project child and Mark All Read fit a phone', async ({ harness, page }) => {
  test.setTimeout(180_000);
  const workers = Array.from({ length: 9 }, (_, i) => `Worker ${i + 1}`);
  const sweep = Array.from({ length: 9 }, (_, i) => `Sweep ${i + 1}`);
  const home = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'compact-hierarchy-home', repo: {},
      threads: [
        { title: 'Ops lead', assistant: true, turns: turn('hello') },
        ...workers.map((title) => ({ title, spawnedBy: 'Ops lead', turns: turn(title) })),
        ...sweep.map((title) => ({ title, turns: turn(title) })),
      ],
    }],
  });
  const { projectId, threadIds } = home.projects[0];
  const lead = threadIds[0];
  const workerIds = threadIds.slice(1, 1 + workers.length);
  const sweepIds = threadIds.slice(1 + workers.length);
  const away = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'compact-hierarchy-away-project-with-a-long-name', repo: {},
      threads: [{ title: 'Away worker with a long title that truncates', spawnedById: lead, turns: turn('away') }],
    }],
  });
  const awayChild = away.projects[0].threadIds[0];
  for (const id of [...workerIds, awayChild, sweepIds[0]]) await harness.rpc('MarkThreadUnread', id);
  const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', projectId, 'Sweep');
  await harness.rpc('SetThreadGroup', sweepIds, group.id);

  await page.emulateMedia({ colorScheme: 'dark' });
  await harness.open(page);
  await awaitSidebarSettled(page);
  const width = page.viewportSize()!.width;
  const homeProject = projectItem(page, projectId);
  const awayProject = projectItem(page, away.projects[0].projectId);
  const agentRow = rowIn(homeProject, lead);
  const agentShell = shellIn(homeProject, lead);

  // Nine unread workers: eight live, a show-more for one.
  const agentMore = homeProject.locator('[data-agent-member] [data-testid="sidebar-container-show-more"]');
  await expect(agentMore).toHaveText('Show 1 More');
  await expect(agentMore.getByTestId('sidebar-container-hidden-status')).toHaveAttribute('data-status', 'idle');
  await expectFits(agentMore, width);
  await agentMore.tap();
  await expect.poll(async () => (await liveIds(homeProject, '[data-agent-member]')).length).toBe(9);
  // The away child nests under the agent's row in its own project.
  const awayAgentShell = shellIn(awayProject, lead);
  const awayShell = shellIn(awayProject, awayChild);
  // Bottom-aligned, the away row keeps the agent row above it in view,
  // wherever the show-more tap left the list scrolled.
  await awayShell.evaluate((el) => el.scrollIntoView({ block: 'end', behavior: 'instant' }));
  expect(await liveIds(awayProject, '[data-agent-member]')).toEqual([awayChild]);
  await expectFits(awayAgentShell, width);
  await expectFits(awayShell, width);
  await capture(page, 'compact-hierarchy-cross-project-child.png', awayAgentShell, awayShell);

  // The group's bound and its show-more.
  const groupRow = page.getByTestId('thread-group-row');
  const groupMore = page.locator('[data-group-member] [data-testid="sidebar-container-show-more"]');
  await groupMore.scrollIntoViewIfNeeded();
  await expect(groupMore).toHaveText('Show 1 More');
  await expectFits(groupMore, width);
  await capture(page, 'compact-hierarchy-group-bound.png', groupMore);
  await groupMore.tap();
  await expect.poll(async () => (await liveIds(homeProject, '[data-group-member]')).length).toBe(9);

  // Collapsed, the agent row carries its children's dot; Mark All Read on
  // its menu button clears it and covers this row's children: the away
  // child stays unread.
  await agentRow.getByTestId('thread-row-agent-expand').tap();
  const agentDot = agentRow.getByTestId('thread-row-status-dot');
  await expect(agentDot).toHaveAttribute('data-rolled-up', '');
  await expectFits(agentShell, width);
  await capture(page, 'compact-hierarchy-agent-rolled-up-dot.png', agentRow);
  await agentShell.getByTestId('thread-row-menu').tap();
  await page.locator('[data-popover]').getByRole('menuitem', { name: 'Mark All Read' }).tap();
  await expect.poll(() => allRead(harness, [lead, ...workerIds]), { timeout: 20_000 }).toBe(true);
  expect(await allRead(harness, [awayChild])).toBe(false);
  await expect(agentDot).toHaveCount(0);
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');

  // The same on the collapsed group.
  await groupRow.getByTestId('thread-group-row-expand').tap();
  const groupDot = groupRow.getByTestId('thread-group-row-status-dot');
  await expect(groupDot).toBeVisible();
  await groupRow.getByTestId('thread-group-row-menu').tap();
  await page.locator('[data-popover]').getByRole('menuitem', { name: 'Mark All Read' }).tap();
  await expect.poll(() => allRead(harness, sweepIds), { timeout: 20_000 }).toBe(true);
  await expect(groupDot).toHaveCount(0);
});
