// Shared body of owned-workers-sidebar.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Sidebar", ruling 36). An assistant is one
// row in the Assistants section and none in a project. A thread spawns three
// workers: the default one is owned and has no row, a standalone one is a
// plain row of its project and a grouped one a member of its group. The
// spawner's row counts the three while they run and stops once they finish.
// Remove from Sidebar takes the standalone worker out while the pane
// showing it stays. After a handoff, Previous Threads lists the assistant's
// past thread and opens it. Screenshots are dark mode.
import type { Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  advanceGate,
  assistantHome,
  awaitGate,
  awaitMockInput,
  awaitToolAnswer,
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadRows,
  threadToolsScenario,
} from './thread-tools-helpers.js';
import { awaitSidebarSettled, capture } from './wake-rows-helpers.js';
import {
  assistantRow,
  assistantsSection,
  chooseRowMenuItem,
  openPreviousThreads,
  openRowMenu,
  previousThreadIds,
  projectItem,
  projectRow,
  projectsSection,
  rowIn,
  type Layout,
} from './sidebar-sections-helpers.js';

interface PlacedRow {
  id: string;
  title: string;
  archived: boolean;
  placement?: string;
}

const turn = (text: string) => [{ userText: text, items: [{ kind: 'assistant_text', summary: 'ready' }] }];

async function placementOf(harness: HarnessApp, id: string): Promise<string | undefined> {
  return ((await threadRows(harness)) as unknown as PlacedRow[]).find((r) => r.id === id)?.placement;
}

/** The chat surface's thread: what the pane shows, whether or not it is on screen. */
function shownThread(page: Page) {
  return page.locator('[data-ui-surface="chat"]');
}

export async function runOwnedWorkersSidebar(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  const tag = layout === 'desktop' ? 'owned' : 'compact-owned';
  const press = async (target: ReturnType<Page['locator']>) => {
    if (layout === 'desktop') await target.click();
    else await target.tap();
  };
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `${tag}-lead`, repo: {},
        threads: [
          { title: 'Ops', assistant: true, turns: turn('hello') },
          { title: 'Release lead', turns: turn('hello') },
          // A seeded spawn is owned, as a live one with no placement is.
          { title: 'Seeded worker', spawnedBy: 'Release lead', turns: turn('w') },
        ],
      },
      { name: `${tag}-owned`, repo: {}, threads: [] },
      { name: `${tag}-loose`, repo: {}, threads: [] },
      { name: `${tag}-grouped`, repo: {}, threads: [] },
    ],
  });
  const [leadProject, ownedProject, looseProject, groupedProject] = seed.projects;
  const [ops, lead, seeded] = leadProject.threadIds;

  // The lead spawns one worker into each worker project; each holds its
  // first turn on a gate so it stays open on the lead's row.
  const spawn = (title: string, project: string, extra: Record<string, unknown> = {}) => ({
    call: { tool: 'thread_spawn', args: { prompt: `Do ${title}.`, title, project_id: project, ...extra } },
  });
  await setScenario(harness, leadProject.path, threadToolsScenario({
    name: `${tag}-lead`, provider: 'claude',
    turns: [{
      steps: [
        spawn('Owned worker', ownedProject.projectId),
        spawn('Standalone worker', looseProject.projectId, { placement: 'standalone' }),
        spawn('Grouped worker', groupedProject.projectId, { group: 'Sweep' }),
      ],
      text: 'Spawned three.',
    }],
    afterTurns: 'silent',
  }));
  for (const project of [ownedProject, looseProject, groupedProject]) {
    await setScenario(harness, project.path, threadToolsScenario({
      name: `${tag}-worker`, provider: 'claude',
      turns: [{ steps: [{ gate: 'worker-hold' }], text: 'Done.' }],
      afterTurns: 'silent',
    }));
  }

  await page.emulateMedia({ colorScheme: 'dark' });
  await harness.open(page);
  await awaitSidebarSettled(page);

  // ---- An assistant is one row, in the Assistants section only.
  await expect(assistantRow(page, ops)).toHaveAttribute('data-sidebar-agent', '');
  await expect(rowIn(page, ops)).toHaveCount(1);
  await expect(rowIn(projectsSection(page), ops)).toHaveCount(0);
  // An owned seeded worker has no row; its spawner has one.
  await expect(projectRow(page, lead)).toBeVisible();
  await expect(rowIn(page, seeded)).toHaveCount(0);

  // ---- The live spawns.
  await harness.rpc('StartSession', lead);
  const leadTurn = awaitTurnCompleted(harness, lead, 120_000);
  await harness.rpc('SendMessage', lead, 'spawn three workers', null);
  const spawned: string[] = [];
  for (let i = 0; i < 3; i++) {
    const answer = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadProject.path, timeoutMs: 60_000 });
    expect(answer.isError, answer.text).toBe(false);
    spawned.push(answer.value!.thread_id);
  }
  await leadTurn;
  const [owned, loose, grouped] = spawned;
  const gates = new Map<string, string>();
  for (const [id, project] of [[owned, ownedProject], [loose, looseProject], [grouped, groupedProject]] as const) {
    gates.set(id, (await awaitGate(harness, 'worker-hold', project.path)).mockId);
  }
  await expect.poll(() => placementOf(harness, owned)).toBe('owned');
  await expect.poll(() => placementOf(harness, loose)).toBe('standalone');
  await expect.poll(() => placementOf(harness, grouped)).toBe('grouped');
  await expect(rowIn(page, owned)).toHaveCount(0);
  await expect(projectItem(page, looseProject.projectId).locator(`[data-sidebar-thread-id="${loose}"]`)).toBeVisible();
  const sweep = page.getByTestId('thread-group-row').filter({ hasText: 'Sweep' });
  await expect(sweep).toHaveCount(1);
  await expect(page.locator(`[data-group-member] [data-sidebar-thread-id="${grouped}"]`)).toHaveCount(1);

  // The spawner's row counts its three open workers, the owned one
  // included, and none waits on the user.
  const leadRow = projectRow(page, lead);
  await expect(leadRow.getByTestId('thread-row-workers')).toHaveText('3');
  await expect(leadRow.getByTestId('thread-row-workers')).toHaveAttribute('title', '3 open workers');
  await expect(leadRow.getByTestId('thread-row-workers-need-you')).toHaveCount(0);
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `${tag}-sidebar.png`, assistantsSection(page), projectItem(page, leadProject.projectId));

  // ---- Remove from Sidebar: a pane showing the worker keeps it.
  await press(projectRow(page, loose));
  await expect(shownThread(page)).toHaveAttribute('data-thread-id', loose);
  if (layout === 'compact') await page.getByTestId('compact-back').tap();
  const menu = await openRowMenu(page, layout, loose);
  await expect(menu.getByRole('menuitem', { name: 'Remove from Sidebar', exact: true })).toBeVisible();
  for (const gone of ['Make Standalone', 'Previous Threads…']) await expect(menu.getByRole('menuitem', { name: gone, exact: true })).toHaveCount(0);
  await capture(page, `${tag}-row-menu.png`, menu);
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menuitem')).toHaveCount(0);
  await chooseRowMenuItem(page, layout, loose, 'Remove from Sidebar');
  await expect.poll(() => placementOf(harness, loose)).toBe('owned');
  await expect(rowIn(page, loose)).toHaveCount(0);
  await expect(shownThread(page)).toHaveAttribute('data-thread-id', loose);
  // Owned again, it still counts on its spawner's row.
  await expect(leadRow.getByTestId('thread-row-workers')).toHaveText('3');

  // ---- The workers finish: the count goes.
  for (const [id, mockId] of gates) {
    const done = awaitTurnCompleted(harness, id);
    await advanceGate(harness, mockId, 'worker-hold');
    await done;
  }
  await expect(leadRow.getByTestId('thread-row-workers')).toHaveCount(0);

  // ---- Previous Threads: a handoff leaves the old thread there.
  const home = await assistantHome(harness, ops);
  await setScenario(harness, home, plainScenario({ name: `${tag}-ops`, provider: 'claude', texts: ['Picked up.'], afterTurns: 'repeatLast', echoUserMessage: true }));
  const handedOff = awaitMockInput(harness, { cwd: home, includes: `The user replaced your previous thread ${ops} with this one` });
  await harness.rpc('StartNextAssistantThread', ops);
  await handedOff;
  let next = '';
  await expect.poll(async () => {
    next = ((await threadRows(harness)) as unknown as PlacedRow[]).find((r) => r.title === 'Ops' && r.id !== ops && !r.archived)?.id ?? '';
    return next;
  }).not.toBe('');
  await expect(assistantRow(page, next)).toBeVisible();
  await expect(rowIn(page, ops)).toHaveCount(0);
  const dialog = await openPreviousThreads(page, layout, next);
  expect(await previousThreadIds(dialog)).toEqual([ops]);
  await expect(dialog.getByTestId('previous-threads-superseded')).toHaveText(/^Superseded /);
  await capture(page, `${tag}-previous-threads.png`, dialog);
  await press(dialog.getByTestId('previous-threads-item'));
  await expect(dialog).toHaveCount(0);
  await expect(shownThread(page)).toHaveAttribute('data-thread-id', ops);
}
