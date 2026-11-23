// A command a Codex agent starts runs in the agent's own thread and outlives
// the agent's turn. The tray lists it, the session counts it as running
// work, and its row's Stop names it by item id and ends it in the agent's
// thread, whose process table holds it. A V1 close_agent shuts the agent's
// thread down with its commands. The mock, like a Codex build that
// publishes no notLoaded for a closed agent, sends no frame for the command;
// the close item ends nothing by itself, so the command leaves the tray when
// the closed agent's terminal list answers that its thread is gone.
import { test, expect } from './fixtures.js';
import { seedAgentThread, startMock, waitForGate } from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';
import type { Page } from '@playwright/test';

const CHILD = 'child-builder';
const rpc = (method: string, params: object) => JSON.stringify({ jsonrpc: '2.0', method, params });
const root = (method: string, fields: object) => rpc(method, { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...fields });
const child = (method: string, fields: object) => rpc(method, { threadId: CHILD, turnId: 'child-turn-1', ...fields });
const emit = (...lines: string[]) => ({ emit: { delayBetweenMs: 40, lines } });
const gate = (name: string) => ({ waitSignal: { name } });

/** The agent's thread starts a PTY and polls it: a backgrounded command. */
const childCommand = [
  rpc('turn/started', { threadId: CHILD, turn: { id: 'child-turn-1' } }),
  child('item/started', { item: { id: 'child-shell', type: 'commandExecution', source: 'unifiedExecStartup', processId: '77', status: 'inProgress', command: 'tail -f build.log' } }),
  child('item/commandExecution/terminalInteraction', { itemId: 'child-shell', processId: '77', stdin: '' }),
  rpc('turn/completed', { threadId: CHILD, turn: { id: 'child-turn-1', status: 'completed' } }),
];
const finishRoot = () => rpc('turn/completed', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}', status: 'completed' } });

async function openThread(harness: HarnessApp, page: Page, tag: string, steps: object[]): Promise<{ threadId: string; mock: string }> {
  await harness.rpc('HarnessSetScenario', { scenario: { version: 1, name: `codex-agent-command-${tag}`, provider: 'codex', afterTurns: 'silent', turns: [{ steps }] } });
  const title = `Agent command ${tag}`;
  const threadId = await seedAgentThread(harness, `agent-command-${tag}`, title, 'codex');
  await harness.open(page);
  await page.getByText(title, { exact: true }).click();
  const mock = await startMock(harness, threadId);
  return { threadId, mock };
}

const commandRow = (page: Page) => page.locator('[data-testid="background-task-tray-row"][data-row-id="child-shell"]');

async function expectListed(harness: HarnessApp, page: Page, threadId: string): Promise<void> {
  await expect.poll(() => harness.rpc<number>('CountRunningBackgroundTasks', threadId)).toBe(1);
  await page.getByTestId('activity-rail-background-toggle').filter({ visible: true }).click();
  await expect(commandRow(page)).toBeVisible();
}

test('a Codex agent command is listed and its Stop ends it in the agent thread', async ({ harness, page }) => {
  const { threadId, mock } = await openThread(harness, page, 'stop', [
    emit(
      rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }),
      root('item/completed', { item: { id: 'call_spawn_builder', type: 'subAgentActivity', kind: 'started', agentThreadId: CHILD, agentPath: '/root/builder' } }),
      ...childCommand,
    ),
    gate('stopped'),
    emit(finishRoot()),
    gate('done'),
  ]);
  await harness.rpc('SendMessage', threadId, 'build in the background', null);
  await waitForGate(harness, 'stopped');

  // The agent's turn is over; its command still runs and is counted.
  await expectListed(harness, page, threadId);

  // The mock's terminal ledger is per thread, so only a terminate that
  // names the agent's thread kills the command. The kill's item/completed
  // lands while the root turn runs and writes the command's row under its
  // agent; a terminate sent to the root thread matches nothing.
  await commandRow(page).getByTestId('background-task-tray-row-stop').click();
  await expect(commandRow(page)).toHaveCount(0);
  await expect.poll(async () => {
    const items = await harness.rpc<{ id: string; parentId?: string; status: string }[]>('ListItems', threadId, true);
    const row = items.find((item) => item.id === 'child-shell');
    return row ? `${row.parentId}:${row.status === 'running' ? 'running' : 'ended'}` : 'no row';
  }).toBe('call_spawn_builder:ended');
  await expect.poll(() => harness.rpc<number>('CountRunningBackgroundTasks', threadId)).toBe(0);
  await expect(page.getByText('That background terminal had already exited.')).toHaveCount(0);

  const settled = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('HarnessMockCommand', mock, { type: 'advance', name: 'stopped' });
  await settled;
  await waitForGate(harness, 'done');
});

test('a V1 close_agent ends the closed agent command with the agent', async ({ harness, page }) => {
  const states = (status: string) => ({ [CHILD]: { status, message: null } });
  const spawn = (method: string, status: string) => root(method, { item: {
    id: 'call_spawn_closer', type: 'collabAgentToolCall', tool: 'spawnAgent', status, senderThreadId: '${THREAD_ID}',
    receiverThreadIds: [CHILD], prompt: 'build it', agentsStates: states('running'),
  } });
  const { threadId, mock } = await openThread(harness, page, 'close', [
    emit(
      rpc('turn/started', { threadId: '${THREAD_ID}', turn: { id: '${TURN_ID}' } }),
      spawn('item/started', 'inProgress'),
      spawn('item/completed', 'completed'),
      ...childCommand,
    ),
    gate('listed'),
    emit(
      root('item/started', { item: { id: 'call_close_closer', type: 'collabAgentToolCall', tool: 'closeAgent', status: 'inProgress', senderThreadId: '${THREAD_ID}', receiverThreadIds: [CHILD], agentsStates: {} } }),
      root('item/completed', { item: { id: 'call_close_closer', type: 'collabAgentToolCall', tool: 'closeAgent', status: 'completed', senderThreadId: '${THREAD_ID}', receiverThreadIds: [CHILD], agentsStates: states('completed') } }),
      finishRoot(),
    ),
    gate('done'),
  ]);
  await harness.rpc('SendMessage', threadId, 'build then close', null);
  await waitForGate(harness, 'listed');
  await expectListed(harness, page, threadId);

  const settled = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('HarnessMockCommand', mock, { type: 'advance', name: 'listed' });
  await settled;
  await waitForGate(harness, 'done');
  await expect(commandRow(page)).toHaveCount(0);
  await expect.poll(() => harness.rpc<number>('CountRunningBackgroundTasks', threadId)).toBe(0);
});
