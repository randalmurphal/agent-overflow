import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAgentController, type AgentController } from './activityRailAgents.svelte';
import { AGENT_ROW_SETTLED_LINGER_MS } from './activityRailBackground.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { setupEventListeners } from '../../stores/events';
import type { ThreadAgentRow } from '../../stores/bindings';
import type { UserMessageOriginWake } from '../../utils/userMessageMeta';
import { replaceQueueForThread, resetForTest as resetSendQueue, type QueueItem } from '../../stores/sendQueue.svelte';

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
    resetSendQueue();
  });

  async function mountAgents(rows: ThreadAgentRow[], hidden = false) {
    vi.useFakeTimers();
    const pane = await buildPane();
    noteThread(pane.threadId!, '', 2);
    const read = setBindingMock('ListThreadAgentRows', async () => ({ rows, hidden }));
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

  it('lingers a settled row from when it first saw it settled, and a click takes it out early', async () => {
    const { pane, controller, clock } = await mountAgents([
      row('done', { state: 'finished', settledAt: 1 }),
      row('other', { state: 'replied', settledAt: 1 }),
    ]);
    clock.now = Date.now() + AGENT_ROW_SETTLED_LINGER_MS - 1;
    expect(controller.rows.map((r) => r.token)).toEqual(['done', 'other']);
    controller.dismiss('other');
    expect(controller.rows.map((r) => r.token)).toEqual(['done']);
    clock.now = Date.now() + AGENT_ROW_SETTLED_LINGER_MS;
    expect(controller.rows).toEqual([]);
    expect(controller.shown).toBe(0);
    void pane;
  });

  it('measures the linger from the wall clock when the rail clock is stopped', async () => {
    // The rail's clock last ticked long ago and only starts again once a
    // settled row is shown.
    const { pane, controller, clock } = await mountAgents([row('child')]);
    clock.now = 0;
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [row('child', { state: 'finished', settledAt: Date.now() })], hidden: false }));
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
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [row('a'), row('b', { activity: 'Reading' })], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'b' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows[0]).toBe(a);
    expect(controller.rows[1]).not.toBe(b);
    expect(controller.rows[1].activity).toBe('Reading');
  });

  it('reads again for its own thread and a nested caller, not another thread, and after a lost frame', async () => {
    const { pane, read } = await mountAgents([row('child'), row('grand', { callerThreadId: 't-child', depth: 1 })]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'unrelated', id: 'x' });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 't-child', id: 'grand' });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(2);
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(3);
    emitWailsEvent('transport:gap', { channel: 'thread:agent_rows_changed' }, '');
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(4);
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
    const wake = (kind: UserMessageOriginWake['kind'], label: string): UserMessageOriginWake => ({ kind, label, threadId: '', computer: '' });
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

  it('keeps the last list through a failed read', async () => {
    const { pane, controller } = await mountAgents([row('child')]);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreadAgentRows', async () => { throw new Error('read failed'); });
    emitWailsEvent('thread:agent_rows_changed', { threadId: pane.threadId, id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(log).toHaveBeenCalled();
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
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
