// The app dies while agents run (docs/architecture/turn-lifecycle.md,
// §Crash recovery). What must hold on the next boot:
//
//   settled - every agent the dead process left running ends killed with
//             a `session_died` completion sibling, and every row under
//             it, through a foreground agent it ran, settles errored: no
//             row is left running or streaming, whichever turn it was
//             written in.
//   once    - a second boot finds nothing to settle and writes nothing.
import { test, expect } from './fixtures.js';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeTurnsScenario,
  emit,
  itemMeta,
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

function unsettled(items: Item[]): string[] {
  return items
    .filter((i) => (i.status === 'running' || i.status === 'streaming') && !(i.kind === 'tool_call' && i.isBackground))
    .map((i) => `${i.id}:${i.status}`)
    .sort();
}

type Row = Item & { rev: number; updatedAt: number };

function snapshot(items: Item[]): string[] {
  return (items as Row[]).map((i) => `${i.id}|${i.status}|${i.summary}|${i.rev}|${i.updatedAt}`).sort();
}

test('a crash with agents running: the next boot settles every agent and its rows once', async () => {
  const dataDir = await mkdtemp(path.join(tmpdir(), 'ao-agent-restart-'));
  let host: HarnessApp | undefined;
  try {
    host = await launchHarness({ dataDir });
    const tasks = [
      { task_id: 'task-a', task_type: 'local_agent', description: 'worker a' },
      { task_id: 'task-b', task_type: 'local_agent', description: 'worker b' },
    ];
    await host.rpc('HarnessSetScenario', {
      scenario: claudeTurnsScenario('agent-restart', [
        [
          emit([
            ...textLines('msg-lead', 'Launching two workers.'),
            toolUseLine('msg-launch-a', 'tu-a', 'Agent', { description: 'worker a', subagent_type: 'worker', prompt: 'Do a.' }),
            taskStartedLine('task-a', 'tu-a', 'worker a'),
            asyncAgentAckLine('tu-a', 'task-a', 'worker a'),
            toolUseLine('msg-launch-b', 'tu-b', 'Agent', { description: 'worker b', subagent_type: 'worker', prompt: 'Do b.' }),
            taskStartedLine('task-b', 'tu-b', 'worker b'),
            asyncAgentAckLine('tu-b', 'task-b', 'worker b'),
            backgroundTasksChangedLine(tasks),
            RESULT_LINE,
          ]),
          { waitSignal: { name: 'work' } },
          emit([
            // Agent a: a shell it owns, a foreground agent of its own that
            // is mid tool call and mid answer, and its own open answer.
            toolUseLine('msg-a-shell', 'tu-a-shell', 'Bash', { command: 'watch a', run_in_background: true }, 'tu-a'),
            taskStartedLine('task-a-shell', 'tu-a-shell', 'watch a', { taskType: 'local_bash', ownedBySubagent: true }),
            shellBackgroundAckLine('tu-a-shell', 'task-a-shell', 'tu-a'),
            toolUseLine('msg-a-sub', 'tu-a-sub', 'Agent', { description: 'helper', subagent_type: 'worker', prompt: 'Help a.' }, 'tu-a'),
            toolUseLine('msg-a-sub-read', 'tu-a-sub-read', 'Read', { file_path: 'README.md' }, 'tu-a-sub'),
            ...openTextLines('msg-a-sub-text', 'Helper is reading', 'tu-a-sub'),
            ...openTextLines('msg-a-text', 'Worker a is waiting', 'tu-a'),
            // Agent b: a tool call in flight.
            toolUseLine('msg-b-read', 'tu-b-read', 'Read', { file_path: 'README.md' }, 'tu-b'),
            backgroundTasksChangedLine([...tasks, { task_id: 'task-a-shell', task_type: 'local_bash', description: 'watch a' }]),
          ]),
        ],
        [
          // The second turn is open when the app dies.
          emit(openTextLines('msg-second', 'Thinking about the next step')),
          { waitSignal: { name: 'never' } },
        ],
      ]),
    });
    const threadId = await seedAgentThread(host, 'restart-app', 'Agent restart');
    const mockId = await startMock(host, threadId);
    const completed = host.waitForEvent('provider:turn_completed');
    await host.rpc('SendMessage', threadId, 'launch two workers', null);
    await completed;
    await waitForGate(host, 'work');
    await advance(host, mockId, 'work');
    await host.rpc('SendMessage', threadId, 'what next', null);
    await waitForGate(host, 'never');
    const live = host;
    await expect
      .poll(async () => unsettled(await listItems(live, threadId)).map((row) => row.split(':').slice(0, -1).join(':')))
      .toEqual(
        expect.arrayContaining(['tu-a-sub', 'tu-a-sub-read', 'tu-b-read', 'text:1:tu-a:1', 'text:1:tu-a-sub:1']),
      );

    expect(await host.crash()).toBe(true);
    host = await launchHarness({ dataDir });
    const recovered = await listItems(host, threadId);
    expect(unsettled(recovered)).toEqual([]);
    expect(
      recovered
        .filter((i) => i.completionOf)
        .map((i) => `${i.completionOf}:${i.status}:${itemMeta(i).status_source}`)
        .sort(),
    ).toEqual(['tu-a-shell:killed:session_died', 'tu-a:killed:session_died', 'tu-b:killed:session_died']);
    const byId = new Map(recovered.map((i) => [i.id, i]));
    for (const id of ['tu-a-sub', 'tu-a-sub-read', 'tu-b-read', 'text:1:tu-a:1', 'text:1:tu-a-sub:1']) {
      expect(`${id}:${byId.get(id)?.status}`).toBe(`${id}:errored`);
    }

    expect(await host.stop()).toBe(true);
    host = await launchHarness({ dataDir });
    expect(snapshot(await listItems(host, threadId))).toEqual(snapshot(recovered));
  } finally {
    await host?.close();
    await rm(dataDir, { recursive: true, force: true });
  }
});
