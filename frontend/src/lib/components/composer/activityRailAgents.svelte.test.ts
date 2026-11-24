import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAgentController, type AgentController } from './activityRailAgents.svelte';
import { AGENT_ROW_SETTLED_LINGER_MS } from './activityRailBackground.svelte';
import { buildPane, makeThread } from '../../../test/helpers/chat';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { answerAsV0019 } from '../../../test/helpers/olderComputer';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { setupEventListeners } from '../../stores/events';
import type { ThreadAgentRow } from '../../stores/bindings';
import type { UserMessageOriginWake } from '../../utils/userMessageMeta';
import { ReportDescriptor } from '../../../../bindings/agent-overflow/internal/agentmeta/models';
import { replaceQueueForThread, type QueueItem } from '../../stores/sendQueue.svelte';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

vi.mock('../../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

function queued(id: string, over: Partial<QueueItem> = {}): QueueItem {
  return { id, sendId: id, threadId: '', message: id, attachmentIds: [], enqueuedAt: 1_000, ...over } as QueueItem;
}

function row(token: string, over: Partial<ThreadAgentRow> = {}): ThreadAgentRow {
  return {
    token, callerThreadId: 'caller', kind: 'spawn', threadId: `t-${token}`, title: token,
    state: 'running', createdAt: 1_000, depth: 0, ...over,
  } as ThreadAgentRow;
}

describe('agents controller', () => {
  let release = () => {};
  let stopEvents = () => {};
  afterEach(() => {
    release();
    release = () => {};
    stopEvents();
    stopEvents = () => {};
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  async function mountAgents(rows: ThreadAgentRow[], hidden = false) {
    vi.useFakeTimers();
    const pane = await buildPane();
    noteThread(pane.threadId!, '', 2);
    const read = setBindingMock('ListThreadAgentRows', async () => ({ rows, hidden }));
    setBindingMock('ListThreadPastAgentRows', async () => ({ rows: [], total: 0 }));
    stopEvents = setupEventListeners();
    // The rail's clock, which a test can hold still while time passes.
    const clock = $state({ now: Date.now() });
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => pane, () => clock.now);
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    return { pane, read, controller, clock };
  }

  it('reads its thread on open and counts open rows only', async () => {
    const { pane, read, controller } = await mountAgents([
      row('child'),
      row('grand', { callerThreadId: 't-child', depth: 1, state: 'blocked', blockedOn: 'approval' }),
      row('done', { state: 'finished', settledAt: Date.now() }),
    ], true);
    expect(read).toHaveBeenCalledWith(pane.threadId);
    expect(controller.rows.map((r) => r.token)).toEqual(['child', 'grand', 'done']);
    expect(controller.count).toBe(2);
    expect(controller.runningCount).toBe(1);
    expect(controller.hidden).toBe(true);
    expect(controller.hasPendingSettled).toBe(true);
  });

  it('keeps a row whose worker still runs open past the linger, and counts its worker once', async () => {
    const { controller, clock } = await mountAgents([
      row('live', { state: 'replied', settledAt: 1, worker: 'running', threadId: 't-worker' }),
      row('send', { kind: 'send', state: 'running', threadId: 't-worker' }),
    ]);
    clock.now = Date.now() + 2 * AGENT_ROW_SETTLED_LINGER_MS;
    controller.dismiss('live');
    expect(controller.rows.map((r) => r.token)).toEqual(['live', 'send']);
    expect(controller.count).toBe(1);
    expect(controller.runningCount).toBe(1);
    expect(controller.hasPendingSettled).toBe(false);
  });

  it('reads Past again once a row it held open for its worker leaves the list', async () => {
    const { pane, controller } = await mountAgents([row('live', { state: 'replied', settledAt: 1, worker: 'running' })]);
    const past = setBindingMock('ListThreadPastAgentRows', async () => ({ rows: [row('live', { state: 'replied', settledAt: 1 })], total: 1 }));
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'live', listed: false }]);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'live' });
    await vi.advanceTimersByTimeAsync(2_000);
    expect(controller.rows).toEqual([]);
    expect(past).toHaveBeenCalled();
    expect(controller.past.total).toBe(1);
  });

  it('lingers a settled row 8 s from when it first saw it settled, and a click takes it out early', async () => {
    const { pane, controller, clock } = await mountAgents([
      row('done', { state: 'finished', settledAt: 1 }),
      row('other', { state: 'replied', settledAt: 1 }),
    ]);
    // The shipped linger (docs/specs/standing-agents.md, Lifetime), written
    // here rather than read from AGENT_ROW_SETTLED_LINGER_MS: a settled row
    // is a click target for that long.
    clock.now = Date.now() + 7_999;
    expect(controller.rows.map((r) => r.token)).toEqual(['done', 'other']);
    controller.dismiss('other');
    expect(controller.rows.map((r) => r.token)).toEqual(['done']);
    clock.now = Date.now() + 8_000;
    expect(controller.rows).toEqual([]);
    expect(controller.shown).toBe(0);
    void pane;
  });

  it('measures the linger from the wall clock when the rail clock is stopped', async () => {
    // The rail's clock last ticked long ago and only starts again once a
    // settled row is shown.
    const { pane, controller, clock } = await mountAgents([row('child')]);
    clock.now = 0;
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'child', row: row('child', { state: 'finished', settledAt: Date.now() }), listed: true }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    clock.now = Date.now();
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
  });

  it('keeps a settled row out when the pane returns to its thread, for the last 64 threads it showed', async () => {
    vi.useFakeTimers();
    await buildPane();
    const done = row('done', { callerThreadId: 'thread-a', state: 'finished', settledAt: Date.now() });
    const clicked = row('clicked', { callerThreadId: 'thread-a', state: 'replied', settledAt: Date.now() });
    setBindingMock('ListThreadAgentRows', async (id: string) => ({ rows: id === 'thread-a' ? [done, clicked] : [], hidden: false }));
    stopEvents = setupEventListeners();
    const current = $state({ id: 'thread-a' });
    const clock = $state({ now: Date.now() });
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => ({ thread: { id: current.id }, hasDraftPlaceholder: false }) as never, () => clock.now);
      return controller.mount();
    });
    const show = async (id: string) => {
      noteThread(id, '', 2);
      current.id = id;
      await vi.advanceTimersByTimeAsync(500);
      clock.now = Date.now();
    };
    await show('thread-a');
    controller.dismiss('clicked');
    expect(controller.rows.map((r) => r.token)).toEqual(['done']);
    await vi.advanceTimersByTimeAsync(AGENT_ROW_SETTLED_LINGER_MS);
    clock.now = Date.now();
    expect(controller.rows).toEqual([]);

    await show('thread-b');
    await show('thread-a');
    expect(controller.rows.map((r) => r.token), 'a row came back after its linger or its click').toEqual([]);

    // Past the bound the thread is forgotten, and its rows linger afresh.
    for (let index = 0; index < 64; index++) await show(`thread-${index}`);
    await show('thread-a');
    expect(controller.rows.map((r) => r.token)).toEqual(['done', 'clicked']);
  });

  it('remembers a thread through 63 others and forgets it after 64', async () => {
    vi.useFakeTimers();
    await buildPane();
    const done = row('done', { callerThreadId: 'thread-a', state: 'finished', settledAt: Date.now() });
    noteThread('thread-a', '', 2);
    for (let i = 0; i < 70; i++) noteThread(`other-${i}`, '', 2);
    setBindingMock('ListThreadAgentRows', async (id: string) => ({ rows: id === 'thread-a' ? [done] : [], hidden: false }));
    stopEvents = setupEventListeners();
    const current = $state({ id: 'thread-a' });
    const clock = $state({ now: Date.now() });
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => ({ thread: { id: current.id }, hasDraftPlaceholder: false }) as never, () => clock.now);
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(AGENT_ROW_SETTLED_LINGER_MS + 100);
    clock.now = Date.now();
    expect(controller.rows).toEqual([]);
    const visit = async (others: number) => {
      for (let i = 0; i < others; i++) {
        current.id = `other-${i}`;
        await vi.advanceTimersByTimeAsync(20);
      }
      current.id = 'thread-a';
      await vi.advanceTimersByTimeAsync(500);
      clock.now = Date.now();
      return controller.rows.map((r) => r.token);
    };
    expect(await visit(63), 'after 63 other threads').toEqual([]);
    await vi.advanceTimersByTimeAsync(AGENT_ROW_SETTLED_LINGER_MS + 100);
    clock.now = Date.now();
    expect(await visit(64), 'after 64 other threads').toEqual(['done']);
  });

  it('keeps a settled row while a row nested under it is shown', async () => {
    const { controller, clock } = await mountAgents([
      row('child', { state: 'finished', settledAt: 1 }),
      row('grand', { callerThreadId: 't-child', depth: 1 }),
    ]);
    clock.now = Date.now() + AGENT_ROW_SETTLED_LINGER_MS * 10;
    expect(controller.rows.map((r) => r.token)).toEqual(['child', 'grand']);
  });

  it('keeps the identity of rows a read did not change', async () => {
    const { pane, controller } = await mountAgents([row('a'), row('b')]);
    const [a, b] = controller.rows;
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'b', row: row('b', { activity: 'Reading' }), listed: true }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'b' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows[0]).toBe(a);
    expect(controller.rows[1]).not.toBe(b);
    expect(controller.rows[1].activity).toBe('Reading');
  });

  // The placement a row shows is what the computer holding its thread said,
  // in its answer to Show in Sidebar or a frame; a list read relays a cache
  // of that computer, from before or after the placement alike.
  it('keeps the placement its thread\'s computer said over every list read, until that computer says another', async () => {
    const { pane, controller } = await mountAgents([row('child', { placement: 'owned' })]);
    let land = (_rows: ThreadAgentRow[]) => {};
    setBindingMock('ListThreadAgentRows', () => new Promise((resolve) => {
      land = (rows) => resolve({ rows, hidden: false });
    }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    controller.placed('t-child', 'standalone', '');
    // A Past read that starts and finishes meanwhile does not answer for it.
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    land([row('child', { placement: 'owned' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.rows[0].placement, 'the older read undid Show in Sidebar').toBe('standalone');
    // Nor does one started after the placement.
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    land([row('child', { placement: 'owned' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.rows[0].placement, 'a later cached read undid Show in Sidebar').toBe('standalone');
    // The thread's computer announces Remove from Sidebar.
    emitWailsEvent('thread:updated', { action: 'full', thread: makeThread({ id: 't-child', placement: 'owned' }) });
    expect(controller.rows[0].placement).toBe('owned');
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    land([row('child', { placement: 'standalone' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.rows[0].placement, 'a cached read undid Remove from Sidebar').toBe('owned');
  });

  // A frame missed while the window's connection to that computer was down
  // leaves no word of it standing: the rows show what the reads relay.
  it('drops what a computer said of a placement when the connection to it drops', async () => {
    const { pane, controller } = await mountAgents([row('child', { placement: 'owned' })]);
    controller.placed('t-child', 'standalone', '');
    __setBackendStatusForTest('', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null });
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [row('child', { placement: 'owned' })], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows[0].placement).toBe('owned');
  });

  // Frames about threads the tray does not show leave nothing behind.
  it('holds no placement for a thread it does not show', async () => {
    const { pane, controller } = await mountAgents([row('child', { placement: 'owned' })]);
    emitWailsEvent('thread:updated', { action: 'listed', thread: makeThread({ id: 't-elsewhere', placement: 'standalone' }) });
    setBindingMock('ListThreadAgentRows', async () => ({
      rows: [row('child', { placement: 'owned' }), row('elsewhere', { placement: 'owned' })], hidden: false,
    }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows.map((r) => r.placement)).toEqual(['owned', 'owned']);
  });

  // An assistant's helper takes no placement: its row reads none, while its
  // thread's frames still carry the stored column.
  it('gives no placement to a row that takes none, from a frame or the reads after it', async () => {
    const { pane, controller } = await mountAgents([row('helper', { placement: undefined })]);
    emitWailsEvent('thread:updated', { action: 'full', thread: makeThread({ id: 't-helper', placement: 'owned' }) });
    expect(controller.rows[0].placement, 'the frame offered Show in Sidebar').toBeFalsy();
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows[0].placement, 'a read after the frame offered Show in Sidebar').toBeFalsy();
  });

  it('reads again for its own thread and a nested caller, not another thread, and after a lost frame', async () => {
    const { pane, read } = await mountAgents([row('child'), row('grand', { callerThreadId: 't-child', depth: 1 })]);
    const readRow = setBindingMock('GetThreadAgentRows', async (_id: string, tokens: string[]) => tokens.map((token) => ({ token, row: row(token), listed: true })));
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'unrelated', id: 'x' });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(1);
    expect(readRow).not.toHaveBeenCalled();
    emitWailsEvent('thread:agent_rows_changed', { threadId: 't-child', id: 'new' });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(2);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(3);
    emitWailsEvent('transport:gap', { channel: 'thread:agent_rows_changed' }, '');
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(4);
    expect(readRow).not.toHaveBeenCalled();
  });

  it('reads the list again for a frame that lands during a list read, about its thread or a thread the answer nests', async () => {
    vi.useFakeTimers();
    const pane = await buildPane();
    noteThread(pane.threadId!, '', 2);
    const answers: ((rows: ThreadAgentRow[]) => void)[] = [];
    const read = setBindingMock('ListThreadAgentRows', () => new Promise((resolve) => {
      answers.push((rows) => resolve({ rows, hidden: false }));
    }));
    stopEvents = setupEventListeners();
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => pane, () => Date.now());
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(read).toHaveBeenCalledTimes(1);
    // The first read's only frame: a spawn the answer predates.
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    answers[0]([]);
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(2);
    // A frame about a thread the second answer nests, while it is out.
    emitWailsEvent('thread:agent_rows_changed', { threadId: 't-child', id: 'grand' });
    answers[1]([row('child')]);
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(3);
    answers[2]([row('child'), row('grand', { callerThreadId: 't-child', depth: 1 })]);
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows.map((r) => r.token)).toEqual(['child', 'grand']);
    // A frame during a read about a thread the answer does not nest reads nothing more.
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'unrelated', id: 'x' });
    answers[3]([row('child')]);
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(4);
  });

  it('reads the rows a burst named in one read and keeps their places in the tree', async () => {
    const { pane, read, controller } = await mountAgents([
      row('child', { nestedHidden: true }),
      row('grand', { callerThreadId: 't-child', depth: 1 }),
    ]);
    const readRows = setBindingMock('GetThreadAgentRows', async (_id: string, tokens: string[]) => tokens.map((token) => ({
      token, row: row(token, { callerThreadId: token === 'grand' ? 't-child' : 'caller', activity: `Reading ${token}` }), listed: true,
    })));
    for (let i = 0; i < 20; i++) {
      emitWailsEvent('thread:agent_rows_changed', { threadId: 't-child', id: 'grand' });
      emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    }
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(1);
    expect(readRows).toHaveBeenCalledTimes(1);
    expect(readRows.mock.calls[0][0]).toBe(pane.threadId);
    expect([...(readRows.mock.calls[0][1] as string[])].sort()).toEqual(['child', 'grand']);
    expect(controller.rows.map((r) => [r.token, r.depth, r.nestedHidden ?? false, r.activity])).toEqual([
      ['child', 0, true, 'Reading child'],
      ['grand', 1, false, 'Reading grand'],
    ]);
  });

  it('reads the list for a burst naming more rows than one row read takes', async () => {
    const many = Array.from({ length: 128 }, (_, i) => row(`r${i}`));
    const { pane, read } = await mountAgents(many);
    const readRows = setBindingMock('GetThreadAgentRows', async (_id: string, tokens: string[]) => tokens.map((token) => ({ token, row: row(token), listed: true })));
    for (const one of many) emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: one.token });
    await vi.advanceTimersByTimeAsync(500);
    expect(readRows).not.toHaveBeenCalled();
    expect(read).toHaveBeenCalledTimes(2);
    for (const one of many.slice(0, 2)) emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: one.token });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(2);
    expect(readRows).toHaveBeenCalledTimes(1);
    expect(readRows.mock.calls[0][1]).toEqual(['r0', 'r1']);
  });

  it('reads the list when a row read says the row left it, or the row is a paired computer\'s', async () => {
    const { pane, read, controller } = await mountAgents([
      row('child'),
      row('remote', { computerId: 'box', computer: 'Box' }),
      row('nested', { callerThreadId: 't-remote', depth: 1, ownerComputerId: 'box' }),
    ]);
    const readRow = setBindingMock('GetThreadAgentRows', async (_id: string, tokens: string[]) => tokens.map((token) => ({ token, listed: false })));
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [row('remote', { computerId: 'box', computer: 'Box' })], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readRow).toHaveBeenCalledTimes(1);
    expect(controller.rows.map((r) => r.token)).toEqual(['remote']);
    const listAgain = setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'remote' });
    await vi.advanceTimersByTimeAsync(500);
    expect(listAgain).toHaveBeenCalledTimes(1);
    expect(readRow).toHaveBeenCalledTimes(1);
    void read;
  });

  it('lists an agent\'s queued message as incoming until it leaves the queue, and not a person\'s', async () => {
    const { pane, controller } = await mountAgents([row('child')]);
    const request = queued('thread-request:tok', {
      agentMessage: true,
      originThread: { computerId: '', computerName: '', threadId: 'sender', title: 'Sender', token: 'tok', kind: 'send' },
    });
    replaceQueueForThread(pane.threadId!, [queued('composer-1'), request]);
    expect(controller.incoming.map((m) => [m.item.sendId, m.kind])).toEqual([['thread-request:tok', 'request']]);
    expect(controller.count).toBe(2);
    expect(controller.shown).toBe(2);
    const first = controller.incoming[0];
    replaceQueueForThread(pane.threadId!, [queued('composer-1'), request]);
    expect(controller.incoming[0].item).toBe(first.item);
    replaceQueueForThread(pane.threadId!, [queued('composer-1')]);
    expect(controller.incoming).toEqual([]);
    expect(controller.count).toBe(1);
  });

  it('names each incoming message by its origin\'s kind, and lists only what the queue marks an agent\'s', async () => {
    const { pane, controller } = await mountAgents([]);
    const thread = (kind: string, threadId = 'sender') => ({ computerId: '', computerName: '', threadId, title: 'Sender', token: 'tok', kind });
    // A queued report's attribution as Go serves it; the kind names it here.
    const wake = (kind: string, label: string): UserMessageOriginWake => ({
      kind, label, threadId: '', computer: '', computerId: '', report: ReportDescriptor.createFrom({ badge: `${kind} · ${label}` }),
    });
    replaceQueueForThread(pane.threadId!, [
      queued('spawn', { agentMessage: true, originThread: thread('spawn') }),
      queued('ask', { agentMessage: true, originThread: thread('ask') }),
      queued('reply', { agentMessage: true, originThread: thread('wake') }),
      queued('reminder', { agentMessage: true }),
      queued('worker-question', { agentMessage: true, originWake: wake('question', 'Worker') }),
      queued('worker-approval', { agentMessage: true, originWake: wake('approval', 'Worker') }),
      queued('loop', { agentMessage: true, originWake: wake('loop', 'Nightly') }),
      queued('completion', { agentMessage: true, originWake: { ...wake('completion', 'build'), computer: 'Build box' } }),
      queued('workflow', { agentMessage: true, originWake: wake('workflow', 'wf-1') }),
      // The person's message joined with an agent's is the person's.
      queued('joined', { originThread: thread('send') }),
    ]);
    expect(controller.incoming.map((m) => [m.item.sendId, m.kind])).toEqual([
      ['spawn', 'request'], ['ask', 'request'], ['reply', 'reply'], ['reminder', 'wake'],
      ['worker-question', 'question'], ['worker-approval', 'question'],
      ['loop', 'wake'], ['completion', 'wake'], ['workflow', 'wake'],
    ]);
  });

  it('keeps the last list through a failed read, says so and reports it', async () => {
    const { pane, controller } = await mountAgents([row('child')]);
    expect(controller.status).toBe('loaded');
    vi.mocked(reportFrontendDiagnostic).mockClear();
    setBindingMock('GetThreadAgentRows', async () => { throw new Error('row read failed'); });
    setBindingMock('ListThreadAgentRows', async () => { throw new Error('read failed'); });
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(reportFrontendDiagnostic).toHaveBeenCalled();
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
    expect(controller.status).toBe('error');
    expect(controller.error).toContain('read failed');
  });

  it('shows the segment when the first read fails, and a retry reads it again', async () => {
    vi.useFakeTimers();
    const pane = await buildPane();
    noteThread(pane.threadId!, '', 2);
    setBindingMock('ListThreadPastAgentRows', async () => ({ rows: [], total: 0 }));
    const read = setBindingMock('ListThreadAgentRows', async () => { throw new Error('ledger offline'); });
    stopEvents = setupEventListeners();
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => pane, () => Date.now());
      return controller.mount();
    });
    expect(controller.status).toBe('loading');
    await vi.advanceTimersByTimeAsync(0);
    expect(read).toHaveBeenCalledTimes(1);
    expect(controller.status).toBe('error');
    expect(controller.error).toContain('ledger offline');
    expect(controller.shown, 'a failed read looks like no agents').toBeGreaterThan(0);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [row('child')], hidden: false }));
    controller.retry();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.status).toBe('loaded');
    expect(controller.error).toBe('');
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
  });

  // v0.0.19 has no agent rows: a thread there shows no Agents segment, no
  // error and no diagnostic, and a frame does not set off another read.
  it('shows nothing for a thread on a computer without agent rows', async () => {
    vi.useFakeTimers();
    const pane = await buildPane();
    noteThread(pane.threadId!, '', 2);
    vi.mocked(reportFrontendDiagnostic).mockClear();
    answerAsV0019();
    stopEvents = setupEventListeners();
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => pane, () => Date.now());
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(500);
    expect(getBindingMock('ListThreadAgentRows')).toHaveBeenCalledTimes(1);
    expect(controller.status).toBe('loaded');
    expect(controller.error).toBe('');
    expect(controller.rows).toEqual([]);
    expect(controller.shown).toBe(0);
    expect(reportFrontendDiagnostic).not.toHaveBeenCalled();
  });

  it('reads again when its computer reconnects', async () => {
    const { read } = await mountAgents([row('child')]);
    __setBackendStatusForTest('', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null });
    await vi.advanceTimersByTimeAsync(0);
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('stops listening when unmounted', async () => {
    const { pane, read } = await mountAgents([row('child')]);
    release();
    release = () => {};
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    emitWailsEvent('transport:gap', { channel: 'thread:agent_rows_changed' }, '');
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(1);
  });
});
