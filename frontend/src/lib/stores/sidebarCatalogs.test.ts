import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, detachBackend, takePinnedBackend } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { collectGarbage } from '../../test/helpers/gc';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import { __catalogRetryCountForTest, catalogLoadState, retryCatalogLoad } from './catalogLoad.svelte';
import { openCatalogReadsForTest, recordCatalogMutation } from './computerCatalogReads';
import { getProjects } from './projects.svelte';
import { getThreadGroups } from './threadGroups.svelte';
import { getThreadById, getThreads, updateThreadTitle } from './threads.svelte';
import { catalogTreeListed, loadSidebarCatalogs } from './sidebarCatalogs';

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (error: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const group = (id: string): ThreadGroup => ({ id, projectId: 'project-1', name: id, createdAt: 0, updatedAt: 0 });
const project = (id: string): ProjectWithCounts => ({
  project: { id, name: id, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
  threadCount: 0, lastActive: 0,
});
const projectIds = () => getProjects().map((row) => row.project.id);
const groupIds = () => getThreadGroups().map((row) => row.id);
const threadIds = () => getThreads().map((row) => row.id);
const groupOf = (id: string) => getThreadById(id)?.groupId;
const never = <T>() => new Promise<T>(() => {});

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  // A round reads all three catalogs of the tree; a case mocks the ones it
  // is about and this answers the rest.
  setBindingMock('ListProjects', async () => []);
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListThreadGroups', async () => []);
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// A listed computer: one round answered with a project and a thread in it,
// and no group.
async function listHome(): Promise<void> {
  setBindingMock('ListProjects', async () => [project('project-1')]);
  setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
  setBindingMock('ListThreadGroups', async () => []);
  await loadSidebarCatalogs();
  await vi.advanceTimersByTimeAsync(0);
  expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
}

function expectNothingRecording(backend = HOME_BACKEND): void {
  expect(openCatalogReadsForTest(backend, 'projects')).toBe(0);
  expect(openCatalogReadsForTest(backend, 'threads')).toBe(0);
  expect(openCatalogReadsForTest(backend, 'groups')).toBe(0);
}

describe("a listed computer's round", () => {
  // The refresh case the project read joined the round for: its project
  // answer and its thread answer land in separate replies, and a project
  // header committed alone would show with an empty "New Thread" button
  // and then grow a thread list, moving every row below it.
  it('commits a new project and the thread under it together, never the header alone', async () => {
    await listHome();
    const threads = deferred<Thread[]>();
    setBindingMock('ListProjects', async () => [project('project-1'), project('project-2')]);
    setBindingMock('ListThreads', () => threads.promise);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    // The project answer is in and waits: no header without its threads.
    expect(projectIds()).toEqual(['project-1']);
    threads.resolve([makeThread({ id: 't1' }), makeThread({ id: 't2', projectId: 'project-2' })]);
    await round;
    expect(projectIds()).toEqual(['project-1', 'project-2']);
    expect(threadIds()).toEqual(['t1', 't2']);
    expectNothingRecording();
  });

  it('commits a late project answer with the thread answer it waited for', async () => {
    await listHome();
    const projects = deferred<ProjectWithCounts[]>();
    const threads = deferred<Thread[]>();
    setBindingMock('ListProjects', () => projects.promise);
    setBindingMock('ListThreads', () => threads.promise);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await round;
    threads.resolve([makeThread({ id: 't1' }), makeThread({ id: 't2', projectId: 'project-2' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(projectIds()).toEqual(['project-1']);
    expect(threadIds()).toEqual(['t1']);
    projects.resolve([project('project-1'), project('project-2')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(projectIds()).toEqual(['project-1', 'project-2']);
    expect(threadIds()).toEqual(['t1', 't2']);
    expectNothingRecording();
  });

  it('commits threads and groups answered on time together', async () => {
    await listHome();
    const threads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual([]);
    threads.resolve([makeThread({ id: 't1', groupId: 'g2' })]);
    await round;
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    expectNothingRecording();
  });

  it('commits two late answers together', async () => {
    await listHome();
    const threads = deferred<Thread[]>();
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await round;
    groups.resolve([group('g2')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual([]);
    threads.resolve([makeThread({ id: 't1', groupId: 'g2' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    expectNothingRecording();
  });

  // F1: the thread answer waits for its partner instead of showing alone.
  it('holds an on-time thread answer while the group read is past the deadline, and commits both when it lands', async () => {
    await listHome();
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', groupId: 'g2' }), makeThread({ id: 'new', groupId: 'g2' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await round;
    expect(threadIds()).toEqual(['t1']);
    expect(groupOf('t1')).toBeUndefined();
    groups.resolve([group('g2')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1', 'new']);
    expect(groupIds()).toEqual(['g2']);
    expectNothingRecording();
  });

  it('commits a pair once both windows end, when another computer\'s group read is slower', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : [makeThread({ id: 't1' })]));
    setBindingMock('ListThreadGroups', async () => []);
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const gpuGroups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : [makeThread({ id: 't1', groupId: 'g2' })]));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? gpuGroups.promise : Promise.resolve([group('g2')])));
    await loadSidebarCatalogs();
    expect(groupOf('t1')).toBeUndefined();
    expect(groupIds()).toEqual([]);
    gpuGroups.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupOf('t1')).toBe('g2');
    expect(groupIds()).toEqual(['g2']);
  });
});

// G1 and H2: the pair is atomic; an answered half whose partner failed is never published.
describe("a listed computer's answered catalog whose partner failed", () => {
  const diskError = (what: string) => new TransportError('method_error', `list ${what}: disk I/O error`);

  it('is withheld on a refresh, and the retry brings the next pair whole', async () => {
    await listHome();
    setBindingMock('ListThreads', async () => { throw diskError('threads'); });
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual([]);
    expect(threadIds()).toEqual(['t1']);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(__catalogRetryCountForTest()).toBe(1);
    expectNothingRecording();

    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', groupId: 'g2' })]);
    await vi.advanceTimersByTimeAsync(1000);
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('is withheld when the project read fails, and the retry brings the whole tree', async () => {
    await listHome();
    setBindingMock('ListProjects', async () => { throw diskError('projects'); });
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' }), makeThread({ id: 't2', projectId: 'project-2' })]);
    await loadSidebarCatalogs().catch(() => null);
    await vi.advanceTimersByTimeAsync(0);
    // Both the project rows and the thread rows stay as the last complete
    // round left them.
    expect(projectIds()).toEqual(['project-1']);
    expect(threadIds()).toEqual(['t1']);
    expect(catalogLoadState(HOME_BACKEND, 'projects').phase).toBe('failed');
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(__catalogRetryCountForTest()).toBe(1);
    expectNothingRecording();

    setBindingMock('ListProjects', async () => [project('project-1'), project('project-2')]);
    await vi.advanceTimersByTimeAsync(1000);
    expect(projectIds()).toEqual(['project-1', 'project-2']);
    expect(threadIds()).toEqual(['t1', 't2']);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it("stays withheld when the retry's partner fails again, and the first complete pair replaces the last", async () => {
    await listHome();
    const threads = setBindingMock('ListThreads', async () => { throw diskError('threads'); });
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual([]);
    await vi.advanceTimersByTimeAsync(1000);
    expect(threads).toHaveBeenCalledTimes(2);
    expect(groupIds()).toEqual([]);
    expect(threadIds()).toEqual(['t1']);
    threads.mockImplementation(async () => [makeThread({ id: 't1', groupId: 'g2' })]);
    await vi.advanceTimersByTimeAsync(2000);
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    expectNothingRecording();
  });

  it('is withheld when the group read fails, yet the round resolves with it for the restore', async () => {
    await listHome();
    const threads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', async () => { throw diskError('thread groups'); });
    const round = loadSidebarCatalogs();
    // The group failure settles first, so the thread answer is withheld as it lands.
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    threads.resolve([makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })]);
    const rows = await round;
    expect(rows.map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    expect(threadBackend('saved-pane')).toBe(HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    expectNothingRecording();
  });

  it("is withheld when the partner fails passively, for the reconnect's hello to re-read", async () => {
    await listHome();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' }), makeThread({ id: 'new' })]);
    setBindingMock('ListThreadGroups', async () => { throw new DisconnectedError('Computer is offline.'); });
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

});

// F2 and O3: a newer round supersedes both reads of a computer at once.
describe('overlapping rounds', () => {
  it("publish only the newer round's answers, even the older round's group answer already in", async () => {
    await listHome();
    const olderThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => olderThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('old-group')]);
    const older = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const newerThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => newerThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('new-group')]);
    const newer = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual([]);
    olderThreads.resolve([makeThread({ id: 'old' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual([]);
    expect(threadIds()).toEqual(['t1']);

    newerThreads.resolve([makeThread({ id: 'new', groupId: 'new-group' })]);
    await Promise.all([older, newer]);
    expect(groupIds()).toEqual(['new-group']);
    expect(threadIds()).toEqual(['new']);
    expectNothingRecording();
  });

  // F3: the superseded caller resolves with what the winning round brought.
  it("resolve the superseded caller with the newer round's thread rows", async () => {
    const old = deferred<Thread[]>();
    setBindingMock('ListThreads', () => old.promise);
    setBindingMock('ListThreadGroups', async () => []);
    const boot = loadSidebarCatalogs();
    const next = deferred<Thread[]>();
    setBindingMock('ListThreads', () => next.promise);
    const hello = loadSidebarCatalogs();
    old.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    next.resolve([makeThread({ id: 'saved-pane' })]);
    expect((await boot).map((thread) => thread.id)).toEqual(['saved-pane']);
    expect((await hello).map((thread) => thread.id)).toEqual(['saved-pane']);
  });

  it('resolve a caller one of whose computers a single-computer round superseded with that round\'s rows', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    const gpuOld = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuOld.promise : Promise.resolve([makeThread({ id: 'home' })])));
    const all = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const gpuNew = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuNew.promise : Promise.resolve([makeThread({ id: 'home' })])));
    const retry = loadSidebarCatalogs('gpu');
    gpuOld.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    gpuNew.resolve([makeThread({ id: 'gpu-thread' })]);
    expect((await all).map((thread) => thread.id).sort()).toEqual(['gpu-thread', 'home']);
    await retry;
  });

  it("include in the superseded caller's rows the newer round's thread answer held for its partner", async () => {
    await listHome();
    const old = deferred<Thread[]>();
    setBindingMock('ListThreads', () => old.promise);
    setBindingMock('ListThreadGroups', async () => []);
    const boot = loadSidebarCatalogs();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })]);
    setBindingMock('ListThreadGroups', never);
    const hello = loadSidebarCatalogs();
    old.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    expect((await boot).map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    await vi.advanceTimersByTimeAsync(2500);
    await hello;
    expect(threadIds()).toEqual(['t1']);
  });
});

// G3: a superseded round follows every round that took a read over, then decides.
describe('a round whose reads newer rounds took over', () => {
  it("resolves with a superseding round's rows when its own still-current read failed with nothing kept", async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    const gpuOld = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu'
      ? gpuOld.promise
      : Promise.reject(new TransportError('method_error', 'list threads: disk I/O error'))));
    const all = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [makeThread({ id: 'gpu-thread' })] : []));
    const retry = loadSidebarCatalogs('gpu');
    gpuOld.resolve([]);
    expect((await all).map((thread) => thread.id)).toEqual(['gpu-thread']);
    await retry;
  });

  it('follows a round that supersedes it while it waits for another', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : [makeThread({ id: 't1' })]));
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);

    // The boot round: Home answers on time with the saved pane's thread and
    // holds it for its slow groups; GPU's threads are slow.
    const gpuOld = deferred<Thread[]>();
    const gpuNew = deferred<Thread[]>();
    const homeNew = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu'
      ? gpuOld.promise
      : Promise.resolve([makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })])));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? Promise.resolve([]) : never<ThreadGroup[]>()));
    let rows: readonly Thread[] | null = null;
    void loadSidebarCatalogs().then((resolved) => { rows = resolved; });
    await vi.advanceTimersByTimeAsync(0);
    // A GPU retry takes GPU's read over; the boot's thread window ends.
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuNew.promise : homeNew.promise));
    setBindingMock('ListThreadGroups', async () => []);
    void loadSidebarCatalogs('gpu');
    gpuOld.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    // While the boot waits for it, a Home retry takes Home's held read over.
    void loadSidebarCatalogs(HOME_BACKEND);
    gpuNew.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    expect(rows).toBeNull();
    homeNew.resolve([makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(rows!.map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
  });
});

// C2: following a newer round is bounded by the caller's own budget.
describe('a boot round superseded by a retry that waits for its RPC', () => {
  it('resolves within its on-time budget with the rows it has', async () => {
    await listHome();
    setBindingMock('ListThreads', never);
    setBindingMock('ListThreadGroups', never);
    const boot = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    void loadSidebarCatalogs(HOME_BACKEND).catch(() => {});
    let resolved: readonly Thread[] | null = null;
    void boot.then((rows) => { resolved = rows; });
    await vi.advanceTimersByTimeAsync(2499);
    expect(resolved).toBeNull();
    await vi.advanceTimersByTimeAsync(1);
    expect(resolved!.map((thread) => thread.id)).toEqual(['t1']);
  });

  it('rejects as a missed deadline at its budget when no thread rows are known, never as an empty list', async () => {
    setBindingMock('ListThreads', never);
    setBindingMock('ListThreadGroups', never);
    const boot = loadSidebarCatalogs().catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(0);
    void loadSidebarCatalogs(HOME_BACKEND).catch(() => {});
    await vi.advanceTimersByTimeAsync(2500);
    expect(await boot).toBeInstanceOf(ReadDeadlineError);
  });
});

// O2: what the boot's layout restore validates against.
describe('the thread rows a round resolves with', () => {
  it("include an unlisted computer's thread answer while its group read is still out", async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', never);
    expect((await loadSidebarCatalogs()).map((thread) => thread.id)).toEqual(['t1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
  });

  it("include a listed computer's held thread answer, with changes made while it was out, and its owners", async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [makeThread({ id: 'gpu-1' })] : []));
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed('gpu')).toBe(true);

    const gpuThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuThreads.promise : Promise.resolve([])));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? never<ThreadGroup[]>() : Promise.resolve([])));
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    updateThreadTitle('gpu-1', 'renamed while out');
    gpuThreads.resolve([makeThread({ id: 'gpu-1' }), makeThread({ id: 'gpu-new' })]);
    const rows = await round;
    expect(rows.map((thread) => [thread.id, thread.title])).toEqual([['gpu-1', 'renamed while out'], ['gpu-new', 'Test thread']]);
    expect(threadBackend('gpu-new')).toBe('gpu');
    expect(getThreadById('gpu-new')).toBeUndefined();
  });
});

// C5: an answer dropped while held leaves nothing behind.
describe('a held answer', () => {
  it('ends its journals and forgets its owners when its computer detaches', async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : []));
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed('gpu')).toBe(true);

    const gpuGroups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [makeThread({ id: 'gpu-held' })] : []));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? gpuGroups.promise : Promise.resolve([])));
    await loadSidebarCatalogs();
    expect(openCatalogReadsForTest('gpu', 'threads')).toBe(1);
    expect(threadBackend('gpu-held')).toBe('gpu');

    detachBackend('gpu');
    expectNothingRecording('gpu');
    expect(threadBackend('gpu-held')).toBeUndefined();
    gpuGroups.resolve([group('gpu-group')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreadById('gpu-held')).toBeUndefined();
    expect(groupIds()).toEqual([]);
  });

  it('ends its journals when a newer round supersedes it', async () => {
    await listHome();
    setBindingMock('ListThreads', async () => [makeThread({ id: 'held' })]);
    setBindingMock('ListThreadGroups', never);
    await loadSidebarCatalogs();
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(1);
    setBindingMock('ListThreads', never);
    void loadSidebarCatalogs().catch(() => {});
    // The newer round's own reads are the only ones recording, the older
    // group read still out included: nothing can commit it.
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(1);
    expect(openCatalogReadsForTest(HOME_BACKEND, 'groups')).toBe(1);
  });
});

// H1: every round of a computer is one attempt of its pair, whatever
// started it, so a retry never runs over a round that is still out.
describe('a retry armed when a newer round begins', () => {
  beforeEach(() => {
    setBindingMock('ListProjects', async () => []);
  });

  async function failGroupsOnce(): Promise<void> {
    await listHome();
    setBindingMock('ListThreadGroups', async () => { throw new TransportError('method_error', 'list thread groups: disk I/O error'); });
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(__catalogRetryCountForTest()).toBe(1);
  }

  async function expectTheLiveRoundAnswers(act: () => Promise<unknown> | void): Promise<void> {
    const liveThreads = deferred<Thread[]>();
    const list = setBindingMock('ListThreads', () => liveThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    // A hello's round, begun while the retry is armed.
    void loadSidebarCatalogs().catch(() => {});
    await vi.advanceTimersByTimeAsync(0);
    expect(__catalogRetryCountForTest()).toBe(0);
    await act();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(list).toHaveBeenCalledTimes(1);
    expect(groupIds()).toEqual([]);
    liveThreads.resolve([makeThread({ id: 't1', groupId: 'g2' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    expect(__catalogRetryCountForTest()).toBe(0);
    expectNothingRecording();
  }

  it('cancels it: the timer never fires over the round, whose pair commits once', async () => {
    await failGroupsOnce();
    await expectTheLiveRoundAnswers(() => vi.advanceTimersByTimeAsync(1000));
  });

  it('makes Retry start nothing while the round is out', async () => {
    await failGroupsOnce();
    await expectTheLiveRoundAnswers(() => retryCatalogLoad(HOME_BACKEND));
  });

  // The round's thread answer is withheld, so nothing settles the threads;
  // the retry their earlier failure asked for is the round's to consume.
  it('consumes it: a round whose groups fail passively leaves no retry for the reconnect to race', async () => {
    await listHome();
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(__catalogRetryCountForTest()).toBe(1);
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', async () => { throw new DisconnectedError('Computer is offline.'); });
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(__catalogRetryCountForTest()).toBe(0);
  });
});

// G4: a read that is done leaves nothing of itself behind.
describe("a computer's read once every part is done", () => {
  it('holds neither the mutations recorded while it was out nor its failure', async () => {
    await listHome();
    const threads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => threads.promise);
    let refused: TransportError | null = new TransportError('method_error', 'list thread groups: disk I/O error');
    const refusedRef = new WeakRef(refused);
    const groups = setBindingMock('ListThreadGroups', async () => { throw refused; });
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    let captured: Thread | null = makeThread({ id: 'captured' });
    const capturedRef = new WeakRef(captured);
    recordCatalogMutation<Thread>(HOME_BACKEND, 'threads', ((row) => (rows) => rows.filter((thread) => thread.id !== row.id))(captured));
    captured = null;
    refused = null;
    threads.resolve([makeThread({ id: 't1' })]);
    await round;
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
    groups.mockClear();
    vi.restoreAllMocks();
    await collectGarbage();
    expect(capturedRef.deref()).toBeUndefined();
    expect(refusedRef.deref()).toBeUndefined();
  });

  it('releases a withheld thread answer once no round runs', async () => {
    await listHome();
    let answer: Thread[] | null = [makeThread({ id: 't1' }), makeThread({ id: 'withheld' })];
    const answerRef = new WeakRef(answer[1]!);
    const list = setBindingMock('ListThreads', async () => answer);
    setBindingMock('ListThreadGroups', async () => { throw new TransportError('method_error', 'list thread groups: disk I/O error'); });
    expect((await loadSidebarCatalogs()).map((thread) => thread.id)).toEqual(['t1', 'withheld']);
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
    answer = null;
    list.mockClear();
    await collectGarbage();
    expect(answerRef.deref()).toBeUndefined();
  });

  // H3: a withheld answer lives for the rounds that can read it, not for
  // every round in flight.
  it("releases a computer's withheld thread answer when its round resolves, while another computer's round is out", async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : [makeThread({ id: 't1' })]));
    await loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);

    // The test drops every reference it holds to the answer, the settled
    // promise that carries it included.
    let homeThreads: { promise: Promise<Thread[]>; resolve: (rows: Thread[]) => void } | null = deferred<Thread[]>();
    const list = setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? never<Thread[]>() : homeThreads!.promise));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu'
      ? Promise.resolve([])
      : Promise.reject(new TransportError('method_error', 'list thread groups: disk I/O error'))));
    void loadSidebarCatalogs('gpu').catch(() => {});
    let home: Promise<readonly Thread[]> | null = loadSidebarCatalogs(HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    let answer: Thread[] | null = [makeThread({ id: 't1' }), makeThread({ id: 'withheld' })];
    const answerRef = new WeakRef(answer[1]!);
    homeThreads.resolve(answer);
    homeThreads = null;
    expect((await home).map((thread) => thread.id)).toEqual(['t1', 'withheld']);
    home = null;
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
    answer = null;
    list.mockClear();
    await collectGarbage();
    expect(answerRef.deref()).toBeUndefined();
  });

  it('hands a withheld thread answer to the round following the one that withheld it', async () => {
    await listHome();
    const old = deferred<Thread[]>();
    setBindingMock('ListThreads', () => old.promise);
    setBindingMock('ListThreadGroups', async () => []);
    const boot = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const fresh = deferred<Thread[]>();
    setBindingMock('ListThreads', () => fresh.promise);
    setBindingMock('ListThreadGroups', async () => { throw new TransportError('method_error', 'list thread groups: disk I/O error'); });
    const retry = loadSidebarCatalogs(HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    fresh.resolve([makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })]);
    expect((await retry).map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    old.resolve([]);
    expect((await boot).map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
  });

  it('hands it to a following round when it is withheld after the round that read it resolved', async () => {
    await listHome();
    const old = deferred<Thread[]>();
    setBindingMock('ListThreads', () => old.promise);
    setBindingMock('ListThreadGroups', async () => []);
    const boot = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    // The retry resolves with its held thread answer; its group read fails after.
    expect((await loadSidebarCatalogs(HOME_BACKEND)).map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    groups.reject(new TransportError('method_error', 'list thread groups: disk I/O error'));
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    old.resolve([]);
    expect((await boot).map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    await vi.advanceTimersByTimeAsync(0);
    expect(threadIds()).toEqual(['t1']);
  });
});
