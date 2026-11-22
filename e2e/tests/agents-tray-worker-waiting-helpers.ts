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
// runHeldWorkerFacts: a running worker's row, mounted before it does
// anything, shows its tool calls, its tokens, its runtime and its latest
// tool call as a subagent's row does, framed again when only its spend
// moves and when a new call reads as the last one did; its turn ends with
// its background shell running, its runtime stands still, and its spawner
// is not told it finished until the shell ends, then with its last
// message; under Past its runtime is its turns' time.
// runReplyThenRest and runHeldThenError: see each.
import type { Locator, Page } from '@playwright/test';

import { expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  FOOTER_TOKEN_PATTERN,
  advanceGate,
  awaitGate,
  awaitMockInput,
  awaitToolAnswer,
  releaseAfterBackgrounded,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type TurnStep,
} from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';
import { taskNotificationLine, taskUpdatedLine, toolResultLine, toolUseLine } from './agent-visibility-helpers.js';
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
const FACTS_READ = /cat build\.log/;
/** How long a test leaves a worker between turns to see its runtime stand still. */
const IDLE_MS = 2_500;

/** A row's runtime as AgentTrayRow writes it (formatElapsed). */
function ranFor(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `Ran ${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `Ran ${minutes}m ${seconds % 60}s`;
  return `Ran ${Math.floor(minutes / 60)}h ${minutes % 60}m ${seconds % 60}s`;
}

function codexFrame(method: string, fields: object): string {
  return JSON.stringify({ jsonrpc: '2.0', method, params: { threadId: '${THREAD_ID}', turnId: '${TURN_ID}', ...fields } });
}

/** Codex's cumulative usage line; the spend is input past the cache, cache writes and output. */
function codexUsageLine(inputTokens: number, outputTokens: number): string {
  const usage = { totalTokens: inputTokens + outputTokens, inputTokens, cachedInputTokens: 40000, cacheWriteInputTokens: 500, outputTokens };
  return JSON.stringify({ jsonrpc: '2.0', method: 'thread/tokenUsage/updated', params: {
    threadId: '${THREAD_ID}', tokenUsage: { total: usage, last: usage, modelContextWindow: 272000 },
  } });
}

/** A command the worker runs to its end: one top-level tool call. */
function commandCall(provider: ProviderName, id: string, command: string): string[] {
  if (provider === 'claude') return [toolUseLine(`msg-${id}`, `tu-${id}`, 'Bash', { command }), toolResultLine(`tu-${id}`, 'ok')];
  return [
    codexFrame('item/started', { item: { id, type: 'commandExecution', status: 'inProgress', command } }),
    codexFrame('item/completed', { item: { id, type: 'commandExecution', status: 'completed', command, exitCode: 0, aggregatedOutput: 'ok\n' } }),
  ];
}

/**
 * The worker's first read: one tool call that spends 2,000 tokens (1,200
 * fresh input, 500 cache writes, 300 output; the 40,000 read from the cache
 * are no spend, as a subagent's count has it).
 */
function factsFirstRead(provider: ProviderName): TurnStep {
  if (provider === 'claude') {
    return {
      emitLines: [
        JSON.stringify({ type: 'assistant', message: {
          id: 'msg-read', role: 'assistant', model: 'claude-opus-4-7',
          content: [{ type: 'tool_use', id: 'tu-read', name: 'Bash', input: { command: 'cat build.log' } }],
          usage: { input_tokens: 1200, output_tokens: 300, cache_creation_input_tokens: 500, cache_read_input_tokens: 40000 },
        } }),
        toolResultLine('tu-read', 'build ok'),
      ],
    };
  }
  return { emitLines: [...commandCall(provider, 'cmd-read', 'cat build.log'), codexUsageLine(41200, 300)] };
}

/** 1,000 more tokens spent and nothing else: no tool call, no message. */
function factsSpendOnly(provider: ProviderName): TurnStep {
  if (provider === 'claude') {
    return {
      emitLines: [
        JSON.stringify({ type: 'stream_event', event: 'message_start', data: { type: 'message_start', message: {
          id: 'msg-think', role: 'assistant', model: 'claude-opus-4-7', usage: { input_tokens: 800, output_tokens: 200 },
        } } }),
        JSON.stringify({ type: 'stream_event', event: 'message_stop', data: { type: 'message_stop' } }),
      ],
    };
  }
  return { emitLines: [codexUsageLine(42000, 500)] };
}

/** The background shell of backgroundShellSteps ending, as each provider reports it. */
function factsShellDone(provider: ProviderName): TurnStep[] {
  if (provider === 'claude') {
    return [{ emitLines: [taskUpdatedLine('task-shell', { status: 'completed' }), taskNotificationLine('task-shell', 'tu-shell', 'The build log ended.')] }];
  }
  return [{
    emitLines: [codexFrame('item/completed', {
      item: { id: 'shell-1', type: 'commandExecution', source: 'unifiedExecStartup', processId: '77', status: 'completed', command: 'tail -f build.log', exitCode: 0, aggregatedOutput: 'built\n' },
    })],
  }];
}

interface ListedRow { token: string; toolCalls?: number; tokens?: number; runtimeMs?: number; turnOpen?: boolean }

async function listedRow(harness: HarnessApp, lead: string, token: string): Promise<ListedRow | undefined> {
  return (await harness.rpc<{ rows: ListedRow[] }>('ListThreadAgentRows', lead)).rows.find((r) => r.token === token);
}

async function listedPastRow(harness: HarnessApp, lead: string, token: string): Promise<ListedRow | undefined> {
  return (await harness.rpc<{ rows: ListedRow[] }>('ListThreadPastAgentRows', lead, { settledAt: 0, token: '' }, 8)).rows.find((r) => r.token === token);
}

/** The row's runtime moves on while its worker's turn runs. */
async function expectRuntimeRunning(row: Locator): Promise<void> {
  const elapsed = row.getByTestId('agent-tray-row-elapsed');
  await expect(elapsed).toHaveText(/^\d+s$/);
  const before = await elapsed.textContent();
  await expect(elapsed).not.toHaveText(before ?? '', { timeout: 5_000 });
}

/** The row's runtime stands still while its worker is between turns. */
async function expectRuntimeIdle(page: Page, row: Locator): Promise<void> {
  const elapsed = row.getByTestId('agent-tray-row-elapsed');
  const before = await elapsed.textContent();
  await page.waitForTimeout(IDLE_MS);
  await expect(elapsed).toHaveText(before ?? '');
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
      { steps: [
        { gate: 'facts-start' },
        factsFirstRead(provider),
        { gate: 'facts-read' },
        factsSpendOnly(provider),
        { gate: 'facts-spent' },
        // The same command again: its line reads as the last call's did.
        { emitLines: commandCall(provider, 'cmd-read-2', 'cat build.log') },
        { gate: 'facts-again' },
        ...backgroundShellSteps(provider),
        { gate: 'facts-hold' },
      ], text: FACTS_WAITING },
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
  const started = Date.now();
  const start = await awaitGate(harness, 'facts-start', workerP.path);

  // The rail is mounted before the worker does anything: every count below
  // reaches it as a frame of the open row.
  await harness.open(page);
  await openAgents(page, LEAD);
  const row = openAgentRow(page, token);
  const tools = row.getByTestId('agent-tray-row-tools');
  const tokens = row.getByTestId('agent-tray-row-tokens');
  const activity = row.getByTestId('agent-tray-row-activity');
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('running', { timeout: 30_000 });
  await expect(tools).toHaveCount(0);

  await advanceGate(harness, start.mockId, 'facts-start');
  const read = await awaitGate(harness, 'facts-read', workerP.path);
  await expect(tools).toHaveText('1 tool', { timeout: 30_000 });
  await expect(tokens).toHaveText('2.0k tokens', { timeout: 30_000 });
  await expect(activity).toHaveText(FACTS_READ, { timeout: 30_000 });
  const line = await activity.textContent();

  // Only its spend moves.
  await advanceGate(harness, read.mockId, 'facts-read');
  const spent = await awaitGate(harness, 'facts-spent', workerP.path);
  await expect(tokens).toHaveText('3.0k tokens', { timeout: 30_000 });
  await expect(tools).toHaveText('1 tool');
  await expect(activity).toHaveText(line ?? '');

  // A second call whose line reads as the first one's did.
  await advanceGate(harness, spent.mockId, 'facts-spent');
  const again = await awaitGate(harness, 'facts-again', workerP.path);
  await expect(tools).toHaveText('2 tools', { timeout: 30_000 });
  await expect(activity).toHaveText(line ?? '');
  await expect(tokens).toHaveText('3.0k tokens');

  // Its background shell: a Claude background Bash is a tool call; a Codex
  // terminal is the tray's alone until it ends.
  await advanceGate(harness, again.mockId, 'facts-again');
  const hold = await awaitGate(harness, 'facts-hold', workerP.path);
  const runningTools = provider === 'claude' ? 3 : 2;
  await expect(tools).toHaveText(`${runningTools} tools`, { timeout: 30_000 });
  await expect(activity).toHaveText(provider === 'claude' ? FACTS_SHELL : FACTS_READ, { timeout: 30_000 });
  await expectRuntimeRunning(row);
  expect(await listedRow(harness, lead, token)).toMatchObject({ toolCalls: runningTools, tokens: 3000, turnOpen: true });
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `facts-${provider}-${layout}-running.png`, agentsBody(page));

  // Its turn ends with the shell running: its spawn is not answered, and
  // its row waits on the shell with its counts kept and its runtime still.
  await advanceGate(harness, hold.mockId, 'facts-hold');
  await expect.poll(async () => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', worker)).activeTurn, { timeout: 30_000 }).toBe(true);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('waiting', { timeout: 30_000 });
  await expect(activity).toHaveText(FACTS_SHELL);
  await expect(tools).toHaveText(`${runningTools} tools`);
  await expect(tokens).toHaveText('3.0k tokens');
  expect(ledgerRow(harness, token)!.state, 'a worker with its shell running has not finished').toBe('running');
  // Its row is listed, its turn not open (turnOpen is left out when false).
  await expect.poll(async () => {
    const listed = await listedRow(harness, lead, token);
    return listed !== undefined && !listed.turnOpen;
  }, { timeout: 30_000 }).toBe(true);
  await expectRuntimeIdle(page, row);
  await capture(page, `facts-${provider}-${layout}-waiting.png`, agentsBody(page));

  // The shell ends in its next turn: the spawn settles finished with that
  // turn's message, and the spawner is told.
  const told = awaitMockInput(harness, { cwd: leadP.path, includes: [token, FACTS_ANSWER] });
  await harness.rpc('SendMessage', worker, 'is the build done?', null);
  await told;
  const span = Date.now() - started;
  expect(ledgerRow(harness, token)).toMatchObject({ state: 'finished', answer: FACTS_ANSWER });
  await openPast(page);
  const past = pastRow(page, token);
  // A Codex terminal is written as a tool call when it ends.
  await expect(past.getByTestId('agent-tray-row-tools')).toHaveText('3 tools');
  await expect(past.getByTestId('agent-tray-row-tokens')).toHaveText('3.0k tokens');
  await expect(past.getByTestId('agent-tray-row-activity')).toHaveCount(0);
  // Its runtime is its turns' time, the time between them left out.
  const settled = await listedPastRow(harness, lead, token);
  expect(settled).toMatchObject({ toolCalls: 3, tokens: 3000 });
  expect(settled!.turnOpen ?? false).toBe(false);
  expect(settled!.runtimeMs ?? 0).toBeGreaterThan(0);
  expect(settled!.runtimeMs!, 'the time between its turns is not runtime').toBeLessThan(span - IDLE_MS);
  await expect(past.getByTestId('agent-tray-row-elapsed')).toHaveAttribute('title', ranFor(settled!.runtimeMs!));
}

const REPLY_WORKER = 'Deploy runner';
const REPLY = 'The deploy started.';

/**
 * runReplyThenRest: a worker replies, then works on in the same turn. Its
 * row stays open, running, counting the calls after the reply and its
 * runtime moving; once it rests the row is under Past with its turn's
 * runtime exactly.
 */
export async function runReplyThenRest(harness: HarnessApp, page: Page, provider: ProviderName, layout: 'desktop' | 'compact'): Promise<void> {
  const tag = `agents-reply-${provider}-${layout}`;
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
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Start the deploy, tell me, then watch it.', title: REPLY_WORKER, project_id: workerP.projectId, provider, notify: true } } }], text: 'Started it.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: `${tag}-worker`, provider, echoUserMessage: true,
    turns: [
      { steps: [
        { capture: { var: 'TOKEN', from: '${USER_INPUT}', pattern: FOOTER_TOKEN_PATTERN } },
        { emitLines: commandCall(provider, 'cmd-deploy', 'make deploy') },
        { gate: 'reply-send' },
        { call: { tool: 'thread_reply', args: { token: '${TOKEN}', text: REPLY } } },
        { emitLines: commandCall(provider, 'cmd-watch', 'make watch') },
        { gate: 'reply-continue' },
      ], text: 'Watched the deploy to the end.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'start the deploy', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string; outcome: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
  // The reply waits for the spawn to return, so it reaches the lead as a message.
  await releaseAfterBackgrounded(spawn, { harness, gate: 'reply-send', cwd: workerP.path });
  const { token, thread_id: worker } = spawn.value!;
  const cont = await awaitGate(harness, 'reply-continue', workerP.path);
  await awaitMockInput(harness, { cwd: leadP.path, includes: [token, REPLY] });

  // Replied and working on: open, running, the call after the reply counted.
  await harness.open(page);
  await openAgents(page, LEAD);
  const row = openAgentRow(page, token);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('running', { timeout: 30_000 });
  await expect(row).toHaveAttribute('data-state', 'replied');
  await expect(row.getByTestId('agent-tray-row-tools')).toHaveText('3 tools', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-activity')).toHaveText(/make watch/, { timeout: 30_000 });
  await expectRuntimeRunning(row);
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `reply-${provider}-${layout}-working-on.png`, agentsBody(page));

  // It rests: under Past, replied, with its turn's runtime.
  await advanceGate(harness, cont.mockId, 'reply-continue');
  await expect.poll(async () => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', worker)).activeTurn, { timeout: 30_000 }).toBe(true);
  await expect(row).toHaveCount(0, { timeout: 30_000 });
  await openPast(page);
  const past = pastRow(page, token);
  await expect(past.getByTestId('agent-tray-row-state')).toHaveText('replied');
  await expect(past.getByTestId('agent-tray-row-tools')).toHaveText('3 tools');
  const settled = await listedPastRow(harness, lead, token);
  expect(settled!.turnOpen ?? false).toBe(false);
  expect(settled!.runtimeMs ?? 0).toBeGreaterThan(0);
  await expect(past.getByTestId('agent-tray-row-elapsed')).toHaveAttribute('title', ranFor(settled!.runtimeMs!));
  await capture(page, `reply-${provider}-${layout}-rested.png`, agentsBody(page));
}

const HELD_WORKER = 'Board checker';
const HELD_FAILURE = 'the board API is down';

/**
 * runHeldThenError: a worker's turn ends with a loop armed, so its spawn is
 * held; a later turn of it fails, and the spawn settles failed with that
 * error, its spawner told. The loop still armed, its row stays open,
 * waiting.
 */
export async function runHeldThenError(harness: HarnessApp, page: Page, provider: ProviderName, layout: 'desktop' | 'compact'): Promise<void> {
  const tag = `agents-held-error-${provider}-${layout}`;
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
      { steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Keep an eye on the board.', title: HELD_WORKER, project_id: workerP.projectId, provider, notify: true } } }], text: 'Started it.' },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerP.path, threadToolsScenario({
    name: `${tag}-worker`, provider, echoUserMessage: true,
    turns: [
      { steps: [{ call: { tool: 'thread_loop', args: { label: LOOP_LABEL, note: 'Look at the board.', every_seconds: 3600 } } }], text: 'Looping on the board.' },
      { fail: HELD_FAILURE },
      { text: 'Noted.' },
    ],
    afterTurns: 'repeatLast',
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'watch the board', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadP.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  const { token, thread_id: worker } = spawn.value!;
  const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd: workerP.path, timeoutMs: 60_000 });
  expect(loop.isError, loop.text).toBe(false);
  await expect.poll(async () => !(await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', worker)).activeTurn, { timeout: 30_000 }).toBe(true);

  // Held: waiting on its loop, its spawn not answered.
  await harness.open(page);
  await openAgents(page, LEAD);
  const row = openAgentRow(page, token);
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('waiting', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-activity')).toHaveText(`Loop · ${LOOP_LABEL}`);
  expect(ledgerRow(harness, token)!.state).toBe('running');

  // A later turn fails: the spawn settles failed with its error, once.
  const told = awaitMockInput(harness, { cwd: leadP.path, includes: [token] });
  await harness.rpc('SendMessage', worker, 'look at the board now', null);
  await told;
  expect(ledgerRow(harness, token)).toMatchObject({ state: 'errored' });
  expect(ledgerRow(harness, token)!.answer).toContain(HELD_FAILURE);
  await expect(row).toHaveAttribute('data-state', 'errored', { timeout: 30_000 });
  await expect(row.getByTestId('agent-tray-row-state')).toHaveText('waiting');
  await expect(row.getByTestId('agent-tray-row-activity')).toHaveText(`Loop · ${LOOP_LABEL}`);
  if (layout === 'desktop') await page.mouse.move(800, 400);
  await capture(page, `held-error-${provider}-${layout}.png`, agentsBody(page));
}
