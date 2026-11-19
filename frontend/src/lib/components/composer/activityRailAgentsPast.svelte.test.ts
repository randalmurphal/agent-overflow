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
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

vi.mock('../../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

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
    expect(readPast).toHaveBeenCalledWith('thread-a', { settledAt: 0, token: '' }, 50);
    expect(readPast, 'one page reached the oldest row shown').toHaveBeenCalledTimes(1);
    // Every row shown stays, below the new one.
    expect(controller.past.rows.map((r) => r.token)).toEqual(rows.slice(0, 29).map((r) => r.token));
    expect(controller.past.total).toBe(31);
    expect(controller.past.moreCount).toBe(2);
  });

  it('rereads every row it shows through cursor pages, past the backend\'s page, and keeps them after a burst', async () => {
    let rows = ledger(80);
    const { controller, readPast } = await mount({ past: () => rows });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    for (let index = 0; index < 3; index++) {
      controller.past.showMore();
      await vi.advanceTimersByTimeAsync(0);
    }
    expect(controller.past.rows).toHaveLength(68);

    // A row beyond the first page changes: its late reply is read.
    rows = rows.map((r, index) => (index === 60 ? { ...r, lateReply: true, answer: 'late' } : r));
    readPast.mockClear();
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.past.rows[60].answer, 'the row past the first page was not reread').toBe('late');
    expect(readPast.mock.calls.map((call) => call[2])).toEqual([50, 50]);
    expect(controller.past.moreCount).toBe(12);

    // More settled at once than one page holds: the rows it showed stay below them.
    const burst = Array.from({ length: 60 }, (_, index) => row(`burst-${index}`, { state: 'finished', settledAt: 3_000_000 - index }));
    rows = [...burst, ...rows];
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.past.rows.map((r) => r.token)).toEqual(rows.slice(0, 128).map((r) => r.token));
    expect(controller.past.total).toBe(140);
    expect(controller.past.moreCount).toBe(12);
    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.token)).toEqual(rows.map((r) => r.token));
  });

  it('takes a late reply to a row the open list still lingers on into Past, by the open list\'s read', async () => {
    const done = row('child', { callerThreadId: 'thread-a', state: 'finished', settledAt: 900, answer: 'first' });
    let settledRows = [done];
    const { controller, readPast } = await mount({ open: [done], past: () => settledRows });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.answer)).toEqual(['first']);
    const late = { ...done, lateReply: true, answer: 'late' };
    settledRows = [late];
    readPast.mockClear();
    const readRows = setBindingMock('GetThreadAgentRows', async () => [{ token: 'child', row: late, listed: true }]);
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readRows).toHaveBeenCalledTimes(1);
    expect(controller.past.rows[0].lateReply, 'Past kept the copy from before the late reply').toBe(true);
    expect(controller.past.rows[0].answer).toBe('late');
    expect(readPast, 'the open list\'s read already said it').not.toHaveBeenCalled();
  });

  it('reads Past for a frame about a lingering row that the open list\'s read no longer lists', async () => {
    const done = row('child', { callerThreadId: 'thread-a', state: 'finished', settledAt: 900 });
    const { readPast } = await mount({ open: [done], past: () => [done] });
    setBindingMock('GetThreadAgentRows', async () => [{ token: 'child', listed: false }]);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    readPast.mockClear();
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'child' });
    await vi.advanceTimersByTimeAsync(500);
    expect(readPast).toHaveBeenCalledTimes(1);
  });

  // A worker continued after its spawn settled: the row the open list
  // holds settled reads open again, and leaves Past.
  function resume(done: ThreadAgentRow, worker: ThreadAgentRow['worker'] = 'running'): ThreadAgentRow {
    const open = { ...done, worker };
    setBindingMock('GetThreadAgentRows', async () => [{ token: done.token, row: open, listed: true }]);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [open], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: done.token });
    return open;
  }

  // Its worker rests again: the open list reads it settled.
  function rest(done: ThreadAgentRow): void {
    setBindingMock('GetThreadAgentRows', async () => [{ token: done.token, row: done, listed: true }]);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [done], hidden: false }));
    emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: done.token });
  }

  it('takes a worker continued after its spawn settled out of an open Past, so it shows once', async () => {
    const done = row('child', { state: 'finished', settledAt: 2_000_000 });
    let ledgerRows = [done, ...ledger(2)];
    const { controller, readPast } = await mount({ open: [done], past: () => ledgerRows });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.token)).toContain('child');
    expect(controller.rows.map((r) => r.token)).not.toContain('child');

    ledgerRows = ledger(2);
    readPast.mockClear();
    let releasePast!: () => void;
    const answer = pages(() => ledgerRows);
    readPast.mockImplementation((id: unknown, after: unknown, limit: unknown) => new Promise((resolve) => {
      releasePast = () => resolve(answer(id as string, after as Cursor, limit as number));
    }));
    resume(done);
    await vi.advanceTimersByTimeAsync(500);
    // The open list's read alone, before Past's own read lands.
    expect(controller.past.rows.map((r) => r.token), 'the reopened worker stayed in Past').not.toContain('child');
    expect(controller.past.total).toBe(2);
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
    expect(controller.rows[0].worker).toBe('running');
    expect(controller.count).toBe(1);
    expect(readPast, 'Past was not read again for the reopened row').toHaveBeenCalled();
    releasePast();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.token)).toEqual(ledger(2).map((r) => r.token));
    expect(controller.past.total).toBe(2);
  });

  it('corrects a collapsed Past\'s count when a worker it counted is continued', async () => {
    const done = row('child', { state: 'finished', settledAt: 900 });
    let ledgerRows = [done];
    const { controller } = await mount({ open: [done], past: () => ledgerRows });
    expect(controller.past.total).toBe(1);
    ledgerRows = [];
    resume(done);
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.past.total, 'the collapsed count kept the reopened worker').toBe(0);
    expect(controller.rows.map((r) => r.token)).toEqual(['child']);
  });

  it('moves a worker between the lists as it rests and resumes again, never in both', async () => {
    const done = row('child', { state: 'finished', settledAt: 900 });
    let ledgerRows = [done];
    const { controller } = await mount({ open: [done], past: () => ledgerRows });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    for (let round = 0; round < 3; round++) {
      ledgerRows = [];
      resume(done, round % 2 === 0 ? 'running' : 'waiting');
      await vi.advanceTimersByTimeAsync(500);
      expect(controller.past.rows.map((r) => r.token), `round ${round}: resumed`).toEqual([]);
      expect(controller.past.total).toBe(0);
      expect(controller.rows.map((r) => r.token)).toEqual(['child']);

      ledgerRows = [done];
      rest(done);
      await vi.advanceTimersByTimeAsync(500);
      expect(controller.past.rows.map((r) => r.token), `round ${round}: rested`).toEqual(['child']);
      expect(controller.past.total).toBe(1);
      expect(controller.rows.map((r) => r.token)).toEqual([]);
    }
  });

  it('drops a Show more that answers after Past collapsed, and opens again on the first page', async () => {
    const { controller, readPast } = await mount({ past: () => ledger(40) });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    const held: Array<() => void> = [];
    const answer = pages(() => ledger(40));
    readPast.mockImplementation((id: unknown, after: unknown, limit: unknown) => new Promise((resolve) => {
      held.push(() => resolve(answer(id as string, after as Cursor, limit as number)));
    }));
    controller.past.showMore();
    await vi.advanceTimersByTimeAsync(0);
    expect(held).toHaveLength(1);
    controller.past.collapse();
    held[0]();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows, 'the reply landed in the collapsed Past').toEqual([]);
    expect(controller.past.moreCount).toBe(0);
    readPast.mockImplementation(answer as never);
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.token)).toEqual(ledger(PAST_FIRST_PAGE).map((r) => r.token));
    expect(controller.past.moreCount).toBe(PAST_MORE_PAGE);
  });

  it('says a failed first read of the count, keeps Past reachable, reports it, and recovers on retry', async () => {
    vi.mocked(reportFrontendDiagnostic).mockClear();
    vi.useFakeTimers();
    noteThread('thread-a', '', 2);
    setBindingMock('ListThreadAgentRows', async () => ({ rows: [], hidden: false }));
    setBindingMock('ListThreadPastAgentRows', async () => { throw new Error('ledger offline'); });
    stopEvents = setupEventListeners();
    let controller!: AgentController;
    release = $effect.root(() => {
      controller = createAgentController(() => ({ thread: { id: 'thread-a' }, hasDraftPlaceholder: false }) as never, () => Date.now());
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.error).toContain('ledger offline');
    expect(controller.past.status).toBe('error');
    expect(controller.shown, 'the segment went away with the failed read').toBeGreaterThan(0);
    expect(reportFrontendDiagnostic).toHaveBeenCalled();
    setBindingMock('ListThreadPastAgentRows', pages(() => ledger(3)));
    controller.past.retry();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.error).toBe('');
    expect(controller.past.status).toBe('loaded');
    expect(controller.past.total).toBe(3);
  });

  it('lists a settled row once: Past open takes it out of the open rows, and closing Past gives it back', async () => {
    const done = row('child', { callerThreadId: 'thread-a', state: 'finished', settledAt: Date.now() });
    const { controller } = await mount({ open: [done, row('other', { callerThreadId: 'thread-a' })], past: () => [done] });
    expect(controller.rows.map((r) => r.token)).toEqual(['child', 'other']);
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.past.rows.map((r) => r.token)).toEqual(['child']);
    expect(controller.rows.map((r) => r.token), 'listed twice').toEqual(['other']);
    controller.past.collapse();
    expect(controller.rows.map((r) => r.token)).toEqual(['child', 'other']);
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
    // A send to the worker is open while its spawn is past.
    const { controller } = await mount({
      open: [row('w', { kind: 'send', placement: 'owned' })],
      past: () => [row('spawn', { threadId: 't-w', placement: 'owned', state: 'finished', settledAt: 5 })],
    });
    controller.past.toggle();
    await vi.advanceTimersByTimeAsync(0);
    controller.placed('t-w', 'standalone', '');
    expect(controller.rows[0].placement).toBe('standalone');
    expect(controller.past.rows[0].placement).toBe('standalone');
  });

  // Past has two inputs: its own page reads and the rows and placements
  // that land while one is out. A page read answers for the ledger as it
  // stood when the read started, so it never installs a row older than a
  // patch or a placement that arrived after that.
  describe('a page read held while a newer row or placement arrives', () => {
    // Holds every Past read until released, answering from `rows` as it
    // stands when the read started.
    function holdPages(readPast: ReturnType<typeof setBindingMock>, rows: () => ThreadAgentRow[]) {
      const held: Array<() => void> = [];
      readPast.mockImplementation((id: unknown, after: unknown, limit: unknown) => {
        const answer = pages(() => rows())(id as string, after as Cursor, limit as number);
        return new Promise((resolve) => { held.push(() => resolve(answer)); });
      });
      return held;
    }

    async function releaseAll(held: Array<() => void>) {
      while (held.length > 0) {
        held.shift()!();
        await vi.advanceTimersByTimeAsync(0);
      }
    }

    // A late reply to `row`, read by the open list.
    function lateReply(done: ThreadAgentRow) {
      const late = { ...done, lateReply: true, answer: 'late' };
      setBindingMock('GetThreadAgentRows', async () => [{ token: done.token, row: late, listed: true }]);
      setBindingMock('ListThreadAgentRows', async () => ({ rows: [late], hidden: false }));
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: done.token });
    }

    it('keeps a late reply that arrives before the first page lands', async () => {
      const done = row('child', { callerThreadId: 'thread-a', state: 'finished', settledAt: 900, answer: 'first' });
      let ledgerRows = [done];
      const { controller, readPast } = await mount({ open: [done], past: () => ledgerRows });
      const held = holdPages(readPast, () => ledgerRows);
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      expect(held).toHaveLength(1);
      lateReply(done);
      ledgerRows = [{ ...done, lateReply: true, answer: 'late' }];
      await vi.advanceTimersByTimeAsync(500);
      await releaseAll(held);
      expect(controller.past.rows.map((r) => r.answer), 'the page read before the reply wrote it back').toEqual(['late']);
      expect(controller.past.rows[0].lateReply).toBe(true);
    });

    it('keeps a late reply to a row on a loaded second page through a reread of every page', async () => {
      const rows = ledger(30).map((r) => ({ ...r, answer: 'first' }));
      const target = rows[20];
      let ledgerRows = rows;
      const { controller, readPast } = await mount({ open: [target], past: () => ledgerRows });
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      controller.past.showMore();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.past.rows).toHaveLength(28);
      const held = holdPages(readPast, () => ledgerRows);
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
      await vi.advanceTimersByTimeAsync(500);
      expect(held.length).toBeGreaterThan(0);
      lateReply(target);
      ledgerRows = rows.map((r) => (r.token === target.token ? { ...r, lateReply: true, answer: 'late' } : r));
      await vi.advanceTimersByTimeAsync(500);
      await releaseAll(held);
      expect(controller.past.rows).toHaveLength(28);
      expect(controller.past.rows[20].answer, 'the reread installed the answer from before the reply').toBe('late');
    });

    it('keeps a late reply to a row a held Show more brings in', async () => {
      const rows = ledger(30).map((r) => ({ ...r, answer: 'first' }));
      const target = rows[12];
      let ledgerRows = rows;
      const { controller, readPast } = await mount({ open: [target], past: () => ledgerRows });
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      const held = holdPages(readPast, () => ledgerRows);
      controller.past.showMore();
      await vi.advanceTimersByTimeAsync(0);
      expect(held).toHaveLength(1);
      lateReply(target);
      ledgerRows = rows.map((r) => (r.token === target.token ? { ...r, lateReply: true, answer: 'late' } : r));
      await vi.advanceTimersByTimeAsync(500);
      await releaseAll(held);
      expect(controller.past.rows).toHaveLength(28);
      expect(controller.past.rows[12].answer).toBe('late');
    });

    it('keeps a page read over an open-list read that started before it and lands after it', async () => {
      const done = row('child', { callerThreadId: 'thread-a', state: 'finished', settledAt: 900, answer: 'first' });
      let ledgerRows = [done];
      const { controller, readPast } = await mount({ open: [done], past: () => ledgerRows });
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      // The open list's row read starts, and is held, with the old answer.
      let releaseRows!: () => void;
      setBindingMock('GetThreadAgentRows', () => new Promise((resolve) => {
        releaseRows = () => resolve([{ token: 'child', row: done, listed: true }]);
      }));
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'child' });
      await vi.advanceTimersByTimeAsync(500);
      // Then the reply lands and Past reads it.
      ledgerRows = [{ ...done, lateReply: true, answer: 'late' }];
      readPast.mockClear();
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
      await vi.advanceTimersByTimeAsync(500);
      expect(readPast).toHaveBeenCalled();
      expect(controller.past.rows[0].answer).toBe('late');
      releaseRows();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.past.rows[0].answer, 'the older open-list read wrote its answer back').toBe('late');
    });

    it('leaves out a worker continued while a page that holds it is out, first page or Show more', async () => {
      const done = row('child', { state: 'finished', settledAt: 2_000_000 });
      let ledgerRows = [done, ...ledger(2)];
      const { controller, readPast } = await mount({ open: [done], past: () => ledgerRows });
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      // A reread starts while the ledger still holds the row settled.
      const held = holdPages(readPast, () => ledgerRows);
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
      await vi.advanceTimersByTimeAsync(500);
      expect(held).toHaveLength(1);
      ledgerRows = ledger(2);
      resume(done);
      await vi.advanceTimersByTimeAsync(500);
      // The reread that started before the resume lands first.
      held.shift()!();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.past.rows.map((r) => r.token), 'the page read before the resume put it back').toEqual(ledger(2).map((r) => r.token));
      expect(controller.past.total, 'the page read before the resume counted it').toBe(2);
      expect(controller.rows.map((r) => r.token)).toEqual(['child']);
      await vi.advanceTimersByTimeAsync(500);
      await releaseAll(held);
      expect(controller.past.rows.map((r) => r.token)).toEqual(ledger(2).map((r) => r.token));
      expect(controller.past.total).toBe(2);
    });

    it('leaves out a worker continued while a Show more that holds it is out', async () => {
      const rows = ledger(30);
      const target = rows[12];
      let ledgerRows = rows;
      const { controller, readPast } = await mount({ open: [target], past: () => ledgerRows });
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      const held = holdPages(readPast, () => ledgerRows);
      controller.past.showMore();
      await vi.advanceTimersByTimeAsync(0);
      expect(held).toHaveLength(1);
      ledgerRows = rows.filter((r) => r.token !== target.token);
      resume(target);
      await vi.advanceTimersByTimeAsync(500);
      // The Show more that started before the resume lands first.
      held.shift()!();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.past.rows.map((r) => r.token), 'the held Show more put it back').not.toContain(target.token);
      expect(controller.past.rows).toHaveLength(27);
      expect(controller.past.total, 'the held Show more counted it').toBe(29);
      expect(controller.rows.map((r) => r.token)).toEqual([target.token]);
      await vi.advanceTimersByTimeAsync(500);
      await releaseAll(held);
      expect(controller.past.rows.map((r) => r.token)).not.toContain(target.token);
      expect(controller.past.total).toBe(29);
    });

    it('keeps a placement over an open-list read that started before it', async () => {
      const done = row('child', { callerThreadId: 'thread-a', placement: 'owned', state: 'finished', settledAt: 900, answer: 'first' });
      const { controller } = await mount({ open: [done], past: () => [done] });
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      let releaseRows!: () => void;
      setBindingMock('GetThreadAgentRows', () => new Promise((resolve) => {
        releaseRows = () => resolve([{ token: 'child', row: { ...done, answer: 'late' }, listed: true }]);
      }));
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a', id: 'child' });
      await vi.advanceTimersByTimeAsync(500);
      controller.placed('t-child', 'standalone', '');
      releaseRows();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.past.rows[0].answer).toBe('late');
      expect(controller.past.rows[0].placement, 'the older read undid Show in Sidebar').toBe('standalone');
    });

    it('keeps a placement made while the first page is out, and while a reread of every page is out', async () => {
      const spawn = (token: string, settledAt: number) => row(token, { threadId: `t-${token}`, placement: 'owned', state: 'finished', settledAt });
      const rows = Array.from({ length: 30 }, (_, index) => spawn(`p${String(index).padStart(2, '0')}`, 1_000_000 - index));
      const { controller, readPast } = await mount({ past: () => rows });
      let held = holdPages(readPast, () => rows);
      controller.past.toggle();
      await vi.advanceTimersByTimeAsync(0);
      controller.placed('t-p03', 'standalone', '');
      await releaseAll(held);
      expect(controller.past.rows[3].placement, 'the first page undid Show in Sidebar').toBe('standalone');

      readPast.mockImplementation(pages(() => rows) as never);
      controller.past.showMore();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.past.rows).toHaveLength(28);
      held = holdPages(readPast, () => rows);
      emitWailsEvent('thread:agent_rows_changed', { threadId: 'thread-a' });
      await vi.advanceTimersByTimeAsync(500);
      expect(held.length).toBeGreaterThan(0);
      controller.placed('t-p21', 'standalone', '');
      await releaseAll(held);
      expect(controller.past.rows[21].placement, 'the reread undid Show in Sidebar').toBe('standalone');
      // What the thread's computer said stands over every later read too,
      // which relays another computer's word.
      expect(controller.past.rows[3].placement).toBe('standalone');
    });
  });
});
