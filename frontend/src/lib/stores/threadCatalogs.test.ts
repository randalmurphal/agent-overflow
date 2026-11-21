import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, detachBackend, takePinnedBackend } from '../transport/backends';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import { observeBackendGeneration, setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { TransportError } from '../transport/wsClient';
import type { Thread, ThreadGroup } from '../types/models';
import { catalogTreeListed, resetCatalogLoadForTest } from './catalogLoad.svelte';
import { openCatalogReadsForTest } from './computerCatalogReads';
import { resetProjectsForTest } from './projects.svelte';
import { getThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import { getThreadById, loadThreads, resetThreadsForTest } from './threads.svelte';
import { loadThreadCatalogs } from './threadCatalogs';

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (error: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const group = (id: string): ThreadGroup => ({ id, projectId: 'project-1', name: id, createdAt: 0, updatedAt: 0 });
const groupIds = () => getThreadGroups().map((row) => row.id);
const groupOf = (id: string) => getThreadById(id)?.groupId;

beforeEach(() => {
  resetStagedBackends();
  resetCatalogLoadForTest();
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
  expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
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
  });

  // Every way the thread read can stop releases the group answer waiting
  // for it: nothing is left recording, and the group rows commit unless the
  // read they belong to is stale too.
  describe('releases a group answer waiting for its thread read when that read', () => {
    async function groupsWaitForThreads(threads: Promise<Thread[]>): Promise<{ round: Promise<readonly Thread[]> }> {
      await listHome();
      setBindingMock('ListThreads', () => threads);
      setBindingMock('ListThreadGroups', async () => [group('g2')]);
      const round = loadThreadCatalogs().catch(() => [] as readonly Thread[]);
      await vi.advanceTimersByTimeAsync(0);
      expect(groupIds()).toEqual([]);
      return { round };
    }
    function expectNothingRecording(backend = HOME_BACKEND): void {
      expect(openCatalogReadsForTest(backend, 'threads')).toBe(0);
      expect(openCatalogReadsForTest(backend, 'groups')).toBe(0);
    }

    it('fails on time', async () => {
      const threads = deferred<Thread[]>();
      const { round } = await groupsWaitForThreads(threads.promise);
      threads.reject(new TransportError('method_error', 'list threads: disk I/O error'));
      await round;
      expect(groupIds()).toEqual(['g2']);
      expect(groupOf('t1')).toBeUndefined();
      expectNothingRecording();
    });

    it('answers late', async () => {
      const threads = deferred<Thread[]>();
      const { round } = await groupsWaitForThreads(threads.promise);
      await vi.advanceTimersByTimeAsync(2500);
      await round;
      expect(groupIds()).toEqual([]);
      threads.resolve([makeThread({ id: 't1', groupId: 'g2' })]);
      await vi.advanceTimersByTimeAsync(0);
      expect(groupIds()).toEqual(['g2']);
      expect(groupOf('t1')).toBe('g2');
      expectNothingRecording();
    });

    it('fails late', async () => {
      const threads = deferred<Thread[]>();
      const { round } = await groupsWaitForThreads(threads.promise);
      await vi.advanceTimersByTimeAsync(2500);
      await round;
      expect(groupIds()).toEqual([]);
      threads.reject(new TransportError('timeout', 'RPC 7 timed out after 60000ms'));
      await vi.advanceTimersByTimeAsync(0);
      expect(groupIds()).toEqual(['g2']);
      expectNothingRecording();
    });

    it('is superseded by a newer read', async () => {
      const threads = deferred<Thread[]>();
      await groupsWaitForThreads(threads.promise);
      setBindingMock('ListThreads', async () => [makeThread({ id: 't1', groupId: 'g2' })]);
      const newer = loadThreads();
      expect(groupIds()).toEqual(['g2']);
      await newer;
      threads.resolve([makeThread({ id: 't1' })]);
      await vi.advanceTimersByTimeAsync(0);
      expect(groupOf('t1')).toBe('g2');
      expectNothingRecording();
    });

    it("ends because its computer's history changed before the commit", async () => {
      stageBackend({ id: 'gpu' });
      setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
      setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu' ? [] : [makeThread({ id: 't1' })]));
      setBindingMock('ListThreadGroups', async () => []);
      await loadThreadCatalogs();
      expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
      // Home answers both at once; the other computer's thread answer is
      // still out when home's history changes.
      const gpuThreads = deferred<Thread[]>();
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuThreads.promise : Promise.resolve([makeThread({ id: 't1', groupId: 'g2' })])));
      setBindingMock('ListThreadGroups', async () => (takePinnedBackend() === 'gpu' ? [] : [group('g2')]));
      const round = loadThreadCatalogs();
      await vi.advanceTimersByTimeAsync(0);
      setBackendIdentityFromBootstrap('home-id', 'gen-2', 'Mac', HOME_BACKEND);
      gpuThreads.resolve([]);
      await round;
      // Both of home's answers describe the old history: neither commits.
      expect(groupIds()).toEqual([]);
      expect(groupOf('t1')).toBeUndefined();
      expectNothingRecording();
    });

    it('belongs to a connection that detached, and the computer is back', async () => {
      stageBackend({ id: 'gpu' });
      setBindingMock('ListThreads', async () => []);
      setBindingMock('ListThreadGroups', async () => []);
      await loadThreadCatalogs();
      expect(catalogTreeListed('gpu')).toBe(true);
      const oldThreads = deferred<Thread[]>();
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? oldThreads.promise : Promise.resolve([])));
      setBindingMock('ListThreadGroups', async () => (takePinnedBackend() === 'gpu' ? [group('old')] : []));
      void loadThreadCatalogs().catch(() => {});
      await vi.advanceTimersByTimeAsync(0);
      detachBackend('gpu');
      stageBackend({ id: 'gpu' });
      setBindingMock('ListThreads', async () => []);
      setBindingMock('ListThreadGroups', async () => (takePinnedBackend() === 'gpu' ? [group('new')] : []));
      await loadThreadCatalogs();
      expect(groupIds()).toEqual(['new']);
      // The old connection's thread read ends; its group answer, waiting for
      // it, describes a connection that is gone.
      oldThreads.resolve([]);
      await vi.advanceTimersByTimeAsync(0);
      expect(groupIds()).toEqual(['new']);
      expectNothingRecording('gpu');
    });
  });
});

// The boot's layout restore validates saved panes against the thread rows
// the round resolves with; a pane whose thread is missing is dropped.
describe('the thread rows a round resolves with', () => {
  it("include an unlisted computer's thread answer while its group read is still out", async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', () => new Promise<ThreadGroup[]>(() => {}));
    expect((await loadThreadCatalogs()).map((thread) => thread.id)).toEqual(['t1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
  });

  it("include a listed computer's on-time thread answer when its group read passes the deadline", async () => {
    // Listed through a failed thread read beside its groups, as a computer
    // whose ListThreads errored at boot is when the restore runs again.
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await loadThreadCatalogs().catch(() => {});
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1', groupId: 'g1' })]);
    setBindingMock('ListThreadGroups', () => new Promise<ThreadGroup[]>(() => {}));
    const round = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    expect((await round).map((thread) => thread.id)).toEqual(['t1']);
  });
});

describe('a history generation observed mid-session', () => {
  it('re-reads every catalog of that computer, loaded or not', async () => {
    setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
    const threads = setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    const groups = setBindingMock('ListThreadGroups', async () => [group('g1')]);
    const projects = setBindingMock('ListProjects', async () => []);
    await loadThreadCatalogs();
    expect([threads, groups, projects].map((mock) => mock.mock.calls.length)).toEqual([1, 1, 0]);

    expect(observeBackendGeneration('gen-2', HOME_BACKEND)).toBe(true);
    await vi.advanceTimersByTimeAsync(0);
    expect([threads, groups, projects].map((mock) => mock.mock.calls.length)).toEqual([2, 2, 1]);
  });
});
