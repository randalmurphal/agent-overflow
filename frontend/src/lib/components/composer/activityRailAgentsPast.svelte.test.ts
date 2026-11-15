import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAgentController, type AgentController } from './activityRailAgents.svelte';
import { PAST_FIRST_PAGE, PAST_MORE_PAGE } from './activityRailAgentsPast.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { setupEventListeners } from '../../stores/events';
import type { ThreadAgentRow } from '../../stores/bindings';
import { resetForTest as resetSendQueue } from '../../stores/sendQueue.svelte';

interface Cursor { settledAt: number; token: string }

function row(token: string, over: Partial<ThreadAgentRow> = {}): ThreadAgentRow {
  return {
    token, callerThreadId: 'thread-a', kind: 'spawn', threadId: `t-${token}`, title: token,
    state: 'running', createdAt: 1_000, depth: 0, ...over,
  } as ThreadAgentRow;
}

// A ledger of `count` settled rows, newest first: past-0 settled last.
function ledger(count: number, caller = 'thread-a'): ThreadAgentRow[] {
  return Array.from({ length: count }, (_, index) => row(`past-${String(index).padStart(3, '0')}`, {
    callerThreadId: caller, state: 'finished', settledAt: 1_000_000 - index,
  }));
}

// ListThreadPastAgentRows over `rows`, in the backend's page order.
function pages(rows: () => ThreadAgentRow[]) {
  return async (_id: string, after: Cursor, limit: number) => {
    const all = rows();
    const start = after.token === '' && after.settledAt === 0
      ? 0
      : all.findIndex((r) => r.token === after.token) + 1;
    const page = all.slice(start, start + limit);
    const more = start + limit < all.length;
    const last = page[page.length - 1];
    return { rows: page, total: all.length, next: more && last ? { settledAt: last.settledAt, token: last.token } : undefined };
  };
}

describe('the Agents segment\'s Past', () => {
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

  async function mount(options: { open?: ThreadAgentRow[]; past?: () => ThreadAgentRow[] } = {}) {
    vi.useFakeTimers();
    for (const id of ['thread-a', 'thread-b']) noteThread(id, '', 2);
    const open = options.open ?? [];
    const list = setBindingMock('ListThreadAgentRows', async () => ({ rows: open, hidden: false }));
    const readPast = setBindingMock('ListThreadPastAgentRows', pages(options.past ?? (() => [])));
    stopEvents = setupEventListeners();
    const current = $state({ id: 'thread-a' });
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => ({ thread: { id: current.id }, hasDraftPlaceholder: false }) as never, () => Date.now());
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    return { controller, list, readPast, current };
  }

  it('reads the total alone while collapsed, and shows the segment for a thread whose only rows are past', async () => {
    const { controller, readPast } = await mount({ past: () => ledger(3) });
    expect(readPast).toHaveBeenCalledTimes(1);
    expect(readPast).toHaveBeenCalledWith('thread-a', { settledAt: 0, token: '' }, 1);
    expect(controller.past.total).toBe(3);
    expect(controller.past.rows).toEqual([]);
    expect(controller.count).toBe(0);
    expect(controller.shown).toBe(1);
  });

  it('shows nothing for a thread with no rows open or past', async () => {
    const { controller } = await mount();
    expect(controller.past.total).toBe(0);
    expect(controller.shown).toBe(0);
  });

  it('opens on the first page, adds a page per Show more, and drops the rows when it collapses', async () => {
    const { controller, readPast } = await mount({ past: () => ledger(40) });
    readPast.mockClear();
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    expect(readPast).toHaveBeenLastCalledWith('thread-a', { settledAt: 0, token: '' }, PAST_FIRST_PAGE);
    expect(controller.past.rows.map((r) => r.token)).toEqual(ledger(PAST_FIRST_PAGE).map((r) => r.token));
    expect(controller.past.moreCount).toBe(PAST_MORE_PAGE);

    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    const last = ledger(PAST_FIRST_PAGE)[PAST_FIRST_PAGE - 1];
    expect(readPast).toHaveBeenLastCalledWith('thread-a', { settledAt: last.settledAt, token: last.token }, PAST_MORE_PAGE);
    expect(controller.past.rows).toHaveLength(PAST_FIRST_PAGE + PAST_MORE_PAGE);
    expect(controller.past.moreCount).toBe(40 - PAST_FIRST_PAGE - PAST_MORE_PAGE);

    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.token)).toEqual(ledger(40).map((r) => r.token));
    expect(controller.past.moreCount).toBe(0);

    controller.past.collapse();
    expect(controller.past.expanded).toBe(false);
    expect(controller.past.rows).toEqual([]);
    expect(controller.past.total).toBe(40);
    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(3);
  });

  it('picks up a row of its own that settles, without reading for a running row\'s activity', async () => {
    const settledRows: ThreadAgentRow[] = [];
    const { controller, readPast } = await mount({ open: [row('child')], past: () => settledRows });
    expect(controller.past.total).toBe(0);
    readPast.mockClear();
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'child', row: row('child', { activity: 'Reading' }), listed: true }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows[0].activity).toBe('Reading');
    expect(readPast, 'a running row\'s activity changed nothing past').not.toHaveBeenCalled();

    const done = row('child', { state: 'finished', settledAt: Date.now() });
    settledRows.push(done);
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'child', row: done, listed: true }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(1);
    expect(controller.past.total).toBe(1);
    // It lingers in the open list as before.
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
  });

  it('reads again for a frame about a request the list does not hold, every request, a lost frame, or a reconnect', async () => {
    const { readPast } = await mount({ open: [row('child')], past: () => ledger(2) });
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'child', row: row('child'), listed: true }]);
    readPast.mockClear();
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'gone' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(1);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(2);
    // A nested row's frame, sent to the caller and again to this thread.
    emitWailsEvent('thread:agent_rows_changed', { threadId: 't-child', id: 'grand' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(2);
    __setBackendStatusForTest('', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(3);
  });

  it('re-reads the rows it shows when the ledger changes, the new row first', async () => {
    let rows = ledger(30);
    const { controller, readPast } = await mount({ past: () => rows });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows).toHaveLength(28);
    readPast.mockClear();
    rows = [row('newest', { state: 'replied', settledAt: 2_000_000 }), ...rows];
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledWith('thread-a', { settledAt: 0, token: '' }, 28);
    // The head reaches the rows shown, so none of them goes.
    expect(controller.past.rows.map((r) => r.token)).toEqual(rows.slice(0, 29).map((r) => r.token));
    expect(controller.past.total).toBe(31);
    expect(controller.past.moreCount).toBe(2);
  });

  it('keeps rows past the backend\'s page after a head that reaches them, and drops them after one that does not', async () => {
    let rows = ledger(80);
    const { controller } = await mount({ past: () => rows });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    for (let index = 0; index < 3; index++) {
      controller.past.showMore();
      await vi.advanceTimersByTimeAsync(0);
    }
    expect(controller.past.rows).toHaveLength(68);
    rows = [row('one', { state: 'finished', settledAt: 2_000_000 }), ...rows];
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.past.rows.map((r) => r.token)).toEqual(rows.slice(0, 69).map((r) => r.token));
    expect(controller.past.moreCount).toBe(12);

    // More settled at once than one page holds: what follows the head is unread.
    const burst = Array.from({ length: 60 }, (_, index) => row(`burst-${index}`, { state: 'finished', settledAt: 3_000_000 - index }));
    rows = [...burst, ...rows];
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.past.rows.map((r) => r.token)).toEqual(burst.slice(0, 50).map((r) => r.token));
    expect(controller.past.moreCount).toBe(PAST_MORE_PAGE);
  });

  it('applies only the newest read, and reads again for a change during one', async () => {
    vi.useFakeTimers();
    noteThread('thread-a', '', 2);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    const answers: Array<(total: number) => void> = [];
    const readPast = setBindingMock('ListThreadPastAgentRows', () => new Promise((resolve) => {
      answers.push((total) => resolve({ rows: [], total }));
    }));
    stopEvents = setupEventListeners();
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => ({ thread: { id: 'thread-a' }, hasDraftPlaceholder: false }) as never, () => Date.now());
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(answers).toHaveLength(1);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(answers, 'a second read ran beside the first').toHaveLength(1);
    answers[0](4);
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.past.total).toBe(4);
    expect(readPast, 'the change during the read was not read').toHaveBeenCalledTimes(2);
    answers[1](5);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.total).toBe(5);
  });

  it('drops a read for the thread it left and the pages it held, and reads the new thread collapsed', async () => {
    const answers: Array<() => void> = [];
    const { controller, current } = await mount({ past: () => ledger(12) });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows).toHaveLength(PAST_FIRST_PAGE);
    const slow = pages(() => ledger(12));
    const readPast = setBindingMock('ListThreadPastAgentRows', (id: string, after: Cursor, limit: number) => (
      id === 'thread-a'
        ? new Promise((resolve) => { answers.push(() => resolve(slow(id, after, limit))); })
        : pages(() => ledger(2, 'thread-b'))(id, after, limit)
    ));
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(answers).toHaveLength(1);
    current.id = 'thread-b';
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.expanded).toBe(false);
    expect(controller.past.rows).toEqual([]);
    expect(readPast).toHaveBeenLastCalledWith('thread-b', { settledAt: 0, token: '' }, 1);
    expect(controller.past.total).toBe(2);
    answers[0]();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.total, 'the old thread\'s answer landed').toBe(2);
    expect(controller.past.rows).toEqual([]);
  });

  it('says why a read failed while open, keeps its rows, and clears the error on the next read', async () => {
    const { controller } = await mount({ past: () => ledger(12) });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    setBindingMock('ListThreadPastAgentRows', async () => { throw new Error('ledger offline'); });
    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.error).toContain('ledger offline');
    expect(controller.past.loadingMore).toBe(false);
    expect(controller.past.rows).toHaveLength(PAST_FIRST_PAGE);
    setBindingMock('ListThreadPastAgentRows', pages(() => ledger(12)));
    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.error).toBe('');
    expect(controller.past.rows).toHaveLength(12);
  });

  it('takes a placement into the rows it holds, open and past', async () => {
    const owned = { placement: 'owned', state: 'finished', settledAt: 5 } as const;
    const { controller } = await mount({ open: [row('w', owned)], past: () => [row('w', owned)] });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    controller.placed('t-w', 'standalone');
    expect(controller.rows[0].placement).toBe('standalone');
    expect(controller.past.rows[0].placement).toBe('standalone');
  });
});
