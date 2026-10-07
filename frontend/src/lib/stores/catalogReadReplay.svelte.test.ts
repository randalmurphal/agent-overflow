// A local mutation made while a catalog read is out is replayed over the
// read's answer. Before, it superseded the read: the answer was dropped, the
// computer's catalog stayed 'loading', and nothing re-read it.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, detachBackend, takePinnedBackend } from '../transport/backends';
import { __resetEntityIndexForTest, noteThread, noteThreadGroup } from '../transport/entityIndex';
import type { ProjectWithCounts, Thread, ThreadGroup } from '../types/models';
import { __catalogRetryCountForTest, catalogLoadState, resetCatalogLoadForTest, retryCatalogLoad } from './catalogLoad.svelte';
import { openCatalogReadsForTest } from './computerCatalogReads';
import { COMPUTER_READ_DEADLINE_MS } from './computerRows';
import { refreshSidebarProjections } from './eventsThreadRows';
import {
  addProjectLocal, getProjects, refreshProjects, removeProjectLocal, resetProjectsForTest, updateProjectLocal,
} from './projects.svelte';
import {
  clearThreadGroupMembership, getThreads, loadThreads, prependThread, removeThread, replaceThread,
  resetThreadsForTest, updateThreadTitle,
} from './threads.svelte';
import { getThreadGroups, loadThreadGroups, resetThreadGroupsForTest, upsertThreadGroup } from './threadGroups.svelte';

const LOADED = { phase: 'loaded', error: '' };
const LAPTOP = 'laptop';

function project(id: string, name = id): ProjectWithCounts {
  return { project: { id, name, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
    threadCount: 0, lastActive: 0 };
}

function group(id: string): ThreadGroup {
  return { id, name: id, projectId: 'project-1', createdAt: 0, updatedAt: 0 };
}

function threads(...ids: string[]): Thread[] {
  return ids.map((id) => makeThread({ id }));
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (error: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const ids = (rows: readonly { id: string }[]) => rows.map((row) => row.id);

beforeEach(() => {
  resetStagedBackends();
  __resetEntityIndexForTest();
  resetThreadsForTest();
  resetProjectsForTest();
  resetThreadGroupsForTest();
  resetCatalogLoadForTest();
  vi.useFakeTimers();
  vi.spyOn(console, 'error').mockImplementation(() => {});
});
afterEach(() => {
  resetStagedBackends();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('a thread catalog read', () => {
  it('loads with a pushed row that landed while it was out', async () => {
    const list = deferred<Thread[]>();
    setBindingMock('ListThreads', () => list.promise);
    const boot = loadThreads();
    noteThread('pushed', HOME_BACKEND);
    replaceThread(makeThread({ id: 'pushed', title: 'pushed title' }));
    updateThreadTitle('t1', 'renamed while out');
    list.resolve([...threads('t1', 't2'), makeThread({ id: 'pushed', title: 'stale title' })]);
    await boot;

    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(getThreads().map((t) => [t.id, t.title])).toEqual([
      ['t1', 'renamed while out'], ['t2', 'Test thread'], ['pushed', 'pushed title'],
    ]);
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(0);
  });

  it('keeps a thread removed while it was out removed, and one added while it was out', async () => {
    const list = deferred<Thread[]>();
    setBindingMock('ListThreads', () => list.promise);
    const boot = loadThreads();
    removeThread('deleted');
    prependThread(makeThread({ id: 'created' }));
    list.resolve(threads('kept', 'deleted'));
    await boot;

    expect(ids(getThreads())).toEqual(['created', 'kept']);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
  });

  it('replays over a late answer and loads from it', async () => {
    const list = deferred<Thread[]>();
    setBindingMock('ListThreads', () => list.promise);
    // No cache and no answer: the boot read itself rejects (computerRows).
    await Promise.all([loadThreads().catch(() => {}), vi.advanceTimersByTimeAsync(COMPUTER_READ_DEADLINE_MS)]);
    // Past the deadline: retry is scheduled, the read keeps recording.
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('loading');
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(1);
    removeThread('deleted');
    list.resolve(threads('kept', 'deleted'));
    await vi.advanceTimersByTimeAsync(0);

    expect(ids(getThreads())).toEqual(['kept']);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(0);
  });

  it('stops recording when a read fails after its deadline', async () => {
    const list = deferred<Thread[]>();
    let calls = 0;
    setBindingMock('ListThreads', () => ++calls === 1 ? list.promise : new Promise<never>(() => {}));
    // No cache and no answer: the boot read itself rejects (computerRows).
    await Promise.all([loadThreads().catch(() => {}), vi.advanceTimersByTimeAsync(COMPUTER_READ_DEADLINE_MS)]);
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(1);
    list.reject(new Error('late failure'));
    await vi.advanceTimersByTimeAsync(0);
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(0);
  });

  it('leaves the catalog to the newest read when two overlap', async () => {
    const first = deferred<Thread[]>();
    const second = deferred<Thread[]>();
    let calls = 0;
    setBindingMock('ListThreads', () => ++calls === 1 ? first.promise : second.promise);
    const older = loadThreads();
    const newer = loadThreads();
    first.resolve(threads('older'));
    await vi.advanceTimersByTimeAsync(0);
    expect(ids(getThreads())).toEqual([]);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('loading');
    second.resolve(threads('newer'));
    await Promise.all([older, newer]);
    expect(ids(getThreads())).toEqual(['newer']);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
  });

  it('keeps a mutation made after a fast computer answered while a slow one is still out', async () => {
    stageBackend({ id: LAPTOP });
    const laptop = deferred<Thread[]>();
    setBindingMock('ListThreads', async () => takePinnedBackend() === LAPTOP ? laptop.promise : threads('home-old'));
    const boot = loadThreads();
    await vi.advanceTimersByTimeAsync(0);
    removeThread('home-old');
    laptop.resolve(threads('laptop-old'));
    await boot;

    expect(ids(getThreads())).toEqual(['laptop-old']);
  });

  it('drops a former owner’s copy of a thread that moved while it was out', async () => {
    stageBackend({ id: LAPTOP });
    noteThread('moved', LAPTOP, 0);
    const laptop = deferred<Thread[]>();
    setBindingMock('ListThreads', async () => takePinnedBackend() === LAPTOP ? laptop.promise : []);
    const boot = loadThreads();
    noteThread('moved', HOME_BACKEND, 1);
    laptop.resolve([makeThread({ id: 'moved', ownershipEpoch: 0 }), makeThread({ id: 'stays' })]);
    await boot;

    expect(ids(getThreads())).toEqual(['stays']);
    expect(catalogLoadState(LAPTOP, 'threads')).toEqual(LOADED);
  });

  it('clears membership of a group deleted while it was out, on rows it had not listed', async () => {
    stageBackend({ id: LAPTOP });
    noteThreadGroup('g1', LAPTOP);
    const laptop = deferred<Thread[]>();
    setBindingMock('ListThreads', async () => takePinnedBackend() === LAPTOP ? laptop.promise : []);
    const boot = loadThreads();
    clearThreadGroupMembership('g1');
    laptop.resolve([makeThread({ id: 'member', groupId: 'g1', pinnedAt: 5, pinGroup: 1 })]);
    await boot;

    expect(getThreads().map((t) => [t.id, t.groupId, t.pinnedAt])).toEqual([['member', undefined, undefined]]);
  });

  it('forgets the reads of a computer that detaches', async () => {
    stageBackend({ id: LAPTOP });
    const laptop = deferred<Thread[]>();
    setBindingMock('ListThreads', async () => takePinnedBackend() === LAPTOP ? laptop.promise : []);
    const boot = loadThreads();
    expect(openCatalogReadsForTest(LAPTOP, 'threads')).toBe(1);
    detachBackend(LAPTOP);
    expect(openCatalogReadsForTest(LAPTOP, 'threads')).toBe(0);
    laptop.resolve(threads('gone'));
    await boot;
    expect(ids(getThreads())).toEqual([]);
  });

  it('loads through a hello’s resync while rows are being pushed', async () => {
    const list = deferred<Thread[]>();
    setBindingMock('ListThreads', () => list.promise);
    setBindingMock('ListProjects', async () => []);
    setBindingMock('ListThreadGroups', async () => []);
    refreshSidebarProjections();
    await vi.advanceTimersByTimeAsync(0);
    updateThreadTitle('t1', 'pushed');
    list.resolve(threads('t1'));
    await vi.advanceTimersByTimeAsync(0);

    expect(getThreads().map((t) => t.title)).toEqual(['pushed']);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
  });
});

describe('a project catalog read', () => {
  it('loads with the projects added, renamed and removed while it was out', async () => {
    const list = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', () => list.promise);
    const refresh = refreshProjects();
    addProjectLocal(project('added').project);
    updateProjectLocal(project('renamed', 'new name').project);
    removeProjectLocal('removed');
    list.resolve([project('renamed', 'old name'), project('removed'), project('kept')]);
    await refresh;

    expect(getProjects().map((p) => [p.project.id, p.project.name])).toEqual([
      ['added', 'added'], ['renamed', 'new name'], ['kept', 'kept'],
    ]);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
  });

  it('replays a mutation only over its own computer’s answer', async () => {
    stageBackend({ id: LAPTOP });
    const home = deferred<ProjectWithCounts[]>();
    const laptop = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', () => takePinnedBackend() === LAPTOP ? laptop.promise : home.promise);
    const refresh = refreshProjects();
    addProjectLocal(project('home-new').project);
    home.resolve([project('home-old')]);
    laptop.resolve([project('laptop-old')]);
    await refresh;

    expect(getProjects().map((p) => p.project.id)).toEqual(['home-new', 'home-old', 'laptop-old']);
    expect(catalogLoadState(LAPTOP, 'projects')).toEqual(LOADED);
  });

  it('does not duplicate a project the answer already lists', async () => {
    const list = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', () => list.promise);
    const refresh = refreshProjects();
    addProjectLocal(project('p1').project);
    list.resolve([project('p1')]);
    await refresh;
    expect(getProjects().map((p) => p.project.id)).toEqual(['p1']);
  });
});

describe('a thread group catalog read', () => {
  it('keeps a group upserted while it was out', async () => {
    const list = deferred<ThreadGroup[]>();
    setBindingMock('ListThreadGroups', () => list.promise);
    const load = loadThreadGroups();
    upsertThreadGroup(group('created'));
    list.resolve([group('existing')]);
    await load;
    expect(getThreadGroups().map((g) => g.id)).toEqual(['existing', 'created']);
  });
});

// An answer passes through several awaits on its way to the store. A mutation
// at any of those microtask boundaries must survive: before the commit it is
// replayed over the answer, after it it applies to the committed rows.
const SETTLING = Array.from({ length: 30 }, (_, index) => index);

async function afterMicrotasks(count: number): Promise<void> {
  for (let index = 0; index < count; index++) await Promise.resolve();
}

describe('a mutation while an answer settles', () => {
  it.each(SETTLING)('keeps a thread created %i microtasks after the boot answer', async (count) => {
    const list = deferred<Thread[]>();
    setBindingMock('ListThreads', () => list.promise);
    const boot = loadThreads();
    list.resolve(threads('t1'));
    await afterMicrotasks(count);
    prependThread(makeThread({ id: 'created' }));
    await boot;
    expect(ids(getThreads()).sort()).toEqual(['created', 't1']);
  });

  it.each(SETTLING)('keeps a thread created %i microtasks after a retry’s answer', async (count) => {
    setBindingMock('ListThreads', () => Promise.reject(new Error('database is busy')));
    await loadThreads().catch(() => {});
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    const list = deferred<Thread[]>();
    setBindingMock('ListThreads', () => list.promise);
    retryCatalogLoad(HOME_BACKEND);
    list.resolve(threads('t1'));
    await afterMicrotasks(count);
    prependThread(makeThread({ id: 'created' }));
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(ids(getThreads()).sort()).toEqual(['created', 't1']);
  });

  it.each(SETTLING)('keeps a project added %i microtasks after the answer', async (count) => {
    const list = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', () => list.promise);
    const refresh = refreshProjects();
    list.resolve([project('p1')]);
    await afterMicrotasks(count);
    addProjectLocal(project('created').project);
    await refresh;
    expect(getProjects().map((p) => p.project.id).sort()).toEqual(['created', 'p1']);
  });

  it.each(SETTLING)('keeps a group upserted %i microtasks after the answer', async (count) => {
    const list = deferred<ThreadGroup[]>();
    setBindingMock('ListThreadGroups', () => list.promise);
    const load = loadThreadGroups();
    list.resolve([group('existing')]);
    await afterMicrotasks(count);
    upsertThreadGroup(group('created'));
    await load;
    expect(getThreadGroups().map((g) => g.id).sort()).toEqual(['created', 'existing']);
  });
});
