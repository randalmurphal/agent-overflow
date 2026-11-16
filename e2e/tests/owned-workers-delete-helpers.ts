// Shared body of owned-workers-delete.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Owned workers"). A thread owns a running
// worker and an archived worker that owns one of its own, and has placed a
// standalone spawn in the sidebar. Deleting the thread from its sidebar row
// says in the confirmation that its three workers go with it; the running
// worker's session ends, all three are gone, the standalone spawn stays and
// runs on, and nothing starts a session for a deleted thread afterwards.
// Screenshots are dark mode.
import type { Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp, HarnessMockEventData } from '../src/harness.js';
import {
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadRows,
  threadToolsScenario,
} from './thread-tools-helpers.js';
import { awaitSidebarSettled, capture } from './wake-rows-helpers.js';
import { chooseRowMenuItem, projectItem, projectRow, rowIn, type Layout } from './sidebar-sections-helpers.js';

const turn = (text: string) => [{ userText: text, items: [{ kind: 'assistant_text', summary: 'ready' }] }];

export async function runOwnedWorkersDelete(harness: HarnessApp, page: Page, layout: Layout): Promise<void> {
  const tag = layout === 'desktop' ? 'delete-owned' : 'compact-delete-owned';
  await harness.rpc('UpdateSettings', { confirmDelete: true });
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `${tag}-lead`, repo: {},
        threads: [
          { title: 'Release lead', turns: turn('hello') },
          { title: 'Archived worker', spawnedBy: 'Release lead', archived: true, turns: turn('w') },
          { title: 'Nested worker', spawnedBy: 'Archived worker', turns: turn('n') },
        ],
      },
      { name: `${tag}-owned`, repo: {}, threads: [] },
      { name: `${tag}-loose`, repo: {}, threads: [] },
    ],
  });
  const [leadProject, ownedProject, looseProject] = seed.projects;
  const [lead, archived, nested] = leadProject.threadIds;

  const spawn = (title: string, project: string, extra: Record<string, unknown> = {}) => ({
    call: { tool: 'thread_spawn', args: { prompt: `Do ${title}.`, title, project_id: project, ...extra } },
  });
  await setScenario(harness, leadProject.path, threadToolsScenario({
    name: `${tag}-lead`, provider: 'claude',
    turns: [{
      steps: [
        spawn('Running worker', ownedProject.projectId),
        spawn('Standalone worker', looseProject.projectId, { placement: 'standalone' }),
      ],
      text: 'Spawned two.',
    }],
    afterTurns: 'silent',
  }));
  for (const project of [ownedProject, looseProject]) {
    await setScenario(harness, project.path, threadToolsScenario({
      name: `${tag}-worker`, provider: 'claude',
      turns: [{ steps: [{ gate: 'worker-hold' }], text: 'Done.' }],
      afterTurns: 'silent',
    }));
  }

  await page.emulateMedia({ colorScheme: 'dark' });
  await harness.open(page);
  await awaitSidebarSettled(page);

  await harness.rpc('StartSession', lead);
  const leadTurn = awaitTurnCompleted(harness, lead, 120_000);
  await harness.rpc('SendMessage', lead, 'spawn two workers', null);
  const spawned: string[] = [];
  for (let i = 0; i < 2; i++) {
    const answer = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadProject.path, timeoutMs: 60_000 });
    expect(answer.isError, answer.text).toBe(false);
    spawned.push(answer.value!.thread_id);
  }
  await leadTurn;
  const [running, loose] = spawned;
  const runningGate = (await awaitGate(harness, 'worker-hold', ownedProject.path)).mockId;
  const looseGate = (await awaitGate(harness, 'worker-hold', looseProject.path)).mockId;
  await expect(projectItem(page, looseProject.projectId).locator(`[data-sidebar-thread-id="${loose}"]`)).toBeVisible();
  await expect(rowIn(page, running)).toHaveCount(0);

  // A session that starts after the delete registers a new mock in its
  // thread's workspace; the lead's and the running worker's must not.
  const registered = (cwd: string) => harness.countEvents<HarnessMockEventData>(
    'harness:mock', (ev) => ev.report.kind === 'registered' && ev.cwd === cwd,
  );
  const exited = () => harness.countEvents<HarnessMockEventData>(
    'harness:mock', (ev) => ev.mockId === runningGate && ev.report.kind === 'exiting',
  );
  const before = { lead: registered(leadProject.path), worker: registered(ownedProject.path) };
  expect(exited()).toBe(0);

  // ---- The confirmation counts the three workers; the standalone spawn
  // has a row of its own and is not one of them.
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await chooseRowMenuItem(page, layout, lead, 'Delete');
  const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
  await expect(dialog).toContainText(
    'This will permanently delete this thread and all its messages. This action cannot be undone. Its 3 workers are deleted with it.',
  );
  await capture(page, `${tag}-confirm.png`, dialog);
  const confirm = dialog.getByRole('button', { name: 'Delete', exact: true });
  if (layout === 'desktop') await confirm.click();
  else await confirm.tap();

  await expect(rowIn(page, lead)).toHaveCount(0);
  await expect.poll(exited, { timeout: 15_000 }).toBe(1);
  for (const id of [lead, running, archived, nested]) {
    await expect(harness.rpc('GetThread', id), `${id} outlived its owner`).rejects.toThrow();
  }
  await expect(page.getByTestId('toast').filter({ hasText: 'was not deleted' })).toHaveCount(0);

  // ---- The placed spawn stays and finishes its turn.
  await expect(projectRow(page, loose)).toBeVisible();
  const looseDone = awaitTurnCompleted(harness, loose);
  await advanceGate(harness, looseGate, 'worker-hold');
  await looseDone;
  expect((await threadRows(harness)).some((row) => row.id === loose)).toBe(true);

  // ---- Nothing starts a session for a deleted thread.
  await page.waitForTimeout(2_000);
  expect(registered(leadProject.path)).toBe(before.lead);
  expect(registered(ownedProject.path)).toBe(before.worker);
  expect(exited()).toBe(1);
}
