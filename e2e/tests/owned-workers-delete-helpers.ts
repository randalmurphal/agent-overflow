// Shared body of owned-workers-delete.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Owned workers"). A thread owns a running
// worker on a worktree of its own and an archived worker that owns one of
// its own, and has placed a standalone spawn in the sidebar. Deleting the
// thread from its sidebar row says in the confirmation that its three
// workers go with it, and nothing of the worktree. With unsaved work on the
// running worker's worktree the delete goes through: the running worker's
// session ends, all three are gone, the worktree stays with its unsaved
// work and is still listed for the worktree actions, the standalone spawn
// stays and runs on, and nothing starts a session for a deleted thread
// afterwards. Screenshots are dark mode.
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

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
import { listWorktrees, samePath } from './worktree-removal-helpers.js';
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
        // On a worktree of its own, made from the local head: the harness
        // project has no origin to fetch from.
        spawn('Running worker', ownedProject.projectId, { worktree: `${tag}-running`, base_local: true }),
        spawn('Standalone worker', looseProject.projectId, { placement: 'standalone' }),
      ],
      text: 'Spawned two.',
    }],
    afterTurns: 'silent',
  }));
  // The running worker's worktree is named only once it is made, so its
  // script is the one for every workspace without its own.
  for (const path of ['', looseProject.path]) {
    await setScenario(harness, path, threadToolsScenario({
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
  const runningTree = (await harness.rpc<{ worktreePath?: string }>('GetThread', running)).worktreePath ?? '';
  expect(runningTree, 'the running worker is on a worktree').not.toBe('');
  expect(existsSync(runningTree)).toBe(true);
  const runningGate = (await awaitGate(harness, 'worker-hold', runningTree)).mockId;
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
  const before = { lead: registered(leadProject.path), worker: registered(runningTree) };
  expect(exited()).toBe(0);

  // ---- The confirmation counts the three workers; the standalone spawn
  // has a row of its own and is not one of them.
  const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
  const confirmDelete = async (shot?: string) => {
    if (layout === 'desktop') await page.mouse.move(800, 400);
    await chooseRowMenuItem(page, layout, lead, 'Delete');
    await expect(dialog).toContainText(
      'This will permanently delete this thread and all its messages. This action cannot be undone. Its 3 workers are deleted with it.',
    );
    if (shot) await capture(page, shot, dialog);
    const confirm = dialog.getByRole('button', { name: 'Delete', exact: true });
    if (layout === 'desktop') await confirm.click();
    else await confirm.tap();
  };

  // ---- Unsaved work on the running worker's worktree changes nothing: the
  // delete goes through, and the worktree stays with the work.
  const unsaved = join(runningTree, 'unsaved.txt');
  writeFileSync(unsaved, 'work in progress\n');
  await confirmDelete(`${tag}-confirm.png`);

  await expect(rowIn(page, lead)).toHaveCount(0);
  await expect.poll(exited, { timeout: 15_000 }).toBe(1);
  for (const id of [lead, running, archived, nested]) {
    await expect(harness.rpc('GetThread', id), `${id} outlived its owner`).rejects.toThrow();
  }
  await expect(page.getByTestId('toast').filter({ hasText: 'was not deleted' })).toHaveCount(0);
  expect(readFileSync(unsaved, 'utf8')).toBe('work in progress\n');
  // Still a worktree of its project, listed where the workspace picker
  // reads it and offers to remove it.
  const listed = await listWorktrees(harness, ownedProject.projectId, ownedProject.path);
  const stillListed = await Promise.all(listed.map((item) => samePath(item.path, runningTree)));
  expect(stillListed.some(Boolean), `the running worker's worktree is no longer listed: ${JSON.stringify(listed)}`).toBe(true);

  // ---- The placed spawn stays and finishes its turn.
  await expect(projectRow(page, loose)).toBeVisible();
  const looseDone = awaitTurnCompleted(harness, loose);
  await advanceGate(harness, looseGate, 'worker-hold');
  await looseDone;
  expect((await threadRows(harness)).some((row) => row.id === loose)).toBe(true);

  // ---- Nothing starts a session for a deleted thread. Every start reads
  // its thread's row before it registers a session (startSessionNow), so
  // once the rows are gone, as asserted above, no later start can register
  // one. A start that races the delete waits for its decision and starts
  // nothing once it commits (internal/app
  // TestACommitEndsEveryStartWaitingForIt). The counts cover any start up
  // to the placed spawn's finished turn.
  expect(registered(leadProject.path)).toBe(before.lead);
  expect(registered(runningTree)).toBe(before.worker);
  expect(exited()).toBe(1);
}
