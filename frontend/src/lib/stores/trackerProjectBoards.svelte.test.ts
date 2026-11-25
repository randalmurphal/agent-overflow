import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  acquireTrackerProjectBoards,
  refreshTrackerProjectBoards,
  saveTrackerProjectBoards,
  trackerProjectBoardsOf,
  withBoardAdded,
  withBoardMoved,
  withBoardRemoved,
  withOpensFirst,
} from './trackerProjectBoards.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { deferred } from '../../test/helpers/providerAccounts';
import { REMOTE_BACKEND_UUID, stageBackend } from '../../test/helpers/backends';
import { trackerBoardChoice } from '../../test/helpers/trackerConnections';
import { answerAsV0019 } from '../../test/helpers/olderComputer';
import { __resetBackendsForTest, getPinnedBackend } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { setCarriedSessionScopes } from '../transport/scopes';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import type { TrackerBoardChoice, TrackerProjectBoard } from './bindings';

const a = trackerBoardChoice({ board: '1', name: 'WEB board', opens_first: true });
const b = trackerBoardChoice({ board: '2', name: 'Ops board', tracker_project: 'OPS' });
const c = trackerBoardChoice({ connection: 'lin-1', connection_kind: 'linear', board: 'team-1', name: 'Team', tracker_project: 'ENG' });

const ids = (list: readonly { board: string; opens_first?: boolean }[]): string[] =>
  list.map((x) => `${x.board}${x.opens_first ? '*' : ''}`);

describe('the board list edits', () => {
  it('make the first board added the one that opens first, and only that one', () => {
    const first = withBoardAdded([], { connection: 'jira-1', board: '1', name: 'WEB board', tracker_project: 'WEB' });
    expect(first).toEqual([{ connection: 'jira-1', board: '1', name: 'WEB board', tracker_project: 'WEB', opens_first: true }]);
    expect(ids(withBoardAdded([a], { connection: 'jira-1', board: '2', name: 'Ops board', tracker_project: 'OPS' }))).toEqual(['1*', '2']);
  });

  it('pass opens-first to the first board left when the board that had it goes', () => {
    expect(ids(withBoardRemoved([a, b, c], 0))).toEqual(['2*', 'team-1']);
    expect(ids(withBoardRemoved([b, { ...a }, c], 1))).toEqual(['2*', 'team-1']);
    expect(ids(withBoardRemoved([a, b], 1))).toEqual(['1*']);
    expect(withBoardRemoved([a], 0)).toEqual([]);
  });

  it('move a board within the list and choose which opens first', () => {
    expect(ids(withBoardMoved([a, b, c], 0, 1))).toEqual(['2', '1*', 'team-1']);
    expect(ids(withBoardMoved([a, b, c], 2, -1))).toEqual(['1*', 'team-1', '2']);
    expect(ids(withBoardMoved([a, b, c], 0, -1))).toEqual(['1*', '2', 'team-1']);
    expect(ids(withOpensFirst([a, b, c], 2))).toEqual(['1', '2', 'team-1*']);
  });

  // The computer keeps a kept board's stored name; only a board the save
  // adds carries one. A synced name can be longer than a listing holds.
  it('send a kept board without its name, and an added one with the name it was chosen by', () => {
    const synced = { ...b, name: 'x'.repeat(300) };
    expect(withBoardMoved([a, synced], 1, -1)).toEqual([
      { connection: 'jira-1', board: '2', opens_first: false },
      { connection: 'jira-1', board: '1', opens_first: true },
    ]);
    expect(withBoardAdded([synced], { connection: 'jira-1', board: '3', name: 'New', tracker_project: 'NEW' })).toEqual([
      { connection: 'jira-1', board: '2', opens_first: false },
      { connection: 'jira-1', board: '3', name: 'New', tracker_project: 'NEW', opens_first: false },
    ]);
  });
});

describe('a held project board list', () => {
  afterEach(() => {
    __resetBackendsForTest();
  });

  it('is read when held, replaced by each save’s answer, and dropped by the last release', async () => {
    const read = setBindingMock('ProjectTrackerBoards', async () => [a]);
    const release = acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    expect(read).toHaveBeenCalledWith('p1');
    expect(ids(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list)).toEqual(['1*']);

    const save = setBindingMock('SetProjectTrackerBoards', async (_p: string, _held: unknown, boards: TrackerProjectBoard[]) =>
      boards.map((x) => trackerBoardChoice({ ...x, opens_first: x.opens_first ?? false })),
    );
    await saveTrackerProjectBoards(HOME_BACKEND, 'p1', (list) => withBoardAdded(list, { connection: 'jira-1', board: '2', name: 'Ops board', tracker_project: 'OPS' }));
    expect(save).toHaveBeenCalledTimes(1);
    expect(ids(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list)).toEqual(['1*', '2']);
    expect(read).toHaveBeenCalledTimes(1);

    release();
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(false);
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', () => [])).rejects.toThrow('not open any more');
  });

  it('refuses a second save while one runs, and a read that started before a save never lands over it', async () => {
    const reading = deferred<TrackerBoardChoice[]>();
    const reads = [Promise.resolve([a]), reading.promise];
    const read = setBindingMock('ProjectTrackerBoards', () => reads.shift()!);
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));

    // A reconnect starts a read; the save that follows answers first.
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null } as never);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null } as never);
    await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    const saving = deferred<TrackerBoardChoice[]>();
    setBindingMock('SetProjectTrackerBoards', () => saving.promise);
    const first = saveTrackerProjectBoards(HOME_BACKEND, 'p1', (list) => withBoardRemoved(list, 0));
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').saving).toBe(true);
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', () => [])).rejects.toThrow('still saving');

    saving.resolve([]);
    await first;
    reading.resolve([a]);
    await Promise.resolve();
    await Promise.resolve();
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list).toEqual([]);
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').saving).toBe(false);
  });

  it('keeps the list and rejects with the computer’s refusal when a save fails', async () => {
    setBindingMock('ProjectTrackerBoards', async () => [a]);
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    const refusal = new Error('this tracker connection was removed from this computer');
    setBindingMock('SetProjectTrackerBoards', async () => { throw refusal; });
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', () => [])).rejects.toBe(refusal);
    expect(ids(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list)).toEqual(['1*']);
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').saving).toBe(false);
  });

  // The edit is applied to the list the store holds when it saves, the
  // list it sends as the one the save was built from: no caller can pair
  // an edit with another list.
  it('applies each edit to the list it holds, and sends that list as the one the edit was built from', async () => {
    const first = deferred<TrackerBoardChoice[]>();
    const answers = [first.promise, Promise.resolve([a, b])];
    setBindingMock('ProjectTrackerBoards', () => answers.shift()!);
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    first.resolve([a]);
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    await refreshTrackerProjectBoards(HOME_BACKEND, 'p1');
    const built: string[][] = [];
    const save = setBindingMock('SetProjectTrackerBoards', async () => [b]);
    await saveTrackerProjectBoards(HOME_BACKEND, 'p1', (list) => {
      built.push(ids(list));
      return withBoardRemoved(list, 0);
    });
    expect(built).toEqual([['1*', '2']]);
    expect(save).toHaveBeenCalledWith('p1', [
      { connection: 'jira-1', board: '1', opens_first: true },
      { connection: 'jira-1', board: '2', opens_first: false },
    ], [{ connection: 'jira-1', board: '2', opens_first: true }]);
  });

  it('sends, with each save, the list it holds as the one the save was built from', async () => {
    setBindingMock('ProjectTrackerBoards', async () => [a, b]);
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    const save = setBindingMock('SetProjectTrackerBoards', async () => [b]);
    await saveTrackerProjectBoards(HOME_BACKEND, 'p1', (list) => withBoardRemoved(list, 0));
    expect(save).toHaveBeenCalledWith('p1', [
      { connection: 'jira-1', board: '1', opens_first: true },
      { connection: 'jira-1', board: '2', opens_first: false },
    ], [{ connection: 'jira-1', board: '2', opens_first: true }]);
  });

  it('shows a failed first read as a failure, never as an empty list, a dropped connection included', async () => {
    const answers: (() => Promise<TrackerBoardChoice[]>)[] = [
      async () => { throw new DisconnectedError('the connection closed'); },
      async () => [a],
    ];
    setBindingMock('ProjectTrackerBoards', () => answers.shift()!());
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').error).toBeInstanceOf(DisconnectedError));
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1')).toMatchObject({ loaded: false, current: false, list: [] });
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', () => [])).rejects.toThrow('has not been read');
    await refreshTrackerProjectBoards(HOME_BACKEND, 'p1');
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1')).toMatchObject({ loaded: true, current: true, error: null });
    expect(ids(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list)).toEqual(['1*']);
  });

  it('keeps a read list over a failed read, and takes no edit built from it until a read answers', async () => {
    const reading = deferred<TrackerBoardChoice[]>();
    const answers: (() => Promise<TrackerBoardChoice[]>)[] = [
      async () => [a],
      async () => { throw new TransportError('method_error', 'store: disk I/O error'); },
      () => reading.promise,
    ];
    setBindingMock('ProjectTrackerBoards', () => answers.shift()!());
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    await refreshTrackerProjectBoards(HOME_BACKEND, 'p1');
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1')).toMatchObject({ loaded: true, current: false });
    expect(ids(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list)).toEqual(['1*']);
    const save = setBindingMock('SetProjectTrackerBoards', async () => []);
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', () => [])).rejects.toThrow('has not been read');
    expect(save).not.toHaveBeenCalled();
    const retried = refreshTrackerProjectBoards(HOME_BACKEND, 'p1');
    reading.resolve([a, b]);
    await retried;
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1')).toMatchObject({ current: true, error: null });
  });

  // A save that fails may still have stored its list ("saved, and reading
  // back failed"), and a refused one was built from a list another save
  // changed: either way the list shown is not known to be the stored one.
  it('reads the list again after any failed save, and takes no edit until that read answers', async () => {
    setBindingMock('ProjectTrackerBoards', async () => [a]);
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    const reading = deferred<TrackerBoardChoice[]>();
    const read = setBindingMock('ProjectTrackerBoards', () => reading.promise);
    const failure = new TransportError('method_error', 'trackerapp: the boards of project p1 are saved, and reading them back failed, so read them again');
    setBindingMock('SetProjectTrackerBoards', async () => { throw failure; });
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', (list) => withBoardAdded(list, { connection: 'jira-1', board: '2' }))).rejects.toBe(failure);
    expect(read).toHaveBeenCalledTimes(1);
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').current).toBe(false);
    await expect(saveTrackerProjectBoards(HOME_BACKEND, 'p1', () => [])).rejects.toThrow('has not been read');
    reading.resolve([a, b]);
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').current).toBe(true));
    expect(ids(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list)).toEqual(['1*', '2']);
  });

  it('reads as empty on a computer whose release has no trackers', async () => {
    answerAsV0019();
    acquireTrackerProjectBoards(HOME_BACKEND, 'p1');
    await vi.waitFor(() => expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').loaded).toBe(true));
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').list).toEqual([]);
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p1').error).toBeNull();
  });

  it('sends a project’s reads and saves to the computer that holds it, and drops it when that computer goes', async () => {
    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate']);
    const targets: (BackendKey | null)[] = [];
    setBindingMock('ProjectTrackerBoards', async () => {
      targets.push(getPinnedBackend());
      return [c];
    });
    setBindingMock('SetProjectTrackerBoards', async () => {
      targets.push(getPinnedBackend());
      return [];
    });
    acquireTrackerProjectBoards('laptop', 'p9');
    await vi.waitFor(() => expect(trackerProjectBoardsOf('laptop', 'p9').loaded).toBe(true));
    await saveTrackerProjectBoards('laptop', 'p9', () => []);
    expect(targets).toEqual(['laptop', 'laptop']);
    expect(trackerProjectBoardsOf(HOME_BACKEND, 'p9').loaded).toBe(false);

    __resetBackendsForTest();
    expect(trackerProjectBoardsOf('laptop', 'p9').loaded).toBe(false);
  });
});
