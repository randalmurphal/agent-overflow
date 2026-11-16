// Shared body of agents-tray-worker-waiting.spec.ts and its compact twin
// (docs/specs/standing-agents.md, "Owned workers"). A lead spawns two
// workers whose turns end while they wait on something they started: one
// leaves a background shell running, one arms a loop. Neither is at rest,
// so each spawn's row stays open, reads waiting with what it waits on as
// its line, offers Stop, is counted, needs nobody and stays out of Past.
// runAncestorWaiting: a worker whose own work is done but whose own worker
// waits on a loop is not at rest either, so its row stays open the same
// way, waiting with no line of its own, its worker nested under it, and
// neither spawn is answered while the loop is armed.
// runHeldWorkerFacts: a running worker's row shows its tool calls, its
// tokens, its runtime and its latest tool call, as a subagent's row does;
// its turn ends with its background shell running, and its spawner is not
// told it finished until the shell ends, then with its last message.
import type { Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  advanceGate,
  awaitGate,
  awaitMockInput,
  awaitToolAnswer,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type TurnStep,
} from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { taskNotificationLine, taskUpdatedLine, toolResultLine } from './agent-visibility-helpers.js';
import {
  LEAD,
  agentsBody,
  backgroundShellSteps,
  ledgerRequests,
  ledgerRow,
  openAgentRow,
  openAgents,
  openPast,
  pastRow,
} from './agents-tray-helpers.js';

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
  // Every turn ends; only the loop is left, so neither spawn is answered:
  // each worker is not at rest while it, or one below it, waits.
  const turnEnded = async (thread: string) => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', thread)).activeTurn;
  for (const thread of [lead, spawn!.targetThreadId, nested!.targetThreadId]) {
    await expect.poll(() => turnEnded(thread), { timeout: 30_000 }).toBe(true);
  }
  for (const request of [spawn!, nested!]) expect(ledgerRow(harness, request.token)!.state, request.token).toBe('running');

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

const FACTS_WORKER = 'Build runner';
const FACTS_WAITING = 'Tailing the build log.';
const FACTS_ANSWER = 'The build is green.';
const FACTS_SHELL = /tail -f build\.log|tail the build log/;

/**
 * The worker's latest tool call when its gate holds it. A Claude background
 * Bash is a tool call; a Codex terminal is the tray's alone, so the Codex
 * worker runs a second command before it.
 */
function factsLatestCall(provider: ProviderName): RegExp {
  return provider === 'claude' ? FACTS_SHELL : /make check/;
}

/**
 * The worker's first turn before its gate: a read that spends 2,000 tokens
 * (1,200 fresh input, 500 cache writes, 300 output; the 40,000 read from
 * the cache are no spend, as a subagent's count has it), then a background
 * shell: two tool calls in all.
 */
function factsWorkerSteps(provider: ProviderName): TurnStep[] {
  if (provider === 'claude') {
    return [{
      emitLines: [
        JSON.stringify({ type: 'assistant', message: {
          id: 'msg-read', role: 'assistant', model: 'claude-opus-4-7',
          content: [{ type: 'tool_use', id: 'tu-read', name: 'Bash', input: { command: 'cat build.log' } }],
          usage: { input_tokens: 1200, output_tokens: 300, cache_creation_input_tokens: 500, cache_read_input_tokens: 40000 },
        } }),
        toolResultLine('tu-read', 'build ok'),
      ],
    }, ...backgroundShellSteps(provider)];
  }
  const frame = (method: string, fields: object) => JSON.stringify({
    jsonrpc: '2.0', method, params: { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...fields },
  });
  const usage = { totalTokens: 42000, inputTokens: 41200, cachedInputTokens: 40000, cacheWriteInputTokens: 500, outputTokens: 300 };
  return [{
    emitLines: [
      frame('item/started', { item: { id: 'cmd-read', type: 'commandExecution', status: 'inProgress', command: 'cat build.log' } }),
      frame('item/completed', { item: { id: 'cmd-read', type: 'commandExecution', status: 'completed', command: 'cat build.log', exitCode: 0, aggregatedOutput: 'build ok\n' } }),
      frame('item/started', { item: { id: 'cmd-check', type: 'commandExecution', status: 'inProgress', command: 'make check' } }),
      frame('item/completed', { item: { id: 'cmd-check', type: 'commandExecution', status: 'completed', command: 'make check', exitCode: 0, aggregatedOutput: 'ok\n' } }),
      JSON.stringify({ jsonrpc: '2.0', method: 'thread/tokenUsage/updated', params: {
        threadId: '${THREAD_ID}', tokenUsage: { total: usage, last: usage, modelContextWindow: 272000 },
      } }),
    ],
  }, ...backgroundShellSteps(provider)];
}

/** The background shell of factsWorkerSteps ending, as each provider reports it. */
function factsShellDone(provider: ProviderName): TurnStep[] {
  if (provider === 'claude') {
    return [{ emitLines: [taskUpdatedLine('task-shell', { status: 'completed' }), taskNotificationLine('task-shell', 'tu-shell', 'The build log ended.')] }];
  }
  return [{
    emitLines: [JSON.stringify({ jsonrpc: '2.0', method: 'item/completed', params: {
      threadId: '${THREAD_ID}', turnId: '${TURN_ID}',
      item: { id: 'shell-1', type: 'commandExecution', source: 'unifiedExecStartup', processId: '77', status: 'completed', command: 'tail -f build.log', exitCode: 0, aggregatedOutput: 'built\n' },
    } })],
  }];
}

export async function runHeldWorkerFacts(harness: HarnessApp, page: Page, provider: ProviderName, layout: 'desktop' | 'compact'): Promise<void> {
  const tag = `agents-facts-${provider}-${layout}`;
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
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Run the build.', title: FACTS_WORKER, project_id: workerP.projectId, provider, notify: true } } }], text: 'Started it.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: `${tag}-worker`, provider, echoUserMessage: true,
    turns: [
      { steps: [...factsWorkerSteps(provider), { gate: 'facts-hold' }], text: FACTS_WAITING },
      { steps: factsShellDone(provider), text: FACTS_ANSWER },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'run the build', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const { token, thread_id: worker } = spawn.value!;
  const hold = await awaitGate(harness, 'facts-hold', workerP.path);

  // Running: its counts, its runtime and its latest tool call.
  await harness.open(page);
  await openAgents(page, LEAD);
  const row = openAgentRow(page, token);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('running', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-elapsed')).toHaveText(/^\d+[smh]/);
  await expect(row.getByTestId('agent-tray-row-tools')).toHaveText('2 tools', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-tokens')).toHaveText('2.0k tokens');
  await expect(row.getByTestId('agent-tray-row-activity')).toHaveText(factsLatestCall(provider), { timeout: 30_000 });
  const listed = (await harness.rpc<{ rows: { token: string; toolCalls?: number; tokens?: number }[] }>('ListThreadAgentRows', lead))
    .rows.find((r) => r.token === token);
  expect(listed).toMatchObject({ toolCalls: 2, tokens: 2000 });
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `facts-${provider}-${layout}-running.png`, agentsBody(page));

  // Its turn ends with the shell running: its spawn is not answered, and
  // its row waits on the shell with its counts kept.
  await advanceGate(harness, hold.mockId, 'facts-hold');
  await expect.poll(async () => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', worker)).activeTurn, { timeout: 30_000 }).toBe(true);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('waiting', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-activity')).toHaveText(FACTS_SHELL);
  await expect(row.getByTestId('agent-tray-row-tools')).toHaveText('2 tools');
  await expect(row.getByTestId('agent-tray-row-tokens')).toHaveText('2.0k tokens');
  expect(ledgerRow(harness, token)!.state, 'a worker with its shell running has not finished').toBe('running');
  await capture(page, `facts-${provider}-${layout}-waiting.png`, agentsBody(page));

  // The shell ends in its next turn: the spawn settles finished with that
  // turn's message, and the spawner is told.
  const told = awaitMockInput(harness, { cwd: leadP.path, includes: [token, FACTS_ANSWER] });
  await harness.rpc('SendMessage', worker, 'is the build done?', null);
  await told;
  expect(ledgerRow(harness, token)).toMatchObject({ state: 'finished', answer: FACTS_ANSWER });
  await openPast(page);
  const past = pastRow(page, token);
  // A Codex terminal is written as a tool call when it ends.
  await expect(past.getByTestId('agent-tray-row-tools')).toHaveText(provider === 'claude' ? '2 tools' : '3 tools');
  await expect(past.getByTestId('agent-tray-row-tokens')).toHaveText('2.0k tokens');
  await expect(past.getByTestId('agent-tray-row-activity')).toHaveCount(0);
}
