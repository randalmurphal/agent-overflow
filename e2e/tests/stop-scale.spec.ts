// Stop with 100 background agents, each owning 3 background shells, all
// launched in an earlier turn (docs/specs/agent-visibility.md, the scale
// bar). The interrupt makes the mock write the CLI's kill frames for all
// of them before its ack: 1,200 frames through one thread's event queue.
// What must hold:
//
//   ack     - the confirmed Stop returns without a control timeout, and
//             the interrupted turn is recorded ("Stopped by user").
//   settled - every agent and every shell ends killed, every row under
//             each agent settles errored, no row is left running, and the
//             tray is empty.
//
// The spec reports the time from Stop to the ack and to the last settled
// row, and the mean handling time per kill frame, as annotations.
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
} from './agent-visibility-helpers.js';

const AGENTS = 100;
const SHELLS = 3;
const agents = Array.from({ length: AGENTS }, (_, i) => i);
const shells = Array.from({ length: SHELLS }, (_, s) => s);
const tu = (i: number) => `tu-${i}`;
const task = (i: number) => `task-${i}`;
const shellTu = (i: number, s: number) => `tu-${i}-sh${s}`;
const shellTask = (i: number, s: number) => `task-${i}-sh${s}`;
const agentTasks = agents.map((i) => ({ task_id: task(i), task_type: 'local_agent', description: `agent ${i}` }));
const shellTasks = agents.flatMap((i) =>
  shells.map((s) => ({ task_id: shellTask(i, s), task_type: 'local_bash', description: `watch ${i}.${s}` })),
);
// Per agent: its level set, task_updated and task_notification, then the
// same three for each shell it owns.
const KILL_FRAMES = AGENTS * 3 * (1 + SHELLS);

function unsettled(items: Item[]): string[] {
  return items
    .filter((i) => (i.status === 'running' || i.status === 'streaming') && !(i.kind === 'tool_call' && i.isBackground))
    .map((i) => `${i.id}:${i.status}`);
}

test('Stop kills 100 agents with 3 shells each: acked without a timeout, every row settled', async ({ harness }) => {
  test.setTimeout(180_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeTurnsScenario('stop-scale', [
      [
        emit([
          ...textLines('msg-lead', 'Launching every agent.'),
          ...agents.flatMap((i) => [
            toolUseLine(`msg-launch-${i}`, tu(i), 'Agent', { description: `agent ${i}`, subagent_type: 'worker', prompt: `Run ${i}.` }),
            taskStartedLine(task(i), tu(i), `agent ${i}`),
            asyncAgentAckLine(tu(i), task(i), `agent ${i}`),
          ]),
          backgroundTasksChangedLine(agentTasks),
          RESULT_LINE,
        ]),
        { waitSignal: { name: 'work' } },
        emit([
          ...agents.flatMap((i) => [
            ...shells.flatMap((s) => [
              toolUseLine(`msg-${i}-sh${s}`, shellTu(i, s), 'Bash', { command: `watch ${i}.${s}`, run_in_background: true }, tu(i)),
              taskStartedLine(shellTask(i, s), shellTu(i, s), `watch ${i}.${s}`, { taskType: 'local_bash', ownedBySubagent: true }),
              shellBackgroundAckLine(shellTu(i, s), shellTask(i, s), tu(i)),
            ]),
            toolUseLine(`msg-${i}-read`, `${tu(i)}-read`, 'Read', { file_path: 'README.md' }, tu(i)),
            ...openTextLines(`msg-${i}-text`, `Agent ${i} is waiting`, tu(i)),
          ]),
          backgroundTasksChangedLine([...agentTasks, ...shellTasks]),
        ]),
      ],
      [
        emit(openTextLines('msg-second', 'Waiting on the agents')),
        { waitSignal: { name: 'never' } },
      ],
    ]),
  });

  const threadId = await seedAgentThread(harness, 'stop-scale-app', 'Stop at scale');
  const mockId = await startMock(harness, threadId);
  const first = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'launch every agent', null);
  await first;
  await waitForGate(harness, 'work');
  await harness.rpc('HarnessMockCommand', mockId, { type: 'advance', name: 'work' });
  await expect
    .poll(async () => (await harness.rpc<Item[]>('ListLiveBackgroundTasks', threadId)).length, { timeout: 60_000 })
    .toBe(AGENTS * (1 + SHELLS));
  await expect.poll(async () => unsettled(await listItems(harness, threadId)).length, { timeout: 60_000 }).toBe(AGENTS * 2);
  await harness.rpc('SendMessage', threadId, 'what next', null);
  await waitForGate(harness, 'never');

  const listed = await harness.rpc<Array<{ transcriptRootId: string }>>('RunningBackgroundAgents', threadId);
  expect(listed).toHaveLength(AGENTS);
  const stopped = harness.waitForEvent('provider:turn_completed', undefined, 120_000);
  const started = Date.now();
  await harness.rpc('InterruptTurn', threadId, listed.map((agent) => agent.transcriptRootId));
  const acked = Date.now();
  console.log(`stop-scale: ack ${acked - started} ms`);
  await stopped;
  console.log(`stop-scale: turn completed ${Date.now() - started} ms`);

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
      killed: AGENTS * (1 + SHELLS),
      siblings: AGENTS * (1 + SHELLS),
      tray: 0,
      stoppedByUser: 1,
    });
  const settledAt = Math.max(
    ...items.filter((i) => i.completionOf || i.parentId).map((i) => (i as Item & { updatedAt: number }).updatedAt),
  );
  const toAck = acked - started;
  const toSettled = settledAt - started;
  const perFrame = toSettled / KILL_FRAMES;
  test.info().annotations.push(
    { type: 'stop-to-ack-ms', description: String(toAck) },
    { type: 'stop-to-last-settled-row-ms', description: String(toSettled) },
    { type: 'kill-frames', description: String(KILL_FRAMES) },
    { type: 'ms-per-kill-frame', description: perFrame.toFixed(2) },
  );
  console.log(`stop-scale: ack ${toAck} ms, last settled row ${toSettled} ms, ${KILL_FRAMES} kill frames, ${perFrame.toFixed(2)} ms/frame`);
});
