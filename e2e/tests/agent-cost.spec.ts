// The per-event cost of an agent fan-out does not grow with its size
// (docs/specs/agent-visibility.md, the scale bar: 1 to 100 subagents with
// the same per-event cost, several clients watching). One backend runs the
// same fan-out at N=10 and N=100: one turn launches N background agents,
// each then writes a text row, a tool call and its result and a progress
// tick, then reports and completes. The mock writes each phase unpaced, so
// the time from its release to the push of the phase's last settled row
// (at the harness client, which receives every frame) is the backend's
// time for the phase's provider events.
//
// Three clients watch: two pages on the fan-out's thread and one on another
// thread. Per client the spec records, from its WebSocket, the event frames
// and bytes it was delivered by channel and the RPCs it sent with their
// reply bytes (wire-cost-helpers.ts). What must hold:
//
//   backend - the milliseconds per provider event of the stream and finish
//             phases at N=100 stay within a small factor of N=10's;
//   watching - the bytes a watching client receives per agent at N=100
//             stay within a small factor of N=10's;
//   elsewhere - the client on the other thread receives no subagent
//             progress and no tray frame of the fan-out's thread.
//
// The numbers are reported as annotations and on stdout.
import type { Browser, Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
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
  type ScenarioStep,
} from './agent-visibility-helpers.js';
import type { HarnessApp } from '../src/harness.js';
import { recordPageWire, sumChannels, type WireRecorder, type WireTally } from './wire-cost-helpers.js';

// Channels whose frames only a client showing the thread uses.
const THREAD_CHANNELS = ['provider:subagent_progress', 'provider:background_tray'];
// An unregistered channel, delivered to loopback clients only: the pages.
const BARRIER_CHANNEL = 'e2e:cost-barrier';

function unpaced(lines: string[]): ScenarioStep {
  return { emit: { lines, delayBetweenMs: 0 } };
}

interface FanOut {
  steps: ScenarioStep[];
  lines: { launch: number; stream: number; finish: number };
  launchIds: Set<string>;
  readIds: Set<string>;
  completionPrefix: string;
}

function fanOut(n: number): FanOut {
  const agents = Array.from({ length: n }, (_, i) => i);
  const tu = (i: number) => `c${n}-tu-${i}`;
  const task = (i: number) => `c${n}-task-${i}`;
  const read = (i: number) => `${tu(i)}-read`;
  const tasks = agents.map((i) => ({ task_id: task(i), task_type: 'local_agent', description: `shard ${i}` }));
  const launch = [
    ...textLines(`c${n}-lead`, 'Sweeping every shard.'),
    ...agents.flatMap((i) => [
      toolUseLine(`c${n}-launch-${i}`, tu(i), 'Agent', { description: `shard ${i}`, subagent_type: 'sweeper', prompt: `Sweep ${i}.` }),
      taskStartedLine(task(i), tu(i), `shard ${i}`),
      asyncAgentAckLine(tu(i), task(i), `shard ${i}`),
    ]),
    backgroundTasksChangedLine(tasks),
    RESULT_LINE,
  ];
  // Every agent works at once, as a real fan-out interleaves on the wire:
  // each round writes one line per agent.
  const stream = [
    ...agents.flatMap((i) => textLines(`c${n}-scan-${i}`, `Shard ${i}: scanning.`, tu(i))),
    ...agents.map((i) => toolUseLine(`c${n}-read-${i}`, read(i), 'Read', { file_path: `shards/${i}.txt` }, tu(i))),
    ...agents.map((i) => toolResultLine(read(i), `shard ${i}: clean`, { parentToolUseId: tu(i) })),
    ...agents.map((i) =>
      taskProgressLine(task(i), tu(i), `Shard ${i}: scanning.`, { total_tokens: 1000 + i, tool_uses: 1, duration_ms: 500 }, 'Read'),
    ),
  ];
  // Every agent reports, then they complete one by one, the CLI's level
  // set shrinking by one per completion.
  const finish = [
    ...agents.flatMap((i) => textLines(`c${n}-report-${i}`, `Shard ${i} is clean.`, tu(i))),
    ...agents.flatMap((i) => [
      taskUpdatedLine(task(i), { status: 'completed', end_time: 1787419835322 + i }),
      taskNotificationLine(task(i), tu(i), `Shard ${i} is clean.`, {
        outputFile: `\${CWD}/c${n}-${i}.jsonl`,
        usage: { total_tokens: 4000 + i, tool_uses: 1, duration_ms: 900 },
        uuid: `c${n}-done-${i}`,
      }),
      backgroundTasksChangedLine(tasks.slice(i + 1)),
    ]),
  ];
  return {
    steps: [unpaced(launch), { waitSignal: { name: 'stream' } }, unpaced(stream), { waitSignal: { name: 'finish' } }, unpaced(finish)],
    lines: { launch: launch.length, stream: stream.length, finish: finish.length },
    launchIds: new Set(agents.map(tu)),
    readIds: new Set(agents.map(read)),
    completionPrefix: `c${n}-tu-`,
  };
}

interface ItemEvent {
  threadId?: string;
  itemId?: string;
  item?: { id?: string; completionOf?: string; status?: string };
}

// The arrival of the last item event of a phase at the harness client, a
// wildcard connection that receives every frame as the backend emits it,
// once every row the phase settles has been pushed: `key` names the row an
// event settles, or "" for an event that is not one of them.
async function phaseEnd(harness: HarnessApp, threadId: string, count: number, key: (evt: ItemEvent) => string): Promise<number> {
  let last = 0;
  await expect
    .poll(
      () => {
        const seen = new Set<string>();
        const times = harness.eventTimes<ItemEvent>('provider:item_event', (evt) => {
          if (evt.threadId !== threadId) return false;
          const id = key(evt);
          if (id) seen.add(id);
          return id !== '';
        });
        last = Math.max(0, ...times);
        return seen.size;
      },
      { timeout: 120_000 },
    )
    .toBe(count);
  return last;
}

// The longest wait between two consecutive item events of the thread
// inside [from, to]: the slowest single stretch of backend work.
function longestGap(harness: HarnessApp, threadId: string, from: number, to: number): number {
  const times = harness
    .eventTimes<ItemEvent>('provider:item_event', (evt) => evt.threadId === threadId)
    .filter((t) => t >= from && t <= to)
    .sort((x, y) => x - y);
  let gap = 0;
  for (let i = 1; i < times.length; i += 1) gap = Math.max(gap, times[i] - times[i - 1]);
  return gap;
}

interface Client {
  page: Page;
  wire: WireRecorder;
}

async function openOn(harness: HarnessApp, client: Client, title: string): Promise<void> {
  await harness.open(client.page);
  await client.page.locator(`[data-testid="thread-row-title"][title="${title}"]`).click();
  await expect(client.page.getByTestId('chat-header-title')).toHaveText(title);
}

interface Run {
  n: number;
  msPerEvent: { launch: number; stream: number; finish: number };
  longestGapMs: { launch: number; stream: number; finish: number };
  watching: WireTally[];
  elsewhere: WireTally;
}

async function measure(harness: HarnessApp, clients: Client[], n: number): Promise<Run> {
  const plan = fanOut(n);
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario(`agent-cost-${n}`, plan.steps) });
  const title = `Cost ${n}`;
  const threadId = await seedAgentThread(harness, `cost-${n}-app`, title);
  const [a, b, other] = clients;
  await openOn(harness, a, title);
  await openOn(harness, b, title);
  await openOn(harness, other, 'Quiet thread');
  harness.clearEvents();
  for (const client of clients) client.wire.arm(threadId);

  const mockId = await startMock(harness, threadId);
  const launchAt = Date.now();
  await harness.rpc('SendMessage', threadId, 'sweep every shard', null);
  const launchEnd = await phaseEnd(harness, threadId, n, (evt) =>
    evt.item?.id && plan.launchIds.has(evt.item.id) ? evt.item.id : '',
  );

  await waitForGate(harness, 'stream');
  const streamAt = Date.now();
  await advance(harness, mockId, 'stream');
  const streamEnd = await phaseEnd(harness, threadId, n, (evt) => {
    const id = evt.item?.id ?? evt.itemId ?? '';
    return plan.readIds.has(id) && (evt.item?.status ?? 'completed') !== 'running' ? id : '';
  });

  await waitForGate(harness, 'finish');
  const finishAt = Date.now();
  await advance(harness, mockId, 'finish');
  const finishEnd = await phaseEnd(harness, threadId, n, (evt) =>
    evt.item?.completionOf?.startsWith(plan.completionPrefix) ? evt.item.completionOf : '',
  );

  for (const client of [a, b]) {
    await expect(client.page.getByTestId('activity-rail-background-toggle')).toHaveCount(0, { timeout: 60_000 });
  }
  // Ending the session runs the thread's pending wire refresh at once
  // (the quiet-point pushes the burst left); the barrier frame emitted
  // after it reaches each client behind everything emitted before it.
  await harness.rpc('StopSession', threadId);
  const barrier = `cost-${n}-${Date.now()}`;
  const arrived = clients.map((client) =>
    client.wire.awaitEvent(BARRIER_CHANNEL, (data) => (data as { barrier?: string })?.barrier === barrier),
  );
  await harness.rpc('HarnessEmit', BARRIER_CHANNEL, { barrier });
  await Promise.all(arrived);
  const [watchA, watchB, elsewhere] = clients.map((client) => client.wire.stop());
  return {
    n,
    msPerEvent: {
      launch: (launchEnd - launchAt) / plan.lines.launch,
      stream: (streamEnd - streamAt) / plan.lines.stream,
      finish: (finishEnd - finishAt) / plan.lines.finish,
    },
    longestGapMs: {
      launch: longestGap(harness, threadId, launchAt, launchEnd),
      stream: longestGap(harness, threadId, streamAt, streamEnd),
      finish: longestGap(harness, threadId, finishAt, finishEnd),
    },
    watching: [watchA, watchB],
    elsewhere,
  };
}

function report(run: Run): Record<string, string> {
  const out: Record<string, string> = {
    [`n${run.n}-ms-per-event`]: `launch ${run.msPerEvent.launch.toFixed(3)} stream ${run.msPerEvent.stream.toFixed(3)} finish ${run.msPerEvent.finish.toFixed(3)}`,
    [`n${run.n}-longest-gap-ms`]: `launch ${run.longestGapMs.launch} stream ${run.longestGapMs.stream} finish ${run.longestGapMs.finish}`,
  };
  run.watching.forEach((tally, i) => {
    const events = sumChannels(tally.channels);
    const rpcs = Object.entries(tally.rpcCalls).map(([m, c]) => `${m}:${c}`).join(' ');
    const lists = tally.rpcReplyBytes.ListLiveBackgroundTasks ?? 0;
    out[`n${run.n}-client${i}`] =
      `bytes ${tally.totalBytes} (${(tally.totalBytes / run.n).toFixed(0)}/agent), events ${events.events}, ` +
      `tray list reply bytes ${lists}, rpcs ${rpcs}`;
    out[`n${run.n}-client${i}-channels`] = Object.entries(tally.channels)
      .sort((x, y) => y[1].bytes - x[1].bytes)
      .map(([channel, row]) => `${channel} ${row.events}/${row.bytes}B`)
      .join(', ');
  });
  const leaked = sumChannels(run.elsewhere.threadChannels, THREAD_CHANNELS);
  out[`n${run.n}-elsewhere`] =
    `bytes ${run.elsewhere.totalBytes}, fan-out thread frames ${sumChannels(run.elsewhere.threadChannels).events}, ` +
    `of them progress/tray ${leaked.events}`;
  return out;
}

async function newClient(browser: Browser): Promise<{ client: Client; close: () => Promise<void> }> {
  const context = await browser.newContext();
  const page = await context.newPage();
  return { client: { page, wire: recordPageWire(page) }, close: () => context.close() };
}

test('an agent fan-out costs the same per event at 10 and 100 agents, per client', async ({ harness, page, browser }) => {
  test.setTimeout(420_000);
  await seedAgentThread(harness, 'quiet-app', 'Quiet thread');
  const second = await newClient(browser);
  const third = await newClient(browser);
  try {
    const clients: Client[] = [{ page, wire: recordPageWire(page) }, second.client, third.client];
    const small = await measure(harness, clients, 10);
    const large = await measure(harness, clients, 100);
    for (const run of [small, large]) {
      for (const [type, description] of Object.entries(report(run))) {
        test.info().annotations.push({ type, description });
        console.log(`agent-cost: ${type}: ${description}`);
      }
    }

    for (const phase of ['stream', 'finish'] as const) {
      expect(
        large.msPerEvent[phase],
        `${phase}: ms per provider event at 100 agents against 10 (${small.msPerEvent[phase].toFixed(3)})`,
      ).toBeLessThanOrEqual(2 * small.msPerEvent[phase] + 0.5);
    }
    for (const i of [0, 1]) {
      const perAgentSmall = small.watching[i].totalBytes / small.n;
      const perAgentLarge = large.watching[i].totalBytes / large.n;
      expect(perAgentLarge, `client ${i}: bytes per agent at 100 against 10 (${perAgentSmall.toFixed(0)})`).toBeLessThanOrEqual(
        1.5 * perAgentSmall,
      );
    }
    for (const run of [small, large]) {
      expect(sumChannels(run.elsewhere.threadChannels, THREAD_CHANNELS).events, `N=${run.n}: progress/tray frames to the other thread's client`).toBe(0);
    }
  } finally {
    await second.close();
    await third.close();
  }
});
