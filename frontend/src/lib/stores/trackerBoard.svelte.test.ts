// The tracker board store: the subscription's lifecycle and routing, the
// order of events against the subscribe answer, the one read in flight,
// patches, full reads, pages, visibility votes, refresh and the end.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import type {
  BoardPatch,
  BoardSubscription,
  BoardView,
  RowView,
  Section,
  SectionPage,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  applyTrackerBoardEvent,
  attachTrackerBoard,
  dismissTrackerRefreshNotice,
  loadMoreOfSection,
  refreshTrackerBoard,
  resyncTrackerBoardsAfterGap,
  setTrackerBoardFiltered,
  setTrackerBoardShown,
  type TrackerBoardAttachment,
  type TrackerBoardEvent,
  type TrackerBoardRef,
} from './trackerBoard.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend } from '../../test/helpers/backends';

const REF: TrackerBoardRef = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };
const SPRINT: Section = { kind: 'sprint', sprint: '8' };
const COLUMN: Section = { kind: 'column', column: 0 };

function row(id: string, order: string, title = `Issue ${id}`): RowView {
  return { order, estimate: 1, card: { id, key: `WEB-${id}`, title, status: { id: 's1' }, created: 0, updated: 0 } } as RowView;
}

function rows(from: number, count: number): RowView[] {
  return Array.from({ length: count }, (_, i) => row(String(from + i), `k${String(from + i).padStart(4, '0')}`));
}

/** A sprint board's view: one active sprint (8), or the sprint named; or
 * with `kanban` a board of one column. */
function view(pages: Array<{ section: Section; rows: RowView[]; next?: boolean }>, opts: { revision?: number; sprint?: string; kanban?: boolean } = {}): BoardView {
  const revision = opts.revision ?? 1;
  return {
    id: '1', lifetime: 1, revision, totals_revision: revision, name: 'WEB board', plans_sprints: !opts.kanban, estimate: { unit: 'points' },
    columns: [{ name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: 0, estimate: 0 } }],
    sprints: [{ id: opts.sprint ?? '8', name: 'Sprint', state: 'active', total: { rows: 1, estimate: 1 } }],
    all: { rows: 1, estimate: 1 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 100 },
    sections: pages.map((p) => ({
      section: p.section, rows: p.rows,
      next: p.next ? { order: p.rows.at(-1)!.order, issue: p.rows.at(-1)!.card.id, lifetime: 1, revision } : null,
    })),
  } as unknown as BoardView;
}

function patch(sections: RowView[][], revision: number): BoardPatch {
  return { lifetime: 1, revision, sections: sections.map((r) => ({ section: SPRINT, rows: r })) } as BoardPatch;
}

function event(seq: number, extra: Partial<TrackerBoardEvent> = {}): TrackerBoardEvent {
  return { scope: { connection: 'c1', board: '1' }, seq, sync: { read_at: 200 }, ...extra };
}

interface Mocks {
  subscribe: ReturnType<typeof vi.fn>;
  board: ReturnType<typeof vi.fn>;
  patch: ReturnType<typeof vi.fn>;
  page: ReturnType<typeof vi.fn>;
  visible: ReturnType<typeof vi.fn>;
  refresh: ReturnType<typeof vi.fn>;
  release: ReturnType<typeof vi.fn>;
  routed: Record<string, string[]>;
}

function install(answer: Partial<BoardSubscription> = {}): Mocks {
  const routed: Record<string, string[]> = {};
  const note = (name: string) => (routed[name] ??= []).push(String(takePinnedBackend() ?? ''));
  let n = 0;
  return {
    routed,
    subscribe: setBindingMock('SubscribeTrackerBoard', async () => {
      note('SubscribeTrackerBoard');
      n += 1;
      return { id: `sub-${n}`, seq: 10, loaded: true, view: view([]), poll_every: 10_000, ...answer };
    }),
    board: setBindingMock('TrackerBoard', async (_id: string, sections: Section[]) => {
      note('TrackerBoard');
      return view(sections.map((section) => ({ section, rows: [row('1', 'a')] })));
    }),
    patch: setBindingMock('TrackerBoardPatch', async () => {
      note('TrackerBoardPatch');
      return patch([[]], 2);
    }),
    page: setBindingMock('TrackerSectionRows', async () => {
      note('TrackerSectionRows');
      return { section: SPRINT, rows: [] } as unknown as SectionPage;
    }),
    visible: setBindingMock('SetTrackerSubscriptionVisible', async () => note('SetTrackerSubscriptionVisible')),
    refresh: setBindingMock('RefreshTrackerSubscription', async () => note('RefreshTrackerSubscription')),
    release: setBindingMock('ReleaseTrackerSubscription', async () => note('ReleaseTrackerSubscription')),
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

function heldIds(board: TrackerBoardAttachment): string[][] {
  return (board.current?.held?.sections ?? []).map((s) => s.rows.map((r) => r.card.id));
}

function setDocumentVisibility(value: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
  document.dispatchEvent(new Event('visibilitychange'));
}

/** A binding mock whose answers the test releases one at a time. */
function held<T>(name: string): { calls: unknown[][]; answer: (value: T) => void; fail: (err: unknown) => void } {
  const waiting: Array<{ resolve: (v: T) => void; reject: (e: unknown) => void }> = [];
  const calls: unknown[][] = [];
  setBindingMock(name, (...args: unknown[]) => {
    calls.push(args);
    return new Promise<T>((resolve, reject) => waiting.push({ resolve, reject }));
  });
  return {
    calls,
    answer: (value) => waiting.shift()!.resolve(value),
    fail: (err) => waiting.shift()!.reject(err),
  };
}

afterEach(() => {
  delete (document as unknown as Record<string, unknown>).visibilityState;
});

describe('the board subscription', () => {
  it('subscribes for the project, reads the Board screen sections, and releases on the last detach', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    const again = attachTrackerBoard(REF);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '1', []);
    // The structure names the active sprint: the screen reads it.
    expect(mocks.board).toHaveBeenCalledWith('sub-1', [SPRINT]);
    expect(heldIds(board)).toEqual([['1']]);
    expect(board.current?.readAt).toBe(100);
    again.release();
    await flush();
    expect(mocks.release).not.toHaveBeenCalled();
    board.release();
    await flush();
    expect(mocks.release).toHaveBeenCalledWith('sub-1');

    // Picked again this session, the board asks for its sections at once.
    const third = attachTrackerBoard(REF);
    await flush();
    expect(mocks.subscribe).toHaveBeenLastCalledWith('p1', 'c1', '1', [SPRINT]);
    third.release();
  });

  it('pins the subscribe and every handle call to the computer that listed the board', async () => {
    stageBackend();
    const mocks = install();
    // A column held in part, so a page is read too.
    mocks.board.mockImplementation(async () => {
      (mocks.routed.TrackerBoard ??= []).push(String(takePinnedBackend() ?? ''));
      return view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    });
    const ref = { ...REF, backend: 'laptop' };
    const board = attachTrackerBoard(ref);
    await flush();
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), 'laptop');
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await refreshTrackerBoard(board.key);
    setTrackerBoardShown(board.key, false);
    await flush();
    board.release();
    await flush();
    for (const name of ['SubscribeTrackerBoard', 'TrackerBoard', 'TrackerBoardPatch', 'TrackerSectionRows', 'RefreshTrackerSubscription', 'SetTrackerSubscriptionVisible', 'ReleaseTrackerSubscription']) {
      expect(mocks.routed[name], name).toEqual(mocks.routed[name]?.map(() => 'laptop'));
      expect(mocks.routed[name].length, name).toBeGreaterThan(0);
    }
    // A frame from another computer for the same scope is not this board's.
    mocks.patch.mockClear();
    const home = attachTrackerBoard(REF);
    await flush();
    applyTrackerBoardEvent(event(50, { changed: ['1'] }), 'laptop');
    await flush();
    expect(mocks.patch).not.toHaveBeenCalled();
    home.release();
  });

  it('says a board this computer has not read is not loaded, and reads it when the first read lands', async () => {
    const mocks = install({ loaded: false, view: null, failure: null });
    const board = attachTrackerBoard(REF);
    await flush();
    expect(board.current?.held).toBeNull();
    expect(mocks.board).not.toHaveBeenCalled();
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    // The first read gives the structure; the screen then reads its sprint.
    expect(mocks.board.mock.calls.map((c) => c[1])).toEqual([[], [SPRINT]]);
    expect(heldIds(board)).toEqual([['1']]);
    board.release();
  });

  it('drops an event the answer holds and holds one that arrives before the answer', async () => {
    install();
    const subscribe = held<BoardSubscription>('SubscribeTrackerBoard');
    const patchCall = setBindingMock('TrackerBoardPatch', async () => patch([[row('2', 'b', 'renamed')]], 2));
    const board = attachTrackerBoard(REF);
    await flush();
    applyTrackerBoardEvent(event(9, { changed: ['1'] }), HOME_BACKEND);
    applyTrackerBoardEvent(event(12, { changed: ['2'], sync: { read_at: 300, failure: { kind: 'transient', message: 'Jira did not answer', at: 300 } } }), HOME_BACKEND);
    subscribe.answer({ id: 'sub-1', seq: 10, loaded: true, view: view([{ section: SPRINT, rows: [row('1', 'a')] }]), poll_every: 10_000 } as BoardSubscription);
    await flush();
    expect(patchCall).toHaveBeenCalledTimes(1);
    expect(patchCall.mock.calls[0][2]).toEqual(['2']);
    expect(heldIds(board)).toEqual([['1', '2']]);
    expect(board.current?.failure?.message).toBe('Jira did not answer');
    expect(board.current?.readAt).toBe(300);
    // Seen again, it is dropped.
    applyTrackerBoardEvent(event(12, { changed: ['2'] }), HOME_BACKEND);
    await flush();
    expect(patchCall).toHaveBeenCalledTimes(1);
    // The failure clears with the event that says so.
    applyTrackerBoardEvent(event(13, { sync: { read_at: 400 } }), HOME_BACKEND);
    expect(board.current?.failure).toBeNull();
    board.release();
  });

  it('gathers the IDs of events that arrive during a read into the next patch', async () => {
    install();
    const board = attachTrackerBoard(REF);
    await flush();
    const patches = held<BoardPatch>('TrackerBoardPatch');
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    applyTrackerBoardEvent(event(12, { changed: ['2'] }), HOME_BACKEND);
    applyTrackerBoardEvent(event(13, { removed: ['3'], changed: ['2'] }), HOME_BACKEND);
    await flush();
    expect(patches.calls.length).toBe(1);
    patches.answer(patch([[row('1', 'a', 'one')]], 2));
    await flush();
    expect(patches.calls.length).toBe(2);
    expect(patches.calls[1][2]).toEqual(['2', '3']);
    // The first patch carried no totals: the held ones are still current.
    expect(patches.calls[1][3]).toEqual({ lifetime: 1, revision: 1 });
    patches.answer(patch([[]], 3));
    await flush();
    expect(board.current?.held?.sections[0].rows.map((r) => r.card.title)).toEqual(['one']);
    board.release();
  });

  it('reads whole on read again, past 100 IDs and after a gap, to the depth it held', async () => {
    const mocks = install();
    mocks.board.mockImplementation(async () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true }));
    mocks.page.mockImplementation(async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: COLUMN, rows: rows(from, 100), next: from + 100 < 300 ? { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } : null };
    });
    const board = attachTrackerBoard(REF);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    expect(board.current?.paging.has('column::0')).toBe(true);
    await flush();
    expect(heldIds(board)[0].length).toBe(200);
    expect(board.current?.paging.size).toBe(0);

    mocks.page.mockClear();
    applyTrackerBoardEvent(event(11, { read_again: true, changed: ['5'] }), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(2);
    expect(mocks.patch).not.toHaveBeenCalled();
    // One page brings the section back to the 200 rows held.
    expect(mocks.page).toHaveBeenCalledTimes(1);
    expect(heldIds(board)[0].length).toBe(200);

    applyTrackerBoardEvent(event(12, { changed: rows(0, 101).map((r) => r.card.id) }), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(3);
    expect(mocks.patch).not.toHaveBeenCalled();

    await resyncTrackerBoardsAfterGap(HOME_BACKEND);
    expect(mocks.board).toHaveBeenCalledTimes(4);
    expect(resyncTrackerBoardsAfterGap('elsewhere')).toBeUndefined();
    board.release();
  });

  it('reads the new active sprint when the structure changes', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    mocks.board.mockImplementationOnce(async () => view([{ section: SPRINT, rows: [] }], { sprint: '9' }));
    mocks.board.mockImplementationOnce(async () => view([{ section: { kind: 'sprint', sprint: '9' }, rows: [row('7', 'a')] }], { sprint: '9' }));
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(mocks.board.mock.calls.slice(-2).map((c) => c[1])).toEqual([[SPRINT], [{ kind: 'sprint', sprint: '9' }]]);
    expect(heldIds(board)).toEqual([['7']]);
    // The first paint's read, the read again, and one more for the sprint
    // that started between the two: no further read follows.
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(3);
    board.release();
  });

  it('reads a stale page again once the patch carries its cursor on', async () => {
    const mocks = install();
    mocks.board.mockImplementation(async () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true }));
    const board = attachTrackerBoard(REF);
    await flush();
    mocks.page.mockImplementationOnce(async () => {
      throw new TransportError('tracker_page_stale', 'the board changed since this section was read; read the section again from its start');
    });
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(1);
    expect(board.current?.paging.has('column::0')).toBe(true);
    // Nothing reads it again until the board moves past that revision.
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(1);
    applyTrackerBoardEvent(event(11, { changed: ['500'] }), HOME_BACKEND);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(2);
    expect(mocks.page.mock.calls[1][2]).toMatchObject({ revision: 2 });
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('reads every page of a section while a filter is on', async () => {
    const mocks = install();
    mocks.board.mockImplementation(async () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true }));
    mocks.page.mockImplementation(async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: COLUMN, rows: rows(from, 100), next: from + 100 < 300 ? { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } : null };
    });
    const board = attachTrackerBoard(REF);
    await flush();
    setTrackerBoardFiltered(board.key, true);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(2);
    expect(heldIds(board)[0].length).toBe(300);
    expect(board.current?.held?.sections[0].next).toBeNull();
    board.release();
  });

  it('reads a sprint whole as soon as it is held', async () => {
    const mocks = install({ view: view([{ section: SPRINT, rows: rows(0, 100), next: true }]) });
    mocks.page.mockImplementation(async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: SPRINT, rows: rows(from, 100), next: from + 100 < 300 ? { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } : null };
    });
    const board = attachTrackerBoard(REF);
    await tick();
    await tick();
    expect(board.current?.paging.has('sprint:8:0')).toBe(true);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(2);
    expect(heldIds(board)[0].length).toBe(300);
    expect(board.current?.paging.size).toBe(0);
    board.release();
  });

  it('stops at the end, and reads nothing after it', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    applyTrackerBoardEvent(event(11, { ended: true, changed: ['1'] }), HOME_BACKEND);
    applyTrackerBoardEvent(event(12, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(board.current?.ended).toBe(true);
    expect(mocks.patch).not.toHaveBeenCalled();
    expect(mocks.board).toHaveBeenCalledTimes(1);
    board.release();
  });

  it('subscribes again when its handle is gone, and on reconnect', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    mocks.patch.mockImplementationOnce(async () => {
      throw new TransportError('tracker_subscription_unknown', 'this board view is no longer open on this computer');
    });
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);

    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(3);
    expect(heldIds(board)).toEqual([['1']]);
    board.release();
  });

  it('shows a read that failed outside the tracker, and clears it with the next read', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    const failure = new TransportError('internal', 'read a patch of board 1: database is locked');
    mocks.patch.mockImplementationOnce(async () => {
      throw failure;
    });
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(board.current?.readError).toBe(failure);
    // The lost IDs are owed a full read, which the next event runs.
    applyTrackerBoardEvent(event(12, {}), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(2);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('reads the copy again on a Refresh after a read of it failed, with no event to run it', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    mocks.patch.mockImplementationOnce(async () => {
      throw new TransportError('internal', 'read a patch of board 1: database is locked');
    });
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(board.current?.readError).not.toBeNull();
    // The tracker read finds nothing new, so no event follows it.
    await refreshTrackerBoard(board.key);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(2);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('takes the failure a gap read answers, unless an event told a newer one during the read', async () => {
    const failure = { kind: 'auth', message: 'Jira refused the token', at: 50 };
    const mocks = install({ failure, view: { ...view([]), sync: { read_at: 100, failure } } as unknown as BoardView });
    let told: typeof failure | undefined = failure;
    mocks.board.mockImplementation(async (_id: string, sections: Section[]) => (
      { ...view(sections.map((section) => ({ section, rows: [row('1', 'a')] }))), sync: { read_at: 100, failure: told } }
    ));
    const board = attachTrackerBoard(REF);
    await flush();
    expect(board.current?.failure).toEqual(failure);
    told = undefined;
    // The recovery frame was lost in the gap; the read tells the board recovered.
    await resyncTrackerBoardsAfterGap(HOME_BACKEND);
    await flush();
    expect(board.current?.failure).toBeNull();

    const reads = held<BoardView>('TrackerBoard');
    const running = resyncTrackerBoardsAfterGap(HOME_BACKEND);
    await flush();
    const later = { kind: 'network', message: 'Jira could not be reached', at: 300 };
    applyTrackerBoardEvent(event(11, { sync: { read_at: 300, failure: later } }), HOME_BACKEND);
    reads.answer({ ...view([{ section: SPRINT, rows: [row('1', 'a')] }]), sync: { read_at: 250 } } as unknown as BoardView);
    await running;
    await flush();
    expect(board.current?.failure).toEqual(later);
    board.release();
  });
});

describe('visibility', () => {
  it('votes the board down while the pane or the window is hidden and up when both show', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    expect(mocks.visible).not.toHaveBeenCalled();
    setTrackerBoardShown(board.key, false);
    await flush();
    setDocumentVisibility('hidden');
    setTrackerBoardShown(board.key, true);
    await flush();
    setDocumentVisibility('visible');
    await flush();
    expect(mocks.visible.mock.calls).toEqual([['sub-1', false], ['sub-1', true]]);
    board.release();
  });

  it('subscribes a board the pane hides with its vote down', async () => {
    const mocks = install();
    const key = attachTrackerBoard(REF);
    setTrackerBoardShown(key.key, false);
    await flush();
    expect(mocks.visible.mock.calls).toEqual([['sub-1', false]]);
    key.release();
  });
});

describe('refresh', () => {
  it('spins only while its call is out and shows its two bounded answers as they are', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF);
    await flush();
    const calls = held<void>('RefreshTrackerSubscription');
    const running = refreshTrackerBoard(board.key);
    expect(board.current?.refreshing).toBe(true);
    calls.answer();
    await running;
    expect(board.current?.refreshing).toBe(false);
    expect(board.current?.refreshNotice).toBeNull();

    const notRun = 'the board was busy with an earlier read from the tracker, so this refresh did not run; refresh again';
    const second = refreshTrackerBoard(board.key);
    calls.fail(new TransportError('tracker_refresh_not_run', notRun));
    await second;
    expect(board.current?.refreshNotice).toEqual({ kind: 'not_run', message: notRun });
    dismissTrackerRefreshNotice(board.key);
    expect(board.current?.refreshNotice).toBeNull();

    const waiting = 'this refresh was still reading from the tracker when its answer was due; what it finds shows as it lands';
    const third = refreshTrackerBoard(board.key);
    calls.fail(new TransportError('tracker_refresh_waiting', waiting));
    await third;
    expect(board.current?.refreshNotice).toEqual({ kind: 'waiting', message: waiting });
    // The read it left running lands.
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    expect(board.current?.refreshNotice).toBeNull();

    const broken = new TransportError('internal', 'refresh: database is locked');
    const fourth = refreshTrackerBoard(board.key);
    calls.fail(broken);
    await fourth;
    expect(board.current?.refreshNotice).toEqual({ kind: 'failed', error: broken });
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    board.release();
  });
});
