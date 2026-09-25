// A client that loses its connection while a thread's 100 agents stream
// and settle replays the whole burst when it reconnects, with no gap on
// any channel (docs/architecture/transport.md, reconnect replay).
//
// The burst is agent-scale.spec.ts's: 100 async agents, each writing a
// text row, a tool call and its result and a progress tick, then a report
// and its completion, with the mock writing a line every 5 ms, the rate
// the CLI writes them. The page is offline from the first streamed line
// until the last completion is pushed, so its cursor on every channel
// predates the whole burst. The harness client, which receives every frame
// live, measures the frames each channel carried per second; they are
// reported as annotations and on stdout.
import type { WebSocketRoute } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskProgressLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const AGENTS = 100;
const indexes = Array.from({ length: AGENTS }, (_, i) => i);
const tu = (i: number) => `tu-${i}`;
const readId = (i: number) => `tu-${i}-read`;
const taskId = (i: number) => `task-${i}`;
const report = (i: number) => `Shard ${i} is clean.`;
const tasks = indexes.map((i) => ({ task_id: taskId(i), task_type: 'local_agent', description: `shard ${i}` }));

const scenario = claudeScenario('replay-burst', [
  emit([
    ...textLines('msg-lead', 'Sweeping every shard in parallel.'),
    ...indexes.flatMap((i) => [
      toolUseLine(`msg-launch-${i}`, tu(i), 'Agent', { description: `shard ${i}`, subagent_type: 'sweeper', prompt: `Sweep ${i}.` }),
      taskStartedLine(taskId(i), tu(i), `shard ${i}`),
      asyncAgentAckLine(tu(i), taskId(i), `shard ${i}`),
    ]),
    backgroundTasksChangedLine(tasks),
    RESULT_LINE,
  ]),
  { waitSignal: { name: 'stream' } },
  emit(
    indexes.flatMap((i) => [
      ...textLines(`msg-scan-${i}`, `Shard ${i}: scanning.`, tu(i)),
      toolUseLine(`msg-read-${i}`, readId(i), 'Read', { file_path: `shards/${i}.txt` }, tu(i)),
      toolResultLine(readId(i), `shard ${i}: clean`, { parentToolUseId: tu(i) }),
      taskProgressLine(taskId(i), tu(i), `Shard ${i}: scanning.`, { total_tokens: 1000 + i, tool_uses: 1, duration_ms: 500 }, 'Read'),
    ]),
  ),
  { waitSignal: { name: 'finish' } },
  emit(
    indexes.flatMap((i) => [
      ...textLines(`msg-report-${i}`, report(i), tu(i)),
      taskUpdatedLine(taskId(i), { status: 'completed', end_time: 1787419835322 + i }),
      taskNotificationLine(taskId(i), tu(i), report(i), {
        outputFile: `\${CWD}/shard-${i}.jsonl`,
        usage: { total_tokens: 4000 + i, tool_uses: 1, duration_ms: 900 },
        uuid: `done-${i}`,
      }),
      backgroundTasksChangedLine(tasks.slice(i + 1)),
    ]),
  ),
]);

interface WireEvent {
  channel: string;
  seq: number;
  gap?: boolean;
}

function wireEvents(message: string | Buffer): WireEvent[] {
  const frame = JSON.parse(String(message)) as { type?: string; events?: WireEvent[] } & WireEvent;
  if (frame.type === 'batch') return frame.events ?? [];
  if (frame.type === 'event') return [frame];
  return [];
}

test('a reconnect after a 100-agent burst replays every channel without a gap', async ({ harness, page }) => {
  test.setTimeout(300_000);
  let online = true;
  let socket: WebSocketRoute | undefined;
  let connections = 0;
  // Every event the page was sent after its reconnect, replayed or live.
  const afterReconnect: WireEvent[] = [];
  await page.routeWebSocket(/\/ws(?:\?|$)/, (route) => {
    if (!online) {
      void route.close({ code: 1012, reason: 'test network outage' });
      return;
    }
    connections += 1;
    const reconnected = connections > 1;
    const server = route.connectToServer();
    socket = route;
    route.onMessage((message) => server.send(message));
    server.onMessage((message) => {
      if (reconnected) afterReconnect.push(...wireEvents(message));
      route.send(message);
    });
  });
  await harness.rpc('HarnessSetScenario', { scenario });
  const threadId = await seedAgentThread(harness, 'replay-burst-app', 'Replay burst');
  await harness.open(page);
  await page.getByText('Replay burst').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'sweep every shard', null);
  await harness.waitForEvent('provider:turn_completed');
  await expect(page.getByTestId('activity-rail-background-count')).toHaveText(String(AGENTS));

  await waitForGate(harness, 'stream');
  online = false;
  await socket!.close({ code: 1012, reason: 'test network outage' });
  harness.clearEvents();
  const burstAt = Date.now();
  await advance(harness, mockId, 'stream');
  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await expect
    .poll(async () => (await listItems(harness, threadId)).filter((item) => item.completionOf).length, { timeout: 120_000 })
    .toBe(AGENTS);
  // Pushes trail the rows they carry; the burst is over once the
  // item stream has been quiet for a moment.
  let quietSince = 0;
  await expect
    .poll(() => {
      const last = Math.max(0, ...harness.eventTimes('provider:item_event'));
      quietSince = last;
      return Date.now() - last;
    }, { timeout: 60_000 })
    .toBeGreaterThan(1_000);
  const seconds = (quietSince - burstAt) / 1000;

  const rates: Record<string, string> = {};
  for (const channel of ['provider:item_event', 'provider:subagent_progress', 'provider:background_tray', 'provider:background_tasks_changed']) {
    const frames = harness.countEvents(channel);
    rates[channel] = `${frames} frames, ${(frames / seconds).toFixed(0)}/s`;
  }
  const outage = `${seconds.toFixed(2)} s`;
  test.info().annotations.push({ type: 'burst', description: JSON.stringify({ outage, rates }) });
  console.log(`replay-burst: outage ${outage}: ${JSON.stringify(rates)}`);

  online = true;
  await expect(page.getByTestId('activity-rail-background-toggle')).toHaveCount(0, { timeout: 60_000 });
  await expect(page.getByTestId('subagent-group').last()).toContainText(report(AGENTS - 1));
  const gaps = afterReconnect.filter((event) => event.gap).map((event) => event.channel);
  expect(gaps, 'channels the reconnect replay reported a gap on').toEqual([]);
  expect(afterReconnect.filter((event) => event.channel === 'provider:item_event').length).toBeGreaterThan(0);
});
