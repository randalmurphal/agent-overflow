import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, detachBackend, takePinnedBackend } from '../transport/backends';
import { __resetEntityIndexForTest, threadBackend } from '../transport/entityIndex';
import { TransportError } from '../transport/wsClient';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import type { Thread, ThreadGroup } from '../types/models';
import { __catalogRetryCountForTest, catalogLoadState, resetCatalogLoadForTest, retryCatalogLoad } from './catalogLoad.svelte';
import { openCatalogReadsForTest, resetCatalogReadsForTest } from './computerCatalogReads';
import { resetProjectsForTest } from './projects.svelte';
import { getThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import { getThreadById, getThreads, resetThreadsForTest, updateThreadTitle } from './threads.svelte';
import { catalogTreeListed, loadThreadCatalogs, resetThreadCatalogsForTest } from './threadCatalogs';

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (error: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const group = (id: string): ThreadGroup => ({ id, projectId: 'project-1', name: id, createdAt: 0, updatedAt: 0 });
const groupIds = () => getThreadGroups().map((row) => row.id);
const threadIds = () => getThreads().map((row) => row.id);
const groupOf = (id: string) => getThreadById(id)?.groupId;
const never = <T>() => new Promise<T>(() => {});

beforeEach(() => {
  resetStagedBackends();
  resetCatalogLoadForTest();
  resetThreadCatalogsForTest();
  resetCatalogReadsForTest();
  resetThreadsForTest();
  resetThreadGroupsForTest();
  resetProjectsForTest();
  __resetEntityIndexForTest();
  vi.useFakeTimers();
  vi.spyOn(console, 'error').mockImplementation(() => {});
});
afterEach(() => {
  resetStagedBackends();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// A listed computer: one round answered with a thread and no group.
async function listHome(): Promise<void> {
  setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
  setBindingMock('ListThreadGroups', async () => []);
  await loadThreadCatalogs();
  await vi.advanceTimersByTimeAsync(0);
  expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
}

function expectNothingRecording(backend = HOME_BACKEND): void {
  expect(openCatalogReadsForTest(backend, 'threads')).toBe(0);
  expect(openCatalogReadsForTest(backend, 'groups')).toBe(0);
}

describe("a listed computer's round", () => {
  it('commits threads and groups answered on time together', async () => {
    await listHome();
    const threads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    const round = loadThreadCatalogs();
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
    const round = loadThreadCatalogs();
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
    const round = loadThreadCatalogs();
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
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const gpuGroups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : [makeThread({ id: 't1', groupId: 'g2' })]));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? gpuGroups.promise : Promise.resolve([group('g2')])));
    await loadThreadCatalogs();
    expect(groupOf('t1')).toBeUndefined();
    expect(groupIds()).toEqual([]);
    gpuGroups.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupOf('t1')).toBe('g2');
    expect(groupIds()).toEqual(['g2']);
  });

  // R1: a failed half lets the other commit alone.
  it('commits the groups alone when the thread read fails', async () => {
    await listHome();
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual(['g2']);
    expect(threadIds()).toEqual(['t1']);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(__catalogRetryCountForTest()).toBe(1);
  });
});

// F2 and O3: a newer round supersedes both reads of a computer at once.
describe('overlapping rounds', () => {
  it("publish only the newer round's answers, even the older round's group answer already in", async () => {
    await listHome();
    const olderThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => olderThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('old-group')]);
    const older = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const newerThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => newerThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('new-group')]);
    const newer = loadThreadCatalogs();
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
    const boot = loadThreadCatalogs();
    const next = deferred<Thread[]>();
    setBindingMock('ListThreads', () => next.promise);
    const hello = loadThreadCatalogs();
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
    const all = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    const gpuNew = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuNew.promise : Promise.resolve([makeThread({ id: 'home' })])));
    const retry = loadThreadCatalogs('gpu');
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
    const boot = loadThreadCatalogs();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' }), makeThread({ id: 'saved-pane' })]);
    setBindingMock('ListThreadGroups', never);
    const hello = loadThreadCatalogs();
    old.resolve([]);
    await vi.advanceTimersByTimeAsync(0);
    expect((await boot).map((thread) => thread.id)).toEqual(['t1', 'saved-pane']);
    await vi.advanceTimersByTimeAsync(2500);
    await hello;
    expect(threadIds()).toEqual(['t1']);
  });
});

// C2: following a newer round is bounded by the caller's own budget.
describe('a boot round superseded by a retry that waits for its RPC', () => {
  it('resolves within its on-time budget with the rows it has', async () => {
    await listHome();
    setBindingMock('ListThreads', never);
    setBindingMock('ListThreadGroups', never);
    const boot = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    void loadThreadCatalogs(HOME_BACKEND).catch(() => {});
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
    const boot = loadThreadCatalogs().catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(0);
    void loadThreadCatalogs(HOME_BACKEND).catch(() => {});
    await vi.advanceTimersByTimeAsync(2500);
    expect(await boot).toBeInstanceOf(ReadDeadlineError);
  });
});

// O2: what the boot's layout restore validates against.
describe('the thread rows a round resolves with', () => {
  it("include an unlisted computer's thread answer while its group read is still out", async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', never);
    expect((await loadThreadCatalogs()).map((thread) => thread.id)).toEqual(['t1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
  });

  it("include a listed computer's held thread answer, with changes made while it was out, and its owners", async () => {
    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreadGroups', async () => []);
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [makeThread({ id: 'gpu-1' })] : []));
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed('gpu')).toBe(true);

    const gpuThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuThreads.promise : Promise.resolve([])));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? never<ThreadGroup[]>() : Promise.resolve([])));
    const round = loadThreadCatalogs();
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
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed('gpu')).toBe(true);

    const gpuGroups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [makeThread({ id: 'gpu-held' })] : []));
    setBindingMock('ListThreadGroups', () => (takePinnedBackend() === 'gpu' ? gpuGroups.promise : Promise.resolve([])));
    await loadThreadCatalogs();
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
    await loadThreadCatalogs();
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(1);
    setBindingMock('ListThreads', never);
    void loadThreadCatalogs().catch(() => {});
    // The newer round's own reads are the only ones recording.
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(1);
    expect(openCatalogReadsForTest(HOME_BACKEND, 'groups')).toBe(2);
  });
});

// F5: a retry is a round of the pair, so it supersedes a live round whole.
describe('a retry while a round is out', () => {
  async function failGroupsOnce(): Promise<void> {
    await listHome();
    setBindingMock('ListThreadGroups', async () => { throw new TransportError('method_error', 'list thread groups: disk I/O error'); });
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(__catalogRetryCountForTest()).toBe(1);
  }

  async function expectOneChange(retry: () => Promise<unknown> | void): Promise<void> {
    const liveThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => liveThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('live-group')]);
    void loadThreadCatalogs().catch(() => {});
    await vi.advanceTimersByTimeAsync(0);
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', groupId: 'g2' })]);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    await retry();
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    liveThreads.resolve([makeThread({ id: 't1' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(groupIds()).toEqual(['g2']);
    expect(groupOf('t1')).toBe('g2');
    expectNothingRecording();
  }

  it('from the timer commits the pair in one change and drops the live round', async () => {
    await failGroupsOnce();
    await expectOneChange(() => vi.advanceTimersByTimeAsync(1000));
  });

  it('from Retry commits the pair in one change and drops the live round', async () => {
    await failGroupsOnce();
    await expectOneChange(() => retryCatalogLoad(HOME_BACKEND));
  });
});
