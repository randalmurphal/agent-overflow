// Subagent scope narrowing end to end: a watched thread's child rows
// (`parentId` set) reach a page only while a surface on it reads that
// agent's transcript, and the real SPA names that transcript in its watch
// frame.
//
// WHY THIS LEVEL. internal/transport/{event_watch_scope,conn_watch_scope}
// _test.go cover the filter and the frame, and the frontend unit suites
// cover the composition and the client. Only this level proves they are
// wired through the app that ships: the parent pane states an empty scope
// set, opening the agent pane is what names the scope before the pane's
// first read, closing it withdraws the scope, a reconnect restates it
// before replay, and a thread with many streaming agents costs a
// parent-pane-only client none of their rows.
//
// Every phase ends with a ROOT row written after the phase's child rows.
// One connection delivers frames in emission order, so that row's arrival
// proves the child rows before it were withheld rather than late.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  listItems,
  seedAgentThread,
  startMock,
  taskStartedLine,
  taskUpdatedLine,
  toolResultLine,
  toolUseLine,
  waitForGate,
  type ScenarioStep,
} from './agent-visibility-helpers.js';
import {
  fenceSocket,
  readWire,
  recordWire,
  sendClientFrame,
  watchedNow,
  watchedScopesNow,
  type ReceivedEvent,
  type WireLog,
} from './transport-watch-helpers.js';
import type { Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';

const CHILD_ONE = 'Scope child one, written before any view.';
const CHILD_TWO = 'Scope child two, written while the agent pane is open.';
const CHILD_THREE = 'Scope child three, written after the pane closed.';
const FINAL = 'Scoped agent finished.';

/** A subagent's text block, the bare sidechain envelope the CLI forwards. */
function sidechainTextLine(messageId: string, text: string, parent: string): string {
  return JSON.stringify({
    type: 'assistant',
    message: { id: messageId, role: 'assistant', model: 'claude-mock-1', type: 'message', content: [{ type: 'text', text }] },
    parent_tool_use_id: parent,
  });
}

/** Text plus one tool call and its result, all inside `parent`'s scope. */
function childRows(tag: string, text: string, parent: string): string[] {
  return [
    sidechainTextLine(`msg-${tag}`, text, parent),
    toolUseLine(`msg-${tag}-bash`, `tu-${tag}-bash`, 'Bash', { command: `ls ${tag}` }, parent),
    toolResultLine(`tu-${tag}-bash`, `listing ${tag}`, { parentToolUseId: parent }),
  ];
}

/** A root tool call and its result: rows no scope filter withholds. */
function rootMarker(tag: string): string[] {
  return [
    toolUseLine(`msg-${tag}`, `tu-${tag}`, 'Bash', { command: `echo ${tag}` }),
    toolResultLine(`tu-${tag}`, tag),
  ];
}

function itemEvents(wire: WireLog, threadId: string): ReceivedEvent[] {
  return wire.received.filter((event) => event.channel === 'provider:item_event' && event.threadId === threadId);
}

function childEvents(wire: WireLog, threadId: string): ReceivedEvent[] {
  return itemEvents(wire, threadId).filter((event) => event.parentId);
}

async function waitForRootRow(page: Page, threadId: string, idPart: string): Promise<void> {
  await expect
    .poll(async () => itemEvents(await readWire(page), threadId).some((event) => !event.parentId && event.itemId?.includes(idPart)))
    .toBe(true);
}

test('a parent pane receives an agent’s rows only while a view of that agent is open', async ({ harness, page }) => {
  test.setTimeout(90_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('scope-watch', [
      emit([
        toolUseLine('msg-agent', 'tu-scope', 'Agent', { description: 'watch scopes', subagent_type: 'Explore' }),
        taskStartedLine('task-scope', 'tu-scope', 'watch scopes'),
        ...childRows('child-one', CHILD_ONE, 'tu-scope'),
        ...rootMarker('root-one'),
      ]),
      { waitSignal: { name: 'open' } },
      emit([...childRows('child-two', CHILD_TWO, 'tu-scope'), ...rootMarker('root-two')]),
      { waitSignal: { name: 'closed' } },
      emit([
        ...childRows('child-three', CHILD_THREE, 'tu-scope'),
        ...rootMarker('root-three'),
        taskUpdatedLine('task-scope', { status: 'completed', end_time: 1787415964725 }),
        toolResultLine('tu-scope', FINAL),
        RESULT_LINE,
      ]),
    ]),
  });
  const threadId = await seedAgentThread(harness, 'scope-watch-app', 'Scope watch');
  await recordWire(page);
  await harness.open(page);
  await page.getByText('Scope watch').click();
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([threadId]);
  // The parent pane says it reads no agent's rows: `[]`, not an absent set.
  expect(watchedScopesNow(await readWire(page))).toEqual([]);

  const mockId = await startMock(harness, threadId);
  const openGate = waitForGate(harness, 'open');
  await harness.rpc('SendMessage', threadId, 'watch scopes', null);
  await openGate;
  await waitForRootRow(page, threadId, 'tu-root-one');
  const launch = (await listItems(harness, threadId)).find((item) => item.toolName === 'Agent');
  expect(launch, 'the agent launch row was never written').toBeDefined();
  const launchId = launch!.id;
  const stored = (await listItems(harness, threadId)).filter((item) => item.parentId === launchId);
  expect(stored.length, 'the agent wrote no rows to withhold').toBeGreaterThan(0);
  expect(childEvents(await readWire(page), threadId)).toEqual([]);

  // Opening the agent names its scope, before the pane's first history read.
  const card = page.getByTestId('message-timeline-scroll').first().getByTestId('subagent-group').first();
  await expect(card).toBeVisible();
  await card.hover();
  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByText(CHILD_ONE)).toBeVisible();
  await expect.poll(async () => watchedScopesNow(await readWire(page))).toEqual([`${threadId}/${launchId}`]);
  const opened = await readWire(page);
  const scopeWatch = opened.sent.findIndex(
    (frame) => frame.type === 'watch' && (frame.scopes ?? []).some((scope) => scope.scopeRootId === launchId),
  );
  const scopedRead = opened.sent.findIndex(
    (frame) => frame.type === 'rpc' && frame.text.includes(`"scopeRootId":"${launchId}"`),
  );
  expect(scopedRead, 'the pane never read its scope').toBeGreaterThanOrEqual(0);
  expect(scopeWatch).toBeGreaterThanOrEqual(0);
  expect(scopeWatch).toBeLessThan(scopedRead);

  const closedGate = waitForGate(harness, 'closed');
  await advance(harness, mockId, 'open');
  await closedGate;
  await waitForRootRow(page, threadId, 'tu-root-two');
  await expect(pane.getByText(CHILD_TWO)).toBeVisible();
  const whileOpen = childEvents(await readWire(page), threadId);
  expect(whileOpen.length).toBeGreaterThan(0);
  expect(whileOpen.every((event) => event.parentId === launchId)).toBe(true);

  // A reconnect restates the scope before it asks for replay, on the new
  // socket, so the replay is filtered like live delivery.
  const socketsBefore = (await readWire(page)).sockets.length;
  await page.evaluate(() => (window as unknown as { __aoWireSocket: WebSocket }).__aoWireSocket.close());
  await expect
    .poll(async () => {
      const wire = await readWire(page);
      if (wire.sockets.length <= socketsBefore) return false;
      return wire.sent.slice(wire.sockets.at(-1)).some((frame) => frame.type === 'replay');
    }, { timeout: 15_000 })
    .toBe(true);
  const reconnected = await readWire(page);
  const fresh = reconnected.sent.slice(reconnected.sockets.at(-1));
  const restated = fresh.findIndex((frame) => frame.type === 'watch');
  const replay = fresh.findIndex((frame) => frame.type === 'replay');
  expect(restated, 'the new socket never restated the watch').toBeGreaterThanOrEqual(0);
  expect(restated).toBeLessThan(replay);
  expect(fresh[restated]!.scopes).toEqual([{ threadId, scopeRootId: launchId }]);
  await expect(pane.getByText(CHILD_TWO)).toBeVisible();

  // Closing the pane withdraws the scope. The fence proves the backend
  // applied that before the agent writes again.
  await page.getByTestId('agent-pane-close').click();
  await expect(pane).toHaveCount(0);
  await expect.poll(async () => watchedScopesNow(await readWire(page))).toEqual([]);
  await fenceSocket(page, 'scope-watch-closed');
  const childrenBeforeClose = childEvents(await readWire(page), threadId).length;
  const settled = harness.waitForEvent('provider:turn_completed', undefined, 30_000);
  await advance(harness, mockId, 'closed');
  await settled;
  await waitForRootRow(page, threadId, 'tu-root-three');
  expect(childEvents(await readWire(page), threadId)).toHaveLength(childrenBeforeClose);
  // Withheld, not lost: the rows are in the store for the next view.
  expect((await listItems(harness, threadId)).some((item) => item.parentId === launchId && item.summary.includes(CHILD_THREE)))
    .toBe(true);
});

const AGENTS = 100;
const ROUNDS = 3;

/** One turn launching AGENTS awaited agents that each write ROUNDS rounds of rows. */
function fanOutTurn(tag: string): ScenarioStep[] {
  const ids = Array.from({ length: AGENTS }, (_, i) => `tu-${tag}-${i}`);
  return [
    emit(ids.flatMap((id, i) => [
      toolUseLine(`msg-${id}`, id, 'Agent', { description: `${tag} agent ${i}`, subagent_type: 'Explore' }),
      taskStartedLine(`task-${id}`, id, `${tag} agent ${i}`),
    ])),
    emit(Array.from({ length: ROUNDS }, (_, round) => ids.flatMap((id, i) =>
      childRows(`${id}-r${round}`, `${tag} agent ${i} round ${round}`, id))).flat()),
    emit([
      ...ids.flatMap((id, i) => [
        taskUpdatedLine(`task-${id}`, { status: 'completed', end_time: 1787415964725 }),
        toolResultLine(id, `${tag} agent ${i} done`),
      ]),
      RESULT_LINE,
    ]),
  ];
}

interface TurnCost {
  itemFrames: number;
  itemBytes: number;
  childFrames: number;
  childBytes: number;
  socketMessages: number;
  socketBytes: number;
}

async function measureTurn(page: Page, harness: HarnessApp, threadId: string, text: string): Promise<TurnCost> {
  const completions = (wire: WireLog) => wire.received
    .filter((event) => event.channel === 'provider:turn_completed' && event.threadId === threadId).length;
  const start = await readWire(page);
  const completedBefore = completions(start);
  await harness.rpc('SendMessage', threadId, text, null);
  await expect.poll(async () => completions(await readWire(page)), { timeout: 120_000 }).toBeGreaterThan(completedBefore);
  const end = await readWire(page);
  const events = itemEvents({ ...end, received: end.received.slice(start.received.length) }, threadId);
  const children = events.filter((event) => event.parentId);
  const sum = (list: ReceivedEvent[]) => list.reduce((total, event) => total + event.bytes, 0);
  return {
    itemFrames: events.length,
    itemBytes: sum(events),
    childFrames: children.length,
    childBytes: sum(children),
    socketMessages: end.receivedMessages - start.receivedMessages,
    socketBytes: end.receivedBytes - start.receivedBytes,
  };
}

test(`${AGENTS} streaming agents send a parent-pane-only client none of their rows`, async ({ harness, page }) => {
  test.setTimeout(300_000);
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'scope-load',
      provider: 'claude',
      turns: [
        { label: 'scoped', steps: fanOutTurn('scoped') },
        { label: 'unscoped', steps: fanOutTurn('unscoped') },
      ],
      afterTurns: 'silent',
    },
  });
  const threadId = await seedAgentThread(harness, 'scope-load-app', 'Scope load');
  await recordWire(page);
  await harness.open(page);
  await page.getByText('Scope load').click();
  await expect.poll(async () => watchedNow(await readWire(page))).toEqual([threadId]);
  expect(watchedScopesNow(await readWire(page))).toEqual([]);
  await startMock(harness, threadId);

  const scoped = await measureTurn(page, harness, threadId, 'fan out scoped');
  expect(scoped.childFrames).toBe(0);
  expect(scoped.itemFrames).toBeGreaterThan(0);

  // The baseline: the same turn under a watch that states no scope set,
  // which admits every scope of the watched thread, as delivery did before
  // scopes existed. Written on the page's own socket; the page must not
  // restate its own set while the baseline runs.
  await sendClientFrame(page, { type: 'watch', threads: [threadId] });
  await fenceSocket(page, 'scope-load-baseline');
  const unscoped = await measureTurn(page, harness, threadId, 'fan out unscoped');
  const lastWatch = (await readWire(page)).sent.filter((frame) => frame.type === 'watch').at(-1);
  expect(lastWatch?.scopes, 'the page restated its watch during the baseline').toBeUndefined();
  expect(unscoped.childFrames).toBeGreaterThanOrEqual(AGENTS * ROUNDS * 2);

  test.info().annotations.push({ type: 'scope-load', description: JSON.stringify({ agents: AGENTS, rounds: ROUNDS, scoped, unscoped }) });
  console.log(`scope-load ${JSON.stringify({ agents: AGENTS, rounds: ROUNDS, scoped, unscoped })}`);
});
