// Stopping background agents at scale, each owning 3 background shells,
// all launched in an earlier turn (docs/specs/agent-visibility.md, the
// scale bar).
//
// Stop with 100 agents: the interrupt makes the mock write the CLI's kill
// frames for all of them before its ack, 1,200 frames through one thread's
// event queue. What must hold:
//
//   ack     - the confirmed Stop returns without a control timeout, and
//             the interrupted turn is recorded ("Stopped by user").
//   settled - every agent and every shell ends killed, every row under
//             each agent settles errored, no row is left running, and the
//             tray is empty.
//
// The tray's Stop All at 10 and at 100 agents: the page sends one
// StopBackgroundTasks call naming every running row, and the backend
// sends each agent's stop_task, whose kill frames the mock writes before
// its ack (the agent's shells die with it). What must hold:
//
//   one call - one StopBackgroundTasks and no per-row stop, at either size;
//   settled  - every agent and every shell ends killed, no row is left
//              running, the tray empties, and no failure is reported;
//   cost     - the time per kill frame and the bytes the page receives per
//              agent at 100 stay within a small factor of 10's.
//
// The specs report their times, and Stop All its wire tally, as
// annotations and on stdout.
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeTurnsScenario,
  emit,
  listItems,
  openTextLines,
  seedAgentThread,
  shellBackgroundAckLine,
  startMock,
  taskStartedLine,
  textLines,
  toolUseLine,
  waitForGate,
  type Item,
  type ScenarioStep,
} from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';
import { recordPageWire, sumChannels, type WireRecorder, type WireTally } from './wire-cost-helpers.js';

const SHELLS = 3;
const shells = Array.from({ length: SHELLS }, (_, s) => s);
// An unregistered channel, delivered to loopback clients only: the page.
const BARRIER_CHANNEL = 'e2e:stop-barrier';

// Per agent: its level set, task_updated and task_notification, then the
// same three for each shell it owns.
function killFrames(agents: number): number {
  return agents * 3 * (1 + SHELLS);
}

interface Fleet {
  // The turn that launches the agents, then, after the `work` gate, their
  // shells and the rows each agent leaves running.
  launch: ScenarioStep;
  work: ScenarioStep;
}

// N background agents with SHELLS shells each; ids start with prefix.
function fleet(prefix: string, n: number): Fleet {
  const agents = Array.from({ length: n }, (_, i) => i);
  const tu = (i: number) => `${prefix}tu-${i}`;
  const task = (i: number) => `${prefix}task-${i}`;
  const shellTu = (i: number, s: number) => `${prefix}tu-${i}-sh${s}`;
  const shellTask = (i: number, s: number) => `${prefix}task-${i}-sh${s}`;
  const agentTasks = agents.map((i) => ({ task_id: task(i), task_type: 'local_agent', description: `agent ${i}` }));
  const shellTasks = agents.flatMap((i) =>
    shells.map((s) => ({ task_id: shellTask(i, s), task_type: 'local_bash', description: `watch ${i}.${s}` })),
  );
  return {
    launch: emit([
      ...textLines(`${prefix}msg-lead`, 'Launching every agent.'),
      ...agents.flatMap((i) => [
        toolUseLine(`${prefix}msg-launch-${i}`, tu(i), 'Agent', { description: `agent ${i}`, subagent_type: 'worker', prompt: `Run ${i}.` }),
        taskStartedLine(task(i), tu(i), `agent ${i}`),
        asyncAgentAckLine(tu(i), task(i), `agent ${i}`),
      ]),
      backgroundTasksChangedLine(agentTasks),
      RESULT_LINE,
    ]),
    work: emit([
      ...agents.flatMap((i) => [
        ...shells.flatMap((s) => [
          toolUseLine(`${prefix}msg-${i}-sh${s}`, shellTu(i, s), 'Bash', { command: `watch ${i}.${s}`, run_in_background: true }, tu(i)),
          taskStartedLine(shellTask(i, s), shellTu(i, s), `watch ${i}.${s}`, { taskType: 'local_bash', ownedBySubagent: true }),
          shellBackgroundAckLine(shellTu(i, s), shellTask(i, s), tu(i)),
        ]),
        toolUseLine(`${prefix}msg-${i}-read`, `${tu(i)}-read`, 'Read', { file_path: 'README.md' }, tu(i)),
        ...openTextLines(`${prefix}msg-${i}-text`, `Agent ${i} is waiting`, tu(i)),
      ]),
      backgroundTasksChangedLine([...agentTasks, ...shellTasks]),
    ]),
  };
}

function unsettled(items: Item[]): string[] {
  return items
    .filter((i) => (i.status === 'running' || i.status === 'streaming') && !(i.kind === 'tool_call' && i.isBackground))
    .map((i) => `${i.id}:${i.status}`);
}

// Starts a thread's mock session, runs the launch turn and releases the
// work, and waits until every agent and shell is live and each agent's
// rows are running.
async function launchFleet(harness: HarnessApp, threadId: string, agents: number): Promise<string> {
  const mockId = await startMock(harness, threadId);
  const first = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'launch every agent', null);
  await first;
  await waitForGate(harness, 'work');
  await harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name: 'work' });
  await expect
    .poll(async () => (await harness.rpc<Item[]>('ListLiveBackgroundTasks', threadId)).length, { timeout: 60_000 })
    .toBe(agents * (1 + SHELLS));
  await expect.poll(async () => unsettled(await listItems(harness, threadId)).length, { timeout: 60_000 }).toBe(agents * 2);
  return mockId;
}

// Polls until every agent and shell is killed and nothing under them runs,
// and returns the thread's rows.
async function expectAllKilled(harness: HarnessApp, threadId: string, agents: number, stoppedByUser: number): Promise<Item[]> {
  let items: Item[] = [];
  await expect
    .poll(
      async () => {
        items = await listItems(harness, threadId);
        const siblings = items.filter((i) => i.completionOf);
        return {
          unsettled: unsettled(items).length,
          killed: siblings.filter((i) => i.status === 'killed').length,
          siblings: siblings.length,
          tray: (await harness.rpc<Item[]>('ListLiveBackgroundTasks', threadId)).length,
          stoppedByUser: items.filter((i) => i.kind === 'error' && i.summary === 'Stopped by user').length,
        };
      },
      { timeout: 120_000 },
    )
    .toEqual({
      unsettled: 0,
      killed: agents * (1 + SHELLS),
      siblings: agents * (1 + SHELLS),
      tray: 0,
      stoppedByUser,
    });
  return items;
}

// The last write of a row the stop settled.
function lastSettledAt(items: Item[]): number {
  return Math.max(...items.filter((i) => i.completionOf || i.parentId).map((i) => (i as Item & { updatedAt: number }).updatedAt));
}

test('Stop kills 100 agents with 3 shells each: acked without a timeout, every row settled', async ({ harness }) => {
  test.setTimeout(180_000);
  const agents = 100;
  const f = fleet('', agents);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('stop-scale', [
      [f.launch, { waitSignal: { name: 'work' } }, f.work],
      [emit(openTextLines('msg-second', 'Waiting on the agents')), { waitSignal: { name: 'never' } }],
    ]),
  });

  const threadId = await seedAgentThread(harness, 'stop-scale-app', 'Stop at scale');
  await launchFleet(harness, threadId, agents);
  await harness.rpc('SendMessage', threadId, 'what next', null);
  await waitForGate(harness, 'never');

  const listed = await harness.rpc<Array<{ transcriptRootId: string }>>('RunningBackgroundAgents', threadId);
  expect(listed).toHaveLength(agents);
  const stopped = harness.waitForEvent('provider:turn_completed', undefined, 120_000);
  const started = Date.now();
  await harness.rpc('InterruptTurn', threadId, listed.map((agent) => agent.transcriptRootId));
  const acked = Date.now();
  console.log(`stop-scale: ack ${acked - started} ms`);
  await stopped;
  console.log(`stop-scale: turn completed ${Date.now() - started} ms`);

  const items = await expectAllKilled(harness, threadId, agents, 1);
  const toAck = acked - started;
  const toSettled = lastSettledAt(items) - started;
  const perFrame = toSettled / killFrames(agents);
  test.info().annotations.push(
    { type: 'stop-to-ack-ms', description: String(toAck) },
    { type: 'stop-to-last-settled-row-ms', description: String(toSettled) },
    { type: 'kill-frames', description: String(killFrames(agents)) },
    { type: 'ms-per-kill-frame', description: perFrame.toFixed(2) },
  );
  console.log(`stop-scale: ack ${toAck} ms, last settled row ${toSettled} ms, ${killFrames(agents)} kill frames, ${perFrame.toFixed(2)} ms/frame`);
});

interface StopAllRun {
  agents: number;
  toTrayEmptyMs: number;
  msPerKillFrame: number;
  tally: WireTally;
}

async function stopAllRun(harness: HarnessApp, page: Page, wire: WireRecorder, agents: number): Promise<StopAllRun> {
  const prefix = `all${agents}-`;
  const f = fleet(prefix, agents);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario(`stop-all-${agents}`, [[f.launch, { waitSignal: { name: 'work' } }, f.work]]),
  });
  const title = `Stop all ${agents}`;
  const threadId = await seedAgentThread(harness, `stop-all-${agents}-app`, title);
  await harness.open(page);
  await page.locator(`[data-testid="thread-row-title"][title="${title}"]`).click();
  await expect(page.getByTestId('chat-header-title')).toHaveText(title);
  await launchFleet(harness, threadId, agents);
  await expect(page.getByTestId('activity-rail-background-count')).toHaveText(String(agents * (1 + SHELLS)), { timeout: 60_000 });
  await page.getByTestId('activity-rail-background-toggle').click();
  const stopAll = page.getByTestId('activity-rail-background-stop-all');
  await expect(stopAll).toBeEnabled();

  wire.arm(threadId);
  const started = Date.now();
  await stopAll.click();
  await expect(page.getByTestId('activity-rail-background-toggle')).toHaveCount(0, { timeout: 120_000 });
  const toTrayEmptyMs = Date.now() - started;
  const items = await expectAllKilled(harness, threadId, agents, 0);
  // The barrier reaches the page behind every frame emitted before it.
  const barrier = `${prefix}${Date.now()}`;
  const arrived = wire.awaitEvent(BARRIER_CHANNEL, (data) => (data as { barrier?: string })?.barrier === barrier);
  await harness.rpc('HarnessEmit', BARRIER_CHANNEL, { barrier });
  await arrived;
  const tally = wire.stop();
  await expect(page.getByTestId('toast').filter({ hasText: /Failed to stop/ })).toHaveCount(0);
  await harness.rpc('StopSession', threadId);
  return {
    agents,
    toTrayEmptyMs,
    msPerKillFrame: (lastSettledAt(items) - started) / killFrames(agents),
    tally,
  };
}

function reportStopAll(run: StopAllRun): Record<string, string> {
  const events = sumChannels(run.tally.channels);
  return {
    [`n${run.agents}-stop-all`]:
      `tray empty ${run.toTrayEmptyMs} ms, ${run.msPerKillFrame.toFixed(2)} ms/kill frame, ` +
      `rpcs ${Object.entries(run.tally.rpcCalls).map(([m, c]) => `${m}:${c}`).join(' ')}`,
    [`n${run.agents}-stop-all-wire`]:
      `bytes ${run.tally.totalBytes} (${(run.tally.totalBytes / run.agents).toFixed(0)}/agent), events ${events.events}: ` +
      Object.entries(run.tally.channels)
        .sort((x, y) => y[1].bytes - x[1].bytes)
        .map(([channel, row]) => `${channel} ${row.events}/${row.bytes}B`)
        .join(', '),
  };
}

test('Stop All stops 10 and 100 agents with 3 shells each in one call, at the same cost per agent', async ({ harness, page }) => {
  test.setTimeout(420_000);
  const wire = recordPageWire(page);
  const small = await stopAllRun(harness, page, wire, 10);
  const large = await stopAllRun(harness, page, wire, 100);
  for (const run of [small, large]) {
    for (const [type, description] of Object.entries(reportStopAll(run))) {
      test.info().annotations.push({ type, description });
      console.log(`stop-scale: ${type}: ${description}`);
    }
  }

  for (const run of [small, large]) {
    expect(run.tally.rpcCalls.StopBackgroundTasks, `N=${run.agents}: StopBackgroundTasks calls`).toBe(1);
    expect(run.tally.rpcCalls.StopClaudeTask ?? 0, `N=${run.agents}: per-row stops`).toBe(0);
  }
  expect(large.msPerKillFrame, `ms per kill frame at 100 agents against 10 (${small.msPerKillFrame.toFixed(2)})`).toBeLessThanOrEqual(
    2 * small.msPerKillFrame + 1,
  );
  const perAgentSmall = small.tally.totalBytes / small.agents;
  expect(large.tally.totalBytes / large.agents, `bytes per agent at 100 against 10 (${perAgentSmall.toFixed(0)})`).toBeLessThanOrEqual(
    2 * perAgentSmall,
  );
});
