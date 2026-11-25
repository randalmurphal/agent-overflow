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
  trackerBoardUpdated as applyTrackerBoardEvent,
  trackerBoardsGap as resyncTrackerBoardsAfterGap,
  type TrackerBoardEvent,
} from './trackerBoardEvents';
import {
  attachTrackerBoard,
  dismissTrackerRefreshNotice,
  loadMoreOfSection,
  refreshTrackerBoard,
  renewEndedTrackerBoard,
  setTrackerBoardFiltered,
  setTrackerBoardShown,
  trackerBoardShowsIssue,
  trackerBoardsSettled,
  type TrackerBoardAttachment,
  type TrackerBoardRef,
} from './trackerBoard.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { DEFAULT_RETRY } from './entityStore.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { DisconnectedError, TransportError } from '../transport/wsClient';
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
 * with `kanban` a board of `columns` columns (one). The Board screen is the
 * pages' sections unless `screen` names it. */
function view(
  pages: Array<{ section: Section; rows: RowView[]; next?: boolean }>,
  opts: { revision?: number; sprint?: string; kanban?: boolean; columns?: number; screen?: Section[] } = {},
): BoardView {
  const revision = opts.revision ?? 1;
  return {
    id: '1', lifetime: 1, revision, totals_revision: revision, name: 'WEB board', plans_sprints: !opts.kanban, estimate: { unit: 'points' },
    columns: Array.from({ length: opts.columns ?? 1 }, (_, i) => ({ name: i === 0 ? 'To Do' : `Column ${i}`, statuses: i === 0 ? [{ id: 's1', group: 'todo' }] : [], total: { rows: 0, estimate: 0 } })),
    sprints: [{ id: opts.sprint ?? '8', name: 'Sprint', state: 'active', total: { rows: 1, estimate: 1 } }],
    all: { rows: 1, estimate: 1 }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 100 },
    screen: opts.screen ?? pages.map((p) => p.section),
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
      return { id: `sub-${n}`, seq: 10, loaded: true, view: view([{ section: SPRINT, rows: [row('1', 'a')] }]), poll_every: 10_000, ...answer };
    }),
    // The Board screen of the sprint board: its active sprint.
    board: setBindingMock('TrackerBoard', async () => {
      note('TrackerBoard');
      return view([{ section: SPRINT, rows: [row('1', 'a')] }]);
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
  it('subscribes for the project with the Board screen\'s cards in the answer, and releases on the last detach', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
    const again = attachTrackerBoard(REF, true);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    // No sections asks for the screen's; the answer holds its cards, so no
    // full read follows.
    expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '1', [], true);
    expect(mocks.board).not.toHaveBeenCalled();
    expect(heldIds(board)).toEqual([['1']]);
    expect(board.current?.readAt).toBe(100);
    again.release();
    await flush();
    expect(mocks.release).not.toHaveBeenCalled();
    board.release();
    await flush();
    expect(mocks.release).toHaveBeenCalledWith('sub-1');

    // Picked again, the board asks for the screen's again.
    const third = attachTrackerBoard(REF, true);
    await flush();
    expect(mocks.subscribe).toHaveBeenLastCalledWith('p1', 'c1', '1', [], true);
    expect(mocks.board).not.toHaveBeenCalled();
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
    const board = attachTrackerBoard(ref, true);
    await flush();
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), 'laptop');
    await flush();
    applyTrackerBoardEvent(event(12, { read_again: true }), 'laptop');
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
    const home = attachTrackerBoard(REF, true);
    await flush();
    applyTrackerBoardEvent(event(50, { changed: ['1'] }), 'laptop');
    await flush();
    expect(mocks.patch).not.toHaveBeenCalled();
    home.release();
  });

  it('says a board this computer has not read is not loaded, and reads it when the first read lands', async () => {
    const mocks = install({ loaded: false, view: null, failure: null });
    const board = attachTrackerBoard(REF, true);
    await flush();
    expect(board.current?.held).toBeNull();
    expect(mocks.board).not.toHaveBeenCalled();
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    // The first read answers the screen with its cards.
    expect(mocks.board.mock.calls.map((c) => c[1])).toEqual([[]]);
    expect(heldIds(board)).toEqual([['1']]);
    board.release();
  });

  it('drops an event the answer holds and holds one that arrives before the answer', async () => {
    install();
    const subscribe = held<BoardSubscription>('SubscribeTrackerBoard');
    const patchCall = setBindingMock('TrackerBoardPatch', async () => patch([[row('2', 'b', 'renamed')]], 2));
    const board = attachTrackerBoard(REF, true);
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
    const board = attachTrackerBoard(REF, true);
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
    expect(patches.calls[1][3]).toEqual({ lifetime: 1, revision: 1, groups: '' });
    patches.answer(patch([[]], 3));
    await flush();
    expect(board.current?.held?.sections[0].rows.map((r) => r.card.title)).toEqual(['one']);
    board.release();
  });

  it("sends the project's status groups the board was read with, and after a refused patch the new ones", async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
    await flush();
    // A save of the project's status groups refuses the patch of sections
    // read before it; the board is read again, under the new groups.
    mocks.patch.mockImplementationOnce(async () => {
      throw new TransportError('tracker_page_stale', "the board, or the project's status groups, changed since this section was read; read the section again from its start");
    });
    mocks.board.mockImplementation(async () => ({ ...view([{ section: SPRINT, rows: [row('1', 'a')] }]), groups: 'g2' }));
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(mocks.patch.mock.calls[0][3]).toEqual({ lifetime: 1, revision: 1, groups: '' });
    expect(mocks.board).toHaveBeenCalledTimes(1);
    expect(board.current?.readError).toBeNull();
    expect(board.current?.held?.groups).toBe('g2');
    applyTrackerBoardEvent(event(12, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(mocks.patch.mock.calls[1][3]).toEqual({ lifetime: 1, revision: 1, groups: 'g2' });
    // A patch it applies keeps them.
    expect(board.current?.held?.groups).toBe('g2');
    board.release();
  });

  it('leaves a page read that timed out for Refresh, so a column nearing its end does not ask again and again', async () => {
    const mocks = install({ view: view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true }) });
    mocks.page.mockImplementation(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(board.current?.sectionErrors.get('column::0')).toBeInstanceOf(TransportError);
    expect(board.current?.paging.size).toBe(0);
    // The column stopped loading and asks for its next page again.
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(1);
    board.release();
  });

  it("keeps reading the other columns after one column's page read timed out", async () => {
    const OTHER: Section = { kind: 'column', column: 1 };
    const mocks = install({
      view: view([{ section: COLUMN, rows: rows(0, 100), next: true }, { section: OTHER, rows: rows(1000, 100), next: true }], { kanban: true, columns: 2 }),
    });
    mocks.page.mockImplementation(async (_id: string, section: Section) => {
      if (section.column === 0) throw new TransportError('timeout', 'the computer did not answer in time');
      return { section, rows: rows(1100, 50), next: null };
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    // The reader scrolls to the end of the other column.
    loadMoreOfSection(board.key, OTHER);
    await flush();
    expect(heldIds(board)[1]).toHaveLength(150);
    expect(board.current?.paging.size).toBe(0);
    expect([...board.current!.sectionErrors.keys()]).toEqual(['column::0']);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it("goes on to a wide board's other columns after an unread one's first page timed out", async () => {
    const mocks = install();
    const screen: Section[] = Array.from({ length: 21 }, (_, i) => ({ kind: 'column', column: i }));
    mocks.subscribe.mockImplementationOnce(async () => ({
      id: 'sub-1', seq: 10, loaded: true, poll_every: 10_000,
      view: view(screen.slice(0, 20).map((section, i) => ({ section, rows: i === 0 ? rows(0, 100) : [], next: i === 0 })), { kanban: true, columns: 21, screen }),
    }));
    mocks.page.mockImplementation(async (_id: string, section: Section) => {
      if (section.column === 20) throw new TransportError('timeout', 'the computer did not answer in time');
      return { section, rows: rows(100, 50), next: null };
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(heldIds(board)[0]).toHaveLength(150);
    expect([...board.current!.sectionErrors.keys()]).toEqual(['column::20']);
    board.release();
  });

  it('keeps the page a filter asked for when it times out, and reads it on Refresh after the filter is cleared', async () => {
    const mocks = install({ view: view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true }) });
    let fail = true;
    mocks.page.mockImplementation(async (_id: string, section: Section) => {
      if (fail) throw new TransportError('timeout', 'the computer did not answer in time');
      return { section, rows: rows(100, 50), next: null };
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    setTrackerBoardFiltered(board.key, true);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(1);
    // Nothing is being read: nothing says the column is loading.
    expect(board.current?.paging.size).toBe(0);
    setTrackerBoardFiltered(board.key, false);
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    // The scroll alone does not read the failed page again.
    expect(mocks.page).toHaveBeenCalledTimes(1);
    fail = false;
    await refreshTrackerBoard(board.key);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(2);
    expect(heldIds(board)[0]).toHaveLength(150);
    expect(board.current?.sectionErrors.size).toBe(0);
    board.release();
  });

  it("reads a column's failed page again with the next event from the tracker", async () => {
    const mocks = install({ view: view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true }) });
    mocks.patch.mockImplementation(async () => ({ lifetime: 1, revision: 2, sections: [{ section: COLUMN, rows: [] }] }));
    mocks.page.mockImplementationOnce(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    mocks.page.mockImplementation(async (_id: string, section: Section) => ({ section, rows: rows(100, 50), next: null }));
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(board.current?.sectionErrors.has('column::0')).toBe(true);
    // A change to a card the board does not hold.
    applyTrackerBoardEvent(event(11, { changed: ['900'] }), HOME_BACKEND);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(2);
    expect(heldIds(board)[0]).toHaveLength(150);
    expect(board.current?.sectionErrors.size).toBe(0);
    board.release();
  });

  it('leaves a failed recovery read for Refresh when a column is scrolled, and reads the scroll after it', async () => {
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    mocks.page.mockImplementation(async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: COLUMN, rows: rows(from, 100), next: { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } };
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(heldIds(board)[0]).toHaveLength(200);
    // A gap's recovery fails while it reads the column back to its depth.
    mocks.page.mockImplementationOnce(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    await resyncTrackerBoardsAfterGap(HOME_BACKEND);
    expect(board.current?.readError).toBeInstanceOf(TransportError);
    mocks.board.mockClear();
    mocks.page.mockClear();
    // The reader scrolls near the end of the column, again and again.
    for (let i = 0; i < 3; i += 1) {
      loadMoreOfSection(board.key, COLUMN);
      await flush();
    }
    expect(mocks.board).not.toHaveBeenCalled();
    expect(mocks.page).not.toHaveBeenCalled();
    expect(board.current?.paging.size).toBe(0);
    // Refresh reads the board whole, back to its depth, then the page asked for.
    await refreshTrackerBoard(board.key);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(1);
    expect(heldIds(board)[0]).toHaveLength(300);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('reads a failed patch again with each next update, and with Refresh', async () => {
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    mocks.patch.mockImplementation(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    mocks.board.mockImplementationOnce(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    applyTrackerBoardEvent(event(11, { changed: ['5'] }), HOME_BACKEND);
    await flush();
    // The patch failed; the full read it owes waits for the next update.
    expect(mocks.patch).toHaveBeenCalledTimes(1);
    expect(mocks.board).not.toHaveBeenCalled();
    expect(board.current?.readError).toBeInstanceOf(TransportError);
    applyTrackerBoardEvent(event(12, { changed: ['6'] }), HOME_BACKEND);
    await flush();
    // That full read failed too; the next update reads it again.
    expect(mocks.board).toHaveBeenCalledTimes(1);
    applyTrackerBoardEvent(event(13, { changed: ['7'] }), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(2);
    expect(board.current?.readError).toBeNull();

    mocks.board.mockImplementationOnce(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    applyTrackerBoardEvent(event(14, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(board.current?.readError).toBeInstanceOf(TransportError);
    await refreshTrackerBoard(board.key);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(4);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('reads whole again on Refresh when a full read failed reading a section back to its depth', async () => {
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    const page = async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: COLUMN, rows: rows(from, 100), next: { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } };
    };
    mocks.page.mockImplementation(page);
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(heldIds(board)[0].length).toBe(200);

    mocks.page.mockImplementationOnce(async () => {
      throw new TransportError('timeout', 'the computer did not answer in time');
    });
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(1);
    expect(board.current?.readError).toBeInstanceOf(TransportError);

    await refreshTrackerBoard(board.key);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(2);
    expect(heldIds(board)[0].length).toBe(200);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('reads whole on read again, past 100 IDs and after a gap, to the depth it held', async () => {
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    mocks.page.mockImplementation(async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: COLUMN, rows: rows(from, 100), next: from + 100 < 300 ? { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } : null };
    });
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, COLUMN);
    expect(board.current?.paging.has('column::0')).toBe(true);
    await flush();
    expect(heldIds(board)[0].length).toBe(200);
    expect(board.current?.paging.size).toBe(0);

    mocks.page.mockClear();
    applyTrackerBoardEvent(event(11, { read_again: true, changed: ['5'] }), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(1);
    expect(mocks.patch).not.toHaveBeenCalled();
    // One page brings the section back to the 200 rows held.
    expect(mocks.page).toHaveBeenCalledTimes(1);
    expect(heldIds(board)[0].length).toBe(200);

    applyTrackerBoardEvent(event(12, { changed: rows(0, 101).map((r) => r.card.id) }), HOME_BACKEND);
    await flush();
    expect(mocks.board).toHaveBeenCalledTimes(2);
    expect(mocks.patch).not.toHaveBeenCalled();

    await resyncTrackerBoardsAfterGap(HOME_BACKEND);
    expect(mocks.board).toHaveBeenCalledTimes(3);
    expect(resyncTrackerBoardsAfterGap('elsewhere')).toBeUndefined();
    board.release();
  });

  it('ends a sprint start and a column change each in one full read that asks for the screen again', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
    await flush();
    // Sprint 9 started in place of 8: the computer answers its screen.
    const nine: Section = { kind: 'sprint', sprint: '9' };
    mocks.board.mockImplementationOnce(async () => view([{ section: nine, rows: [row('7', 'a')] }], { sprint: '9' }));
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(mocks.board.mock.calls.map((c) => c[1])).toEqual([[]]);
    expect(board.current?.held?.sections.map((s) => s.section)).toEqual([nine]);
    expect(heldIds(board)).toEqual([['7']]);
    board.release();
    await flush();

    // A kanban board gains a column.
    const second: Section = { kind: 'column', column: 1 };
    mocks.subscribe.mockImplementationOnce(async () => ({ id: 'sub-9', seq: 10, loaded: true, view: view([{ section: COLUMN, rows: [row('1', 'a')] }], { kanban: true }), poll_every: 10_000 }));
    mocks.board.mockClear();
    mocks.board.mockImplementationOnce(async () => view([{ section: COLUMN, rows: [row('1', 'a')] }, { section: second, rows: [row('2', 'b')] }], { kanban: true, columns: 2 }));
    const kanban = attachTrackerBoard(REF, true);
    await flush();
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(mocks.board.mock.calls.map((c) => c[1])).toEqual([[]]);
    expect(kanban.current?.held?.sections.map((s) => s.section)).toEqual([COLUMN, second]);
    expect(heldIds(kanban)).toEqual([['1'], ['2']]);
    kanban.release();
  });

  it('shows the 20 sections a wide board carries at once and reads the rest one by one, retrying one that timed out on Refresh', async () => {
    const mocks = install();
    const screen: Section[] = Array.from({ length: 23 }, (_, i) => ({ kind: 'column', column: i }));
    const answered = (rev: number) => view(screen.slice(0, 20).map((section, i) => ({ section, rows: [row(String(i), 'a')] })), { kanban: true, columns: 23, screen, revision: rev });
    mocks.subscribe.mockImplementationOnce(async () => ({ id: 'sub-1', seq: 10, loaded: true, view: answered(1), poll_every: 10_000 }));
    const pages = held<SectionPage>('TrackerSectionRows');
    const page = (s: Section, base: number) => ({ section: s, rows: [row(String(base + (s.column ?? 0)), 'a')] }) as unknown as SectionPage;
    const board = attachTrackerBoard(REF, true);
    await flush();
    // The carried sections show before any other read lands.
    expect(board.current?.held?.sections.map((s) => s.section)).toEqual(screen);
    expect(heldIds(board).slice(18)).toEqual([['18'], ['19'], [], [], []]);
    expect(board.current?.held?.sections.slice(20).map((s) => s.unread)).toEqual([true, true, true]);
    expect([...board.current!.paging]).toEqual(['column::20']);
    // One read at a time, from the first page.
    expect(pages.calls).toEqual([['sub-1', screen[20], null]]);
    pages.answer(page(screen[20], 100));
    await flush();
    expect(heldIds(board)[20]).toEqual(['120']);
    expect(pages.calls[1]).toEqual(['sub-1', screen[21], null]);
    // A read that timed out with the connection up is shown on its section,
    // and the read goes on to the next.
    pages.fail(new TransportError('timeout', 'the computer did not answer in time'));
    await flush();
    expect([...board.current!.sectionErrors.keys()]).toEqual(['column::21']);
    expect(board.current?.held?.sections[21].unread).toBe(true);
    expect(pages.calls[2]).toEqual(['sub-1', screen[22], null]);
    expect([...board.current!.paging]).toEqual(['column::22']);
    pages.answer(page(screen[22], 100));
    await flush();
    expect(pages.calls).toHaveLength(3);
    // Refresh reads the failed one again.
    await refreshTrackerBoard(board.key);
    await flush();
    expect(pages.calls[3]).toEqual(['sub-1', screen[21], null]);
    pages.answer(page(screen[21], 100));
    await flush();
    expect(heldIds(board).slice(20)).toEqual([['120'], ['121'], ['122']]);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('releases a wide board at once while its other sections are being read', async () => {
    const mocks = install();
    const screen: Section[] = Array.from({ length: 21 }, (_, i) => ({ kind: 'column', column: i }));
    mocks.subscribe.mockImplementationOnce(async () => ({
      id: 'sub-1', seq: 10, loaded: true, poll_every: 10_000,
      view: view(screen.slice(0, 20).map((section) => ({ section, rows: [] })), { kanban: true, columns: 21, screen }),
    }));
    held<SectionPage>('TrackerSectionRows');
    const board = attachTrackerBoard(REF, true);
    await flush();
    board.release();
    await flush();
    expect(mocks.release).toHaveBeenCalledWith('sub-1');
  });

  it('reads a section a full read did not carry back to the depth it held, before it replaces the board', async () => {
    const mocks = install();
    const screen: Section[] = Array.from({ length: 21 }, (_, i) => ({ kind: 'column', column: i }));
    const answered = () => view(screen.slice(0, 20).map((section) => ({ section, rows: [row('1', 'a')] })), { kanban: true, columns: 21, screen });
    mocks.subscribe.mockImplementationOnce(async () => ({ id: 'sub-1', seq: 10, loaded: true, view: answered(), poll_every: 10_000 }));
    mocks.board.mockImplementation(async () => answered());
    const cursor = (issue: string) => ({ order: `k${issue}`, issue, lifetime: 1, revision: 1 });
    mocks.page.mockImplementation(async (_id: string, section: Section, after: { issue: string } | null) => (
      after === null
        ? { section, rows: rows(500, 100), next: cursor('599') }
        : { section, rows: rows(600, 50), next: null }
    ));
    const board = attachTrackerBoard(REF, true);
    await flush();
    loadMoreOfSection(board.key, screen[20]);
    await flush();
    expect(heldIds(board)[20]).toHaveLength(150);
    mocks.page.mockClear();
    applyTrackerBoardEvent(event(11, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(mocks.page.mock.calls.map((c) => c[2])).toEqual([null, cursor('599')]);
    expect(heldIds(board)[20]).toHaveLength(150);
    board.release();
  });

  it('reads a stale page again once the patch carries its cursor on', async () => {
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    const board = attachTrackerBoard(REF, true);
    await flush();
    mocks.page.mockImplementationOnce(async () => {
      throw new TransportError('tracker_page_stale', 'the board changed since this section was read; read the section again from its start');
    });
    loadMoreOfSection(board.key, COLUMN);
    await flush();
    expect(mocks.page).toHaveBeenCalledTimes(1);
    // Nothing is being read: the page waits for the patch.
    expect(board.current?.paging.size).toBe(0);
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
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    mocks.page.mockImplementation(async (_id: string, _s: Section, after: { issue: string }) => {
      const from = Number(after.issue) + 1;
      return { section: COLUMN, rows: rows(from, 100), next: from + 100 < 300 ? { order: `k${String(from + 99).padStart(4, '0')}`, issue: String(from + 99), lifetime: 1, revision: 1 } : null };
    });
    const board = attachTrackerBoard(REF, true);
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
    const board = attachTrackerBoard(REF, true);
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
    const board = attachTrackerBoard(REF, true);
    await flush();
    applyTrackerBoardEvent(event(11, { ended: true, changed: ['1'] }), HOME_BACKEND);
    applyTrackerBoardEvent(event(12, { read_again: true }), HOME_BACKEND);
    await flush();
    expect(board.current?.ended).toBe(true);
    expect(mocks.patch).not.toHaveBeenCalled();
    expect(mocks.board).not.toHaveBeenCalled();
    board.release();
  });

  it('ends a board its computer refuses as unlisted or turned off, retries nothing, and subscribes again on renewal', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      const mocks = install();
      mocks.subscribe.mockImplementationOnce(async () => {
        throw new TransportError('tracker_connection_off', 'this connection is turned off');
      });
      const board = attachTrackerBoard(REF, true);
      await vi.advanceTimersByTimeAsync(0);
      expect(board.current?.ended).toBe(true);
      expect(board.error).toBeNull();
      // Past the entity store's longest retry wait, nothing subscribes again.
      await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.maxMs * 2);
      expect(mocks.subscribe).toHaveBeenCalledTimes(1);
      renewEndedTrackerBoard(board.key);
      await vi.advanceTimersByTimeAsync(0);
      expect(mocks.subscribe).toHaveBeenCalledTimes(2);
      expect(board.current?.ended).toBe(false);
      expect(heldIds(board)).toEqual([['1']]);
      // A board that has not ended is not subscribed again.
      renewEndedTrackerBoard(board.key);
      await vi.advanceTimersByTimeAsync(0);
      expect(mocks.subscribe).toHaveBeenCalledTimes(2);
      board.release();
      await vi.advanceTimersByTimeAsync(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it('subscribes again when its handle is gone, and on reconnect', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
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
    const board = attachTrackerBoard(REF, true);
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
    expect(mocks.board).toHaveBeenCalledTimes(1);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('reads the copy again on a Refresh after a read of it failed, with no event to run it', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
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
    expect(mocks.board).toHaveBeenCalledTimes(1);
    expect(board.current?.readError).toBeNull();
    board.release();
  });

  it('takes the failure a gap read answers, unless an event told a newer one during the read', async () => {
    const failure = { kind: 'auth', message: 'Jira refused the token', at: 50 };
    const mocks = install({ failure, view: { ...view([]), sync: { read_at: 100, failure } } as unknown as BoardView });
    let told: typeof failure | undefined = failure;
    mocks.board.mockImplementation(async () => (
      { ...view([{ section: SPRINT, rows: [row('1', 'a')] }]), sync: { read_at: 100, failure: told } }
    ));
    const board = attachTrackerBoard(REF, true);
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

describe('a write waiting on the board', () => {
  /** Whether `p` has settled by now. */
  async function settledBy(p: Promise<void>): Promise<boolean> {
    let done = false;
    void p.then(() => { done = true; });
    await flush();
    return done;
  }

  /** A board with the patch an event asked for out, and a wait on it. */
  async function waitingOnPatch() {
    install();
    const board = attachTrackerBoard(REF, true);
    await flush();
    const patches = held<BoardPatch>('TrackerBoardPatch');
    // The write's refresh tells its event before the write's answer.
    applyTrackerBoardEvent(event(11, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(patches.calls.length).toBe(1);
    return { board, patches, wait: trackerBoardsSettled(HOME_BACKEND, 'c1') };
  }

  it('waits for the patch an event asked for and ends when it lands', async () => {
    const { board, patches, wait } = await waitingOnPatch();
    expect(await settledBy(wait)).toBe(false);
    // Another connection's board, and a board with nothing owed, add no wait.
    expect(await settledBy(trackerBoardsSettled(HOME_BACKEND, 'c2'))).toBe(true);
    patches.answer(patch([[row('1', 'a', 'saved')]], 2));
    expect(await settledBy(wait)).toBe(true);
    expect(board.current?.held?.sections[0].rows[0].card.title).toBe('saved');
    expect(await settledBy(trackerBoardsSettled(HOME_BACKEND, 'c1'))).toBe(true);
    board.release();
  });

  it('ends when the patch fails, leaving the board in its failure', async () => {
    const { board, patches, wait } = await waitingOnPatch();
    patches.fail(new TransportError('internal', 'the copy could not be read'));
    expect(await settledBy(wait)).toBe(true);
    expect(board.current?.readError).toBeTruthy();
    // While the failed read is held for a recovery trigger, a new wait ends at once.
    applyTrackerBoardEvent(event(12, { changed: ['1'] }), HOME_BACKEND);
    await flush();
    expect(await settledBy(trackerBoardsSettled(HOME_BACKEND, 'c1'))).toBe(true);
    board.release();
  });

  it('ends when the board is released, or the connection drops, during the read', async () => {
    const released = await waitingOnPatch();
    released.board.release();
    expect(await settledBy(released.wait)).toBe(true);

    const dropped = await waitingOnPatch();
    dropped.patches.fail(new DisconnectedError('client closed'));
    expect(await settledBy(dropped.wait)).toBe(true);
    dropped.board.release();
  });

  it('does not wait for a page read, and waits behind one for a patch owed meanwhile', async () => {
    const kanban = () => view([{ section: COLUMN, rows: rows(0, 100), next: true }], { kanban: true });
    const mocks = install({ view: kanban() });
    mocks.board.mockImplementation(async () => kanban());
    const pages = held<SectionPage>('TrackerSectionRows');
    const patches = held<BoardPatch>('TrackerBoardPatch');
    const board = attachTrackerBoard(REF, true);
    await flush();
    // A filter reads every page of the column, one after another.
    setTrackerBoardFiltered(board.key, true);
    await flush();
    expect(pages.calls.length).toBe(1);
    expect(await settledBy(trackerBoardsSettled(HOME_BACKEND, 'c1'))).toBe(true);
    applyTrackerBoardEvent(event(11, { changed: ['5'] }), HOME_BACKEND);
    const wait = trackerBoardsSettled(HOME_BACKEND, 'c1');
    expect(await settledBy(wait)).toBe(false);
    const more = (from: number) => ({ order: `k${String(from + 9).padStart(4, '0')}`, issue: String(from + 9), lifetime: 1, revision: 2 });
    pages.answer({ section: COLUMN, rows: rows(100, 10), next: more(100) } as unknown as SectionPage);
    await flush();
    expect(patches.calls.length).toBe(1);
    expect(await settledBy(wait)).toBe(false);
    patches.answer({ lifetime: 1, revision: 2, sections: [{ section: COLUMN, rows: [] }] } as unknown as BoardPatch);
    // The next page is out, and the wait has ended.
    expect(await settledBy(wait)).toBe(true);
    expect(pages.calls.length).toBe(2);
    board.release();
  });

  it('says whether a held board shows an issue, and stops once it is released', async () => {
    install();
    const board = attachTrackerBoard(REF, true);
    await flush();
    expect(trackerBoardShowsIssue(HOME_BACKEND, 'c1', '1')).toBe(true);
    expect(trackerBoardShowsIssue(HOME_BACKEND, 'c1', '2')).toBe(false);
    expect(trackerBoardShowsIssue(HOME_BACKEND, 'c2', '1')).toBe(false);
    board.release();
    await flush();
    expect(trackerBoardShowsIssue(HOME_BACKEND, 'c1', '1')).toBe(false);
  });
});

describe('visibility', () => {
  it('votes the board down while the pane or the window is hidden and up when both show', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
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
    const board = attachTrackerBoard(REF, false);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledWith('p1', 'c1', '1', [], false);
    expect(mocks.visible).not.toHaveBeenCalled();
    setTrackerBoardShown(board.key, true);
    await flush();
    expect(mocks.visible.mock.calls).toEqual([['sub-1', true]]);
    board.release();
    await flush();

    // Hidden while the subscribe is out, it votes down once the answer lands.
    mocks.visible.mockClear();
    const late = attachTrackerBoard(REF, true);
    setTrackerBoardShown(late.key, false);
    await flush();
    expect(mocks.subscribe).toHaveBeenLastCalledWith('p1', 'c1', '1', [], true);
    expect(mocks.visible.mock.calls).toEqual([['sub-2', false]]);
    late.release();
  });
});

describe('refresh', () => {
  it('spins only while its call is out and shows its two bounded answers as they are', async () => {
    const mocks = install();
    const board = attachTrackerBoard(REF, true);
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
