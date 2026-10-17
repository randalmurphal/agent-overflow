// Shared body of standing-agent-spawn-placement.spec.ts and its compact
// twin (docs/specs/standing-agents.md ruling 36): an assistant spawns one
// thread with each placement into another project, each answer reaches it
// and marks the worker read, and the sidebar shows the nested one under the
// agent row (folded into its done footer once settled), the grouped one in
// its group and the standalone one as a plain row of its own project; then
// the row menu (and on desktop a drag onto the group) moves the workers
// between placements. Screenshots are dark mode.
import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadRows,
  threadToolsScenario,
} from './thread-tools-helpers.js';
import { awaitSidebarSettled, capture } from './wake-rows-helpers.js';

interface PlacedRow {
  id: string;
  title: string;
  projectId: string;
  groupId?: string;
  placement?: string;
  spawnedByThreadId?: string;
  lastReadAt?: number;
  latestTurnCompletedAt?: number;
}

export type Layout = 'desktop' | 'compact';

function row(page: Page, id: string): Locator {
  return page.locator(`[data-sidebar-thread-id="${id}"]`);
}

function shell(page: Page, id: string): Locator {
  return page.locator('[data-testid="thread-row-shell"]', { has: row(page, id) });
}

async function rowOf(harness: HarnessApp, id: string): Promise<PlacedRow | undefined> {
  return ((await threadRows(harness)) as unknown as PlacedRow[]).find((r) => r.id === id);
}

async function placementOf(harness: HarnessApp, id: string): Promise<string | undefined> {
  return (await rowOf(harness, id))?.placement;
}

/** Where the sidebar shows a row: under the agent, in a group, or loose in its project's list. */
async function sidebarPlace(page: Page, id: string, projectId: string): Promise<string> {
  return await page.evaluate(({ id, projectId }) => {
    const el = document.querySelector(`[data-sidebar-thread-id="${id}"]`);
    if (!el) return 'absent';
    const item = el.closest('[role="listitem"]');
    if (item?.hasAttribute('data-agent-member')) return 'agent';
    if (item?.hasAttribute('data-group-member')) return 'group';
    const project = el.closest('[data-testid="project-item"]');
    return project?.getAttribute('data-project-id') === projectId ? 'project' : 'elsewhere';
  }, { id, projectId });
}

async function openMenu(page: Page, layout: Layout, id: string): Promise<Locator> {
  if (layout === 'desktop') await row(page, id).click({ button: 'right' });
  else await shell(page, id).getByTestId('thread-row-menu').tap();
  const menu = page.locator('[data-popover]').filter({ has: page.getByRole('menuitem') }).last();
  await expect(menu).toBeVisible();
  return menu;
}

async function choose(page: Page, layout: Layout, id: string, label: string): Promise<void> {
  const menu = await openMenu(page, layout, id);
  const target = menu.getByRole('menuitem', { name: label, exact: true });
  if (layout === 'desktop') await target.click();
  else await target.tap();
}

async function moveToGroup(page: Page, layout: Layout, id: string, group: string): Promise<void> {
  const menu = await openMenu(page, layout, id);
  const submenu = menu.getByRole('menuitem', { name: 'Move to Group' });
  if (layout === 'desktop') await submenu.hover();
  else await submenu.tap();
  const target = page.getByRole('menuitem', { name: group, exact: true });
  if (layout === 'desktop') await target.click();
  else await target.tap();
}

const turn = (text: string) => [{ userText: text, items: [{ kind: 'assistant_text', summary: 'ready' }] }];

export async function runSpawnPlacement(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  const tag = layout === 'desktop' ? 'placement' : 'compact-placement';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-home`, repo: {}, threads: [{ title: 'Ops lead', assistant: true, turns: turn('hello') }] },
      { name: `${tag}-workers`, repo: {}, threads: [] },
    ],
  });
  const [home, workers] = seed.projects;
  const agent = home.threadIds[0];

  await setScenario(harness, workers.path, plainScenario({ name: `${tag}-worker`, provider: 'claude', texts: ['Done.'] }));
  const spawn = (title: string, extra: Record<string, unknown>) => ({
    call: { tool: 'thread_spawn', args: { prompt: `Do ${title}.`, title, project_id: workers.projectId, wait_seconds: 60, ...extra }, timeoutMs: 90_000 },
  });
  await setScenario(
    harness,
    home.path,
    threadToolsScenario({
      name: `${tag}-agent`,
      provider: 'claude',
      turns: [{
        steps: [
          spawn('Nested worker', {}),
          spawn('Grouped worker', { group: 'Sweep' }),
          spawn('Standalone worker', { placement: 'standalone' }),
        ],
        text: 'Spawned three.',
      }],
      afterTurns: 'silent',
    }),
  );

  await page.emulateMedia({ colorScheme: 'dark' });
  await harness.open(page);
  await awaitSidebarSettled(page);
  await harness.rpc('StartSession', agent);
  const agentTurn = awaitTurnCompleted(harness, agent, 120_000);
  await harness.rpc('SendMessage', agent, 'spawn three workers', null);
  await agentTurn;

  const spawned = async (title: string) => {
    const found = ((await threadRows(harness)) as unknown as PlacedRow[]).find((r) => r.title === title);
    expect(found, title).toBeDefined();
    return found!;
  };
  const nested = await spawned('Nested worker');
  const grouped = await spawned('Grouped worker');
  const standalone = await spawned('Standalone worker');

  // Every placement keeps the spawn record and the reply path: each worker
  // names the agent as its spawner, and its answer reached the agent's
  // waiting call, which marks it read.
  for (const [worker, placement] of [[nested, 'nested'], [grouped, 'grouped'], [standalone, 'standalone']] as const) {
    expect(worker.spawnedByThreadId, worker.title).toBe(agent);
    expect(worker.projectId, worker.title).toBe(workers.projectId);
    await expect.poll(() => placementOf(harness, worker.id), { message: worker.title }).toBe(placement);
    await expect.poll(async () => {
      const r = await rowOf(harness, worker.id);
      return r?.latestTurnCompletedAt !== undefined && (r.lastReadAt ?? 0) >= r.latestTurnCompletedAt;
    }, { message: `${worker.title} read`, timeout: 30_000 }).toBe(true);
  }
  // The sidebar: the nested worker is the agent's, settled, so it is in
  // the done footer; the other two are rows of the workers project, live.
  const footer = page.getByTestId('thread-row-agent-done');
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
  await expect.poll(() => sidebarPlace(page, grouped.id, workers.projectId)).toBe('group');
  await expect.poll(() => sidebarPlace(page, standalone.id, workers.projectId)).toBe('project');
  await expect(row(page, grouped.id).getByTestId('thread-row-status-dot')).toHaveCount(0);
  await expect(row(page, standalone.id).getByTestId('thread-row-status-dot')).toHaveCount(0);
  const sweep = page.getByTestId('thread-group-row').filter({ hasText: 'Sweep' });
  await expect(sweep).toHaveCount(1);
  const toggle = footer.getByTestId('thread-row-agent-done-toggle');
  const tapOrClick = async (target: Locator) => {
    if (layout === 'desktop') await target.click();
    else await target.tap();
  };
  await tapOrClick(toggle);
  await expect.poll(() => sidebarPlace(page, nested.id, workers.projectId)).toBe('agent');
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `${tag}-three-placements.png`, row(page, agent), row(page, nested.id), sweep, row(page, grouped.id), row(page, standalone.id));
  await tapOrClick(toggle);
  await expect(row(page, nested.id)).toHaveCount(0);

  // The menu of a placed worker names the agent to nest under.
  const menu = await openMenu(page, layout, standalone.id);
  await expect(menu.getByRole('menuitem', { name: 'Nest Under Ops lead', exact: true })).toBeVisible();
  await expect(menu.getByRole('menuitem', { name: 'Move to Group' })).toBeVisible();
  await expect(menu.getByRole('menuitem', { name: 'Make Standalone', exact: true })).toHaveCount(0);
  await capture(page, `${tag}-child-menu.png`, menu);
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menuitem')).toHaveCount(0);

  // Standalone to nested: it leaves its project's list for the agent's
  // done footer (settled, read).
  await choose(page, layout, standalone.id, 'Nest Under Ops lead');
  await expect.poll(() => placementOf(harness, standalone.id)).toBe('nested');
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('2 done');
  await expect.poll(() => sidebarPlace(page, standalone.id, workers.projectId)).not.toBe('project');

  // Grouped to standalone, then into the group again: by a drag on desktop,
  // from the menu under the compact layout (which does not drag).
  await choose(page, layout, grouped.id, 'Remove from Group');
  await expect.poll(() => placementOf(harness, grouped.id)).toBe('standalone');
  await expect.poll(() => sidebarPlace(page, grouped.id, workers.projectId)).toBe('project');
  if (layout === 'desktop') await row(page, grouped.id).dragTo(sweep);
  else await moveToGroup(page, layout, grouped.id, 'Sweep');
  await expect.poll(() => placementOf(harness, grouped.id)).toBe('grouped');
  await expect.poll(() => sidebarPlace(page, grouped.id, workers.projectId)).toBe('group');

  // Nested to standalone, from the done footer's list.
  await tapOrClick(toggle);
  await expect(row(page, nested.id)).toBeVisible();
  await choose(page, layout, nested.id, 'Make Standalone');
  await expect.poll(() => placementOf(harness, nested.id)).toBe('standalone');
  await expect.poll(() => sidebarPlace(page, nested.id, workers.projectId)).toBe('project');
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
  await capture(page, `${tag}-after-moves.png`, row(page, nested.id), row(page, grouped.id), footer);
}
