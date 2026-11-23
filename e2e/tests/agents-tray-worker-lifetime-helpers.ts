// Shared body of agents-tray-worker-lifetime.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Owned workers"). A worker answers its
// spawn with thread_reply and keeps working in the same turn. Its row in
// the spawner's Agents list stays open while it runs: it reads running,
// offers Stop, is counted on the toggle and on the spawner's sidebar row,
// and stays out of Past, past the tray's linger and the backend's settled
// window. Once its turn ends it leaves the open rows for Past with its
// reply, and nothing counts it.
//
// Both windows are a millisecond on this backend, so every read after the
// reply is past them: the row is open because its worker runs, never
// because a window still holds it.
import type { Page } from '@playwright/test';

import { HARNESS_WORKER_TIMEOUT_MS, expect, test as base, type SeedResult } from './fixtures.js';
import { harnessTiming, launchHarness, type HarnessApp } from '../src/harness.js';
import {
  FOOTER_TOKEN_PATTERN,
  advanceGate,
  awaitGate,
  awaitToolAnswer,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { LEAD, agentsBody, openAgentRow, openAgents, openPast, pastRow } from './agents-tray-helpers.js';
import { projectRow, type Layout } from './sidebar-sections-helpers.js';

const WORKER = 'Deploy watcher';
const REPLY = 'The deploy started; I am watching it.';
// The tray's linger (AGENT_ROW_SETTLED_LINGER_MS) and the backend's settled
// window (threadAgentSettledWindow), both over before the next read.
const PAST_WINDOWS = harnessTiming({ 'agent-row-linger': '1ms', 'agent-settled-window': '1ms' });

/** The gate's fixtures over a worker backend whose windows are over at once. */
export const test = base.extend<object, { harnessWorker: HarnessApp }>({
  harnessWorker: [
    async ({}, use) => {
      const app = await launchHarness({ env: PAST_WINDOWS });
      await use(app);
      await app.close();
    },
    { scope: 'worker', timeout: HARNESS_WORKER_TIMEOUT_MS },
  ],
});

/** The thread list: under compact, back out of a shown thread. */
async function showList(page: Page, layout: Layout): Promise<void> {
  if (layout !== 'compact') return;
  const back = page.getByTestId('compact-back').filter({ visible: true });
  if (await back.count()) await back.tap();
}

export async function runWorkerLifetime(harness: HarnessApp, page: Page, provider: ProviderName, layout: Layout): Promise<void> {
  const tag = `agents-lifetime-${provider}-${layout}`;
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, provider }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [leadP, workerP] = seed.projects;
  const lead = leadP.threadIds[0];
  await setScenario(harness, leadP.path, threadToolsScenario({
    name: `${tag}-lead`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Rerun the deploy, tell me when it starts, then watch it.', title: WORKER, project_id: workerP.projectId, provider, notify: true } } }], text: 'Started it.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: `${tag}-worker`, provider, echoUserMessage: true,
    turns: [
      { steps: [
        { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
        { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: REPLY } } },
        // Still in the same turn: watching the deploy.
        { gate: 'worker-hold' },
      ], text: 'Watched the deploy to the end.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'rerun the deploy', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const { token, thread_id: worker } = spawn.value!;
  const reply = await awaitToolAnswer(harness, { tool: 'thread_reply', cwd: workerP.path, timeoutMs: 60_000 });
  expect(reply.isError, reply.text).toBe(false);
  const hold = await awaitGate(harness, 'worker-hold', workerP.path);
  const workerRunning = async () => Boolean((await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', worker)).activeTurn);
  // The turn opens when the backend handles the turn start it reads from
  // the provider's output, which can trail the provider's tool call and
  // gate, both made out of band.
  await expect.poll(workerRunning, { message: 'the worker is still in its turn', timeout: 10_000 }).toBe(true);

  await harness.open(page);
  const leadRow = projectRow(page, lead);
  const workers = leadRow.getByTestId('thread-row-workers');
  const row = openAgentRow(page, token);
  const count = page.getByTestId('activity-rail-agents-count').filter({ visible: true });

  // Replied and still running: the spawner's sidebar row counts the worker.
  await expect(workers).toHaveText('1', { timeout: 30_000 });
  expect(await harness.rpc<{ open: number }>('GetThreadWorkerSummary', lead)).toMatchObject({ open: 1, needYou: 0 });
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `lifetime-${provider}-${layout}-sidebar-counts-running.png`, leadRow);

  // Its row is open: running, Stop, counted once, out of Past.
  const expectOpen = async () => {
    await openAgents(page, LEAD);
    await expect(row.getByTestId('agent-tray-row-state')).toHaveText('running', { timeout: 30_000 });
    await expect(row).toHaveAttribute('data-state', 'replied');
    await expect(row).not.toHaveAttribute('data-settled');
    await expect(row.getByTestId('agent-tray-row-stop')).toBeVisible();
    await expect(count).toHaveText('1');
    await expect(agentsBody(page).getByTestId('activity-rail-agents-running-label')).toHaveText('1 running');
    await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveCount(0);
  };
  await expectOpen();
  await capture(page, `lifetime-${provider}-${layout}-replied-while-running.png`, agentsBody(page));

  // Read again from a fresh page, past the linger and the settled window,
  // it is still open while the worker runs.
  expect(await workerRunning(), 'the worker is still in its turn').toBe(true);
  await page.reload();
  await showList(page, layout);
  await expectOpen();
  await capture(page, `lifetime-${provider}-${layout}-open-after-windows.png`, agentsBody(page));

  // Its turn ends: the row leaves for Past with its reply, and nothing counts it.
  await advanceGate(harness, hold.mockId, 'worker-hold');
  await expect.poll(workerRunning, { timeout: 30_000 }).toBe(false);
  await expect(row).toHaveCount(0, { timeout: 30_000 });
  await expect(count).toHaveCount(0);
  await expect(agentsBody(page).getByTestId('activity-rail-agents-past-count')).toHaveText('1');
  await openPast(page);
  const past = pastRow(page, token);
  await expect(past.getByTestId('agent-tray-row-state')).toHaveText('replied');
  await expect(past.getByTestId('agent-tray-row-stop')).toHaveCount(0);
  await capture(page, `lifetime-${provider}-${layout}-past-after-rest.png`, agentsBody(page));
  expect(await harness.rpc<{ open: number }>('GetThreadWorkerSummary', lead)).toMatchObject({ open: 0, needYou: 0 });
  await showList(page, layout);
  await expect(workers).toHaveCount(0);
}
