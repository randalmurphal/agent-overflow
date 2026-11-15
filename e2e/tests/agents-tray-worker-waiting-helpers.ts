// Shared body of agents-tray-worker-waiting.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Owned workers"). A lead spawns two
// workers whose turns end while they wait on something they started: one
// leaves a background shell running, one arms a loop. Neither is at rest,
// so each spawn's row stays open, reads waiting with what it waits on as
// its line, offers Stop, is counted, needs nobody and stays out of Past.
// runAncestorWaiting: a worker whose own work is done but whose own worker
// waits on a loop is not at rest either, so its row stays open the same
// way, waiting with no line of its own, its worker nested under it.
import type { Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { awaitToolAnswer, setScenario, threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { LEAD, agentsBody, backgroundShellSteps, ledgerRequests, openAgentRow, openAgents } from './agents-tray-helpers.js';

const SHELL_WORKER = 'Build watcher';
const LOOP_WORKER = 'Board ticker';
const LOOP_LABEL = 'board';

export async function runWorkerWaiting(harness: HarnessApp, page: Page, provider: ProviderName, layout: 'desktop' | 'compact'): Promise<void> {
  const tag = `agents-waiting-${provider}-${layout}`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
      { name: `${tag}-shell`, repo: {}, threads: [] },
      { name: `${tag}-loop`, repo: {}, threads: [] },
    ],
  });
  const [leadP, shellP, loopP] = seed.projects;
  const lead = leadP.threadIds[0];
  await setScenario(harness, leadP.path, threadToolsScenario({
    name: `${tag}-lead`, provider, echoUserMessage: true,
    turns: [
      { steps: [
        { call: { tool: 'thread_spawn', args: { prompt: 'Watch the build.', title: SHELL_WORKER, project_id: shellP.projectId, provider } } },
        { call: { tool: 'thread_spawn', args: { prompt: 'Keep an eye on the board.', title: LOOP_WORKER, project_id: loopP.projectId, provider } } },
      ], text: 'Started both.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, shellP.path, threadToolsScenario({
    name: `${tag}-shell`, provider, echoUserMessage: true,
    turns: [{ steps: backgroundShellSteps(provider), text: 'Tailing the build log.' }, { text: 'Noted.' }],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, loopP.path, threadToolsScenario({
    name: `${tag}-loop`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_loop', args: { label: LOOP_LABEL, note: 'Look at the board.', every_seconds: 3600 } } }], text: 'Looping on the board.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'watch the build and the board', null);
  const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd: loopP.path, timeoutMs: 60_000 });
  expect(loop.isError, loop.text).toBe(false);
  let spawns: ReturnType<typeof ledgerRequests> = [];
  await expect.poll(() => (spawns = ledgerRequests(harness, lead).filter((r) => r.kind === 'spawn')).length, { timeout: 30_000 }).toBe(2);
  const [shellSpawn, loopSpawn] = spawns;
  // Both workers' turns end; what they started is still there.
  const turnEnded = async (thread: string) => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', thread)).activeTurn;
  await expect.poll(() => turnEnded(shellSpawn.targetThreadId), { timeout: 30_000 }).toBe(true);
  await expect.poll(() => turnEnded(loopSpawn.targetThreadId), { timeout: 30_000 }).toBe(true);
  await expect.poll(() => harness.rpc<unknown[]>('ListLiveBackgroundTasks', shellSpawn.targetThreadId).then((tasks) => tasks.length), { timeout: 30_000 }).toBeGreaterThan(0);

  await harness.open(page);
  await openAgents(page, LEAD);
  const count = page.getByTestId('activity-rail-agents-count').filter({ visible: true });
  for (const [spawn, line] of [[shellSpawn, /tail -f build\.log|tail the build log/], [loopSpawn, `Loop · ${LOOP_LABEL}`]] as const) {
    const row = openAgentRow(page, spawn.token);
    await expect(row.getByTestId('agent-tray-row-state')).toHaveText('waiting', { timeout: 30_000 });
    await expect(row).not.toHaveAttribute('data-settled');
    await expect(row.getByTestId('agent-tray-row-stop')).toBeVisible();
    await expect(row.getByTestId('agent-tray-row-activity')).toHaveText(line);
  }
  await expect(count).toHaveText('2');
  await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveCount(0);
  expect(await harness.rpc<{ open: number }>('GetThreadWorkerSummary', lead)).toMatchObject({ open: 2, needYou: 0 });
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `waiting-${provider}-${layout}-shell-and-loop.png`, agentsBody(page));
}

const ANCESTOR_WORKER = 'Board lead';

export async function runAncestorWaiting(harness: HarnessApp, page: Page, provider: ProviderName, layout: 'desktop' | 'compact'): Promise<void> {
  const tag = `agents-ancestor-${provider}-${layout}`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
      { name: `${tag}-child`, repo: {}, threads: [] },
      { name: `${tag}-grand`, repo: {}, threads: [] },
    ],
  });
  const [leadP, childP, grandP] = seed.projects;
  const lead = leadP.threadIds[0];
  await setScenario(harness, leadP.path, threadToolsScenario({
    name: `${tag}-lead`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Run the board.', title: ANCESTOR_WORKER, project_id: childP.projectId, provider } } }], text: 'Started it.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, childP.path, threadToolsScenario({
    name: `${tag}-child`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Keep an eye on the board.', title: LOOP_WORKER, project_id: grandP.projectId, provider } } }], text: 'Handed it on.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, grandP.path, threadToolsScenario({
    name: `${tag}-grand`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_loop', args: { label: LOOP_LABEL, note: 'Look at the board.', every_seconds: 3600 } } }], text: 'Looping on the board.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'run the board', null);
  const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd: grandP.path, timeoutMs: 60_000 });
  expect(loop.isError, loop.text).toBe(false);
  let spawn: ReturnType<typeof ledgerRequests>[number] | undefined;
  await expect.poll(() => (spawn = ledgerRequests(harness, lead).find((r) => r.kind === 'spawn'))?.targetThreadId ?? '', { timeout: 30_000 }).not.toBe('');
  let nested: ReturnType<typeof ledgerRequests>[number] | undefined;
  await expect.poll(() => (nested = ledgerRequests(harness, spawn!.targetThreadId).find((r) => r.kind === 'spawn'))?.targetThreadId ?? '', { timeout: 30_000 }).not.toBe('');
  // Every turn ends and both spawns settle; only the loop is left.
  const turnEnded = async (thread: string) => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', thread)).activeTurn;
  await expect.poll(() => ledgerRequests(harness, lead).concat(ledgerRequests(harness, spawn!.targetThreadId))
    .filter((r) => r.kind === 'spawn' && ['replied', 'finished'].includes(r.state)).length, { timeout: 60_000 }).toBe(2);
  for (const thread of [lead, spawn!.targetThreadId, nested!.targetThreadId]) {
    await expect.poll(() => turnEnded(thread), { timeout: 30_000 }).toBe(true);
  }

  await harness.open(page);
  await openAgents(page, LEAD);
  const top = openAgentRow(page, spawn!.token);
  const below = openAgentRow(page, nested!.token);
  await expect(top.getByTestId('agent-tray-row-state')).toHaveText('waiting', { timeout: 30_000 });
  await expect(top).not.toHaveAttribute('data-settled');
  await expect(top.getByTestId('agent-tray-row-stop')).toBeVisible();
  await expect(top.getByTestId('agent-tray-row-activity'), 'a worker waiting on its own worker has no line of its own').toHaveCount(0);
  await expect(below).toHaveAttribute('data-depth', '1');
  await expect(below.getByTestId('agent-tray-row-state')).toHaveText('waiting');
  await expect(below.getByTestId('agent-tray-row-activity')).toHaveText(`Loop · ${LOOP_LABEL}`);
  await expect(page.getByTestId('activity-rail-agents-count').filter({ visible: true })).toHaveText('2');
  await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveCount(0);
  expect(await harness.rpc<{ open: number }>('GetThreadWorkerSummary', lead)).toMatchObject({ open: 2, needYou: 0 });
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `waiting-${provider}-${layout}-ancestor-held-by-loop.png`, agentsBody(page));
}
