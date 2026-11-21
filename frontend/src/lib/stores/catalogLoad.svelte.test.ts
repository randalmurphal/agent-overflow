import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, detachBackend, takePinnedBackend } from '../transport/backends';
import { observeBackendGeneration, setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import type { ProjectWithCounts, Thread } from '../types/models';
import {
  __catalogRetryCountForTest,
  catalogLoadState,
  CATALOG_RETRY_DELAYS_MS,
  CATALOG_RETRY_MAX_MS,
  resetCatalogLoadForTest,
  retryCatalogLoad,
  settleCatalogRead,
} from './catalogLoad.svelte';
import { catalogTreeListed, loadThreadCatalogs, resetThreadCatalogsForTest } from './threadCatalogs';
import { getThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import type { ThreadGroup } from '../types/models';
import { refreshSidebarProjections } from './eventsThreadRows';
import { getProjects, refreshProjects, resetProjectsForTest } from './projects.svelte';
import { getThreads, resetThreadsForTest } from './threads.svelte';

function project(id: string): ProjectWithCounts {
  return { project: { id, name: id, path: `/repo/${id}`, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false },
    threadCount: 0, lastActive: 0 };
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (error: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const LOADING = { phase: 'loading', error: '' };
const LOADED = { phase: 'loaded', error: '' };

beforeEach(() => {
  resetStagedBackends();
  resetProjectsForTest();
  __resetEntityIndexForTest();
  vi.useFakeTimers();
  vi.spyOn(console, 'error').mockImplementation(() => {});
  // Retry re-reads every catalog of the computer that has not loaded.
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListThreadGroups', async () => []);
});
afterEach(() => {
  resetStagedBackends();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('catalog load state', () => {
  it('marks each catalog loaded when a read commits its rows', async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListProjects', async () => [project('p1')]);
    await loadThreadCatalogs();
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADING);
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
  });

  it('marks the catalogs loaded from a hello’s sidebar refresh', async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListProjects', async () => [project('p1')]);
    setBindingMock('ListThreadGroups', async () => []);
    refreshSidebarProjections();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
  });

  it('ignores a read settled for a computer that is not attached', () => {
    settleCatalogRead('projects', 'ghost', { answered: false, error: new TransportError('method_error', 'list projects: disk I/O error'), cached: false });
    expect(__catalogRetryCountForTest()).toBe(0);
    expect(catalogLoadState('ghost', 'projects')).toEqual(LOADING);
    settleCatalogRead('projects', 'ghost', { answered: true });
    expect(catalogLoadState('ghost', 'projects')).toEqual(LOADING);
  });

  it('shows a failed read, keeps retrying on the schedule, and loads with the rows', async () => {
    let failures = 2;
    const list = setBindingMock('ListThreads', async () => {
      if (failures > 0) {
        failures--;
        throw new TransportError('method_error', 'list threads: database disk image is malformed');
      }
      return [makeThread({ id: 't1' })];
    });
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADING);

    // Rows an earlier read left in the store may still be served; either
    // way this computer failed to answer.
    await loadThreadCatalogs().catch(() => null);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(catalogLoadState(HOME_BACKEND, 'threads').error).toMatch(/database disk image is malformed/i);
    expect(__catalogRetryCountForTest()).toBe(1);

    await vi.advanceTimersByTimeAsync(999);
    expect(list).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(list).toHaveBeenCalledTimes(2);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');

    await vi.advanceTimersByTimeAsync(1999);
    expect(list).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(list).toHaveBeenCalledTimes(3);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(getThreads().map((thread) => thread.id)).toEqual(['t1']);
    expect(__catalogRetryCountForTest()).toBe(0);

    // Loaded is terminal for the schedule: nothing further is read.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(list).toHaveBeenCalledTimes(3);
  });

  it('retries a read that missed the startup deadline and loads its answer', async () => {
    let calls = 0;
    setBindingMock('ListThreads', () => {
      calls++;
      return calls === 1 ? new Promise<Thread[]>(() => {}) : Promise.resolve([makeThread({ id: 't1' })]);
    });
    // The boot read may still paint a replica catalog; either way the
    // computer has not answered, so its catalog is loading, not loaded.
    const boot = loadThreadCatalogs().catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(2500);
    const outcome = await boot;
    if (!Array.isArray(outcome)) expect(outcome).toBeInstanceOf(ReadDeadlineError);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADING);
    expect(__catalogRetryCountForTest()).toBe(1);

    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toBe(2);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(getThreads().map((thread) => thread.id)).toEqual(['t1']);
  });

  it('lets a slow retry answer instead of superseding it with the next attempt', async () => {
    const slow = deferred<ProjectWithCounts[]>();
    let calls = 0;
    setBindingMock('ListProjects', () => {
      calls++;
      return calls === 1 ? new Promise<ProjectWithCounts[]>(() => {}) : slow.promise;
    });
    const boot = refreshProjects();
    await vi.advanceTimersByTimeAsync(2500);
    await boot;
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toBe(2);

    // Well past the startup deadline and every scheduled delay.
    await vi.advanceTimersByTimeAsync(30_000);
    expect(calls).toBe(2);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADING);

    slow.resolve([project('p1')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
  });

  it('keeps loading through temporarily_unavailable, backing off 1, 2, 4, 8 s, then every 10 s', async () => {
    const list = setBindingMock('ListProjects', async () => {
      throw new TransportError('temporarily_unavailable', 'backend is starting');
    });
    await refreshProjects();
    expect(list).toHaveBeenCalledTimes(1);
    const delays = [...CATALOG_RETRY_DELAYS_MS, CATALOG_RETRY_MAX_MS, CATALOG_RETRY_MAX_MS];
    expect(delays).toEqual([1000, 2000, 4000, 8000, 10_000, 10_000]);
    for (const [index, delay] of delays.entries()) {
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(list).toHaveBeenCalledTimes(index + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(list).toHaveBeenCalledTimes(index + 2);
      expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADING);
      expect(__catalogRetryCountForTest()).toBe(1);
    }
  });

  it('does not stack retries when repeated hellos re-read a failing catalog', async () => {
    const list = setBindingMock('ListProjects', async () => {
      throw new TransportError('method_error', 'list projects: disk I/O error');
    });
    await refreshProjects();
    // Each connection edge and hello refreshes the lists.
    await refreshProjects();
    await refreshProjects();
    await refreshProjects();
    expect(list).toHaveBeenCalledTimes(4);
    expect(__catalogRetryCountForTest()).toBe(1);

    await vi.advanceTimersByTimeAsync(1000);
    expect(list).toHaveBeenCalledTimes(5);
    expect(__catalogRetryCountForTest()).toBe(1);
    // The hellos did not advance the schedule: the next delay is the second.
    await vi.advanceTimersByTimeAsync(1999);
    expect(list).toHaveBeenCalledTimes(5);
    await vi.advanceTimersByTimeAsync(1);
    expect(list).toHaveBeenCalledTimes(6);
  });

  it('clears a detached computer’s timers and state, including a retry in flight', async () => {
    stageBackend({ id: 'gpu' });
    const inFlight = deferred<ProjectWithCounts[]>();
    let gpuCalls = 0;
    setBindingMock('ListProjects', async () => {
      if (takePinnedBackend() !== 'gpu') return [project('mac')];
      gpuCalls++;
      if (gpuCalls === 2) return inFlight.promise;
      throw new TransportError('method_error', 'list projects: disk I/O error');
    });
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    expect(catalogLoadState('gpu', 'projects').phase).toBe('failed');
    expect(__catalogRetryCountForTest()).toBe(1);

    detachBackend('gpu');
    expect(__catalogRetryCountForTest()).toBe(0);
    expect(catalogLoadState('gpu', 'projects')).toEqual(LOADING);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(gpuCalls).toBe(1);

    // Re-attached, failing again, then detached while the retry is out.
    stageBackend({ id: 'gpu' });
    gpuCalls = 0;
    await refreshProjects();
    await vi.advanceTimersByTimeAsync(1000);
    expect(gpuCalls).toBe(2);
    detachBackend('gpu');
    inFlight.reject(new TransportError('method_error', 'list projects: disk I/O error'));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(gpuCalls).toBe(2);
    expect(__catalogRetryCountForTest()).toBe(0);
    expect(catalogLoadState('gpu', 'projects')).toEqual(LOADING);
  });

  it('shows a scope refusal and waits for Retry instead of asking again', async () => {
    let refused = true;
    const list = setBindingMock('ListProjects', async () => {
      if (refused) throw new TransportError('scope_required', 'not authorized');
      return [project('p1')];
    });
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects').phase).toBe('failed');
    expect(__catalogRetryCountForTest()).toBe(0);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(list).toHaveBeenCalledTimes(1);

    refused = false;
    retryCatalogLoad(HOME_BACKEND);
    expect(list).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
  });

  it('retries now on Retry and starts the schedule over', async () => {
    const list = setBindingMock('ListProjects', async () => {
      throw new TransportError('method_error', 'list projects: disk I/O error');
    });
    await refreshProjects();
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(2000);
    expect(list).toHaveBeenCalledTimes(3);

    retryCatalogLoad(HOME_BACKEND);
    expect(list).toHaveBeenCalledTimes(4);
    await vi.advanceTimersByTimeAsync(0);
    expect(__catalogRetryCountForTest()).toBe(1);
    // First delay again, not the fourth.
    await vi.advanceTimersByTimeAsync(1000);
    expect(list).toHaveBeenCalledTimes(5);
  });

  it('waits for the connection instead of retrying an offline or starting computer', async () => {
    const list = setBindingMock('ListProjects', async () => {
      throw new DisconnectedError('backend is starting');
    });
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADING);
    expect(__catalogRetryCountForTest()).toBe(0);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(list).toHaveBeenCalledTimes(1);
  });

  it("shows a later refresh's failure beside the rows it keeps, and retries it", async () => {
    let fail = false;
    const list = setBindingMock('ListProjects', async () => {
      if (fail) throw new TransportError('method_error', 'list projects: disk I/O error');
      return [project('p1')];
    });
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    fail = true;
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual({ phase: 'failed', error: 'Disk I/O error.' });
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
    expect(__catalogRetryCountForTest()).toBe(1);
    fail = false;
    await vi.advanceTimersByTimeAsync(1000);
    expect(list).toHaveBeenCalledTimes(3);
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('retries a loaded catalog quietly through temporarily_unavailable', async () => {
    let unavailable = false;
    const list = setBindingMock('ListProjects', async () => {
      if (unavailable) throw new TransportError('temporarily_unavailable', 'backend is restarting');
      return [project('p1')];
    });
    await refreshProjects();
    unavailable = true;
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(1);
    unavailable = false;
    await vi.advanceTimersByTimeAsync(1000);
    expect(list).toHaveBeenCalledTimes(3);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('waits for the late outcome of a loaded catalog past its deadline, without a retry', async () => {
    const late = deferred<ProjectWithCounts[]>();
    let calls = 0;
    setBindingMock('ListProjects', () => (++calls === 1 ? Promise.resolve([project('p1')]) : late.promise));
    await refreshProjects();
    const refresh = refreshProjects();
    await vi.advanceTimersByTimeAsync(2500);
    await refresh;
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
    late.reject(new TransportError('method_error', 'list projects: disk I/O error'));
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'projects').phase).toBe('failed');
    expect(__catalogRetryCountForTest()).toBe(1);
    expect(calls).toBe(2);
  });

  it("shows a loaded thread list's failed refresh beside its rows and retries the pair", async () => {
    let fail = false;
    const threads = setBindingMock('ListThreads', async () => {
      if (fail) throw new TransportError('method_error', 'list threads: disk I/O error');
      return [makeThread({ id: 't1' })];
    });
    const groups = setBindingMock('ListThreadGroups', async () => []);
    await loadThreadCatalogs();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    fail = true;
    await loadThreadCatalogs();
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual({ phase: 'failed', error: 'Disk I/O error.' });
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADED);
    expect(getThreads().map((thread) => thread.id)).toEqual(['t1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(__catalogRetryCountForTest()).toBe(1);
    fail = false;
    await vi.advanceTimersByTimeAsync(1000);
    expect([threads, groups].map((mock) => mock.mock.calls.length)).toEqual([3, 3]);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
  });
});

describe('a history generation observed mid-session', () => {
  beforeEach(() => {
    resetCatalogLoadForTest();
    setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
  });

  it('puts every catalog of that computer back to loading and re-reads it', async () => {
    const threads = setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    const groups = setBindingMock('ListThreadGroups', async () => []);
    const projects = setBindingMock('ListProjects', async () => [project('p1')]);
    await loadThreadCatalogs();
    await refreshProjects();
    const answers = deferred<Thread[]>();
    threads.mockImplementation(() => answers.promise);

    expect(observeBackendGeneration('gen-2', HOME_BACKEND)).toBe(true);
    expect((['projects', 'threads', 'groups'] as const).map((kind) => catalogLoadState(HOME_BACKEND, kind).phase))
      .toEqual(['loading', 'loading', 'loading']);
    await vi.advanceTimersByTimeAsync(0);
    expect([threads, groups, projects].map((mock) => mock.mock.calls.length)).toEqual([2, 2, 2]);
    // The old history's rows stay until the new history's answer replaces them.
    expect(getThreads().map((thread) => thread.id)).toEqual(['t1']);
    answers.resolve([makeThread({ id: 't2' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(getThreads().map((thread) => thread.id)).toEqual(['t2']);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
  });

  it("withholds the re-read's answered half when its partner fails: the re-read is not a retry", async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    const groups = setBindingMock('ListThreadGroups', async () => []);
    await loadThreadCatalogs();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    setBindingMock('ListThreads', async () => [makeThread({ id: 't2' })]);
    groups.mockImplementation(async () => { throw new TransportError('method_error', 'list thread groups: disk I/O error'); });
    observeBackendGeneration('gen-2', HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    expect(getThreads().map((thread) => thread.id)).toEqual(['t1']);
    // The retry is one: its half publishes alone when the partner fails again.
    await vi.advanceTimersByTimeAsync(1000);
    expect(getThreads().map((thread) => thread.id)).toEqual(['t2']);
  });

  it('shows and retries a re-read that fails, instead of keeping the old history loaded', async () => {
    let fail = false;
    const threads = setBindingMock('ListThreads', async () => {
      if (fail) throw new TransportError('method_error', 'list threads: disk I/O error');
      return [makeThread({ id: 't1' })];
    });
    await loadThreadCatalogs();
    fail = true;
    observeBackendGeneration('gen-2', HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual({ phase: 'failed', error: 'Disk I/O error.' });
    expect(__catalogRetryCountForTest()).toBeGreaterThan(0);
    fail = false;
    await vi.advanceTimersByTimeAsync(1000);
    expect(threads).toHaveBeenCalledTimes(3);
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
  });
});

describe('the sidebar listing', () => {
  const group = (id: string): ThreadGroup => ({ id, projectId: 'p1', name: id, createdAt: 0, updatedAt: 0 });
  const offline = () => { throw new DisconnectedError('Computer is offline.'); };

  beforeEach(() => {
    resetCatalogLoadForTest();
    resetThreadGroupsForTest();
  });

  it('lists a computer once its threads answered and no read of its threads or groups is out', async () => {
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const round = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    await round;
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    groups.resolve([group('g1')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('keeps a computer unlisted while a read is past its deadline, until the late answer', async () => {
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const round = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    expect((await round).map((thread) => thread.id)).toEqual(['t1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    groups.resolve([group('g1')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
  });

  it('lists a computer offline with its thread rows kept', async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    await loadThreadCatalogs();
    resetThreadCatalogsForTest();
    setBindingMock('ListThreads', offline);
    setBindingMock('ListThreadGroups', offline);
    await loadThreadCatalogs();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('holds a computer offline with nothing kept until both reads of its connection answer', async () => {
    setBindingMock('ListThreads', offline);
    setBindingMock('ListThreadGroups', offline);
    await expect(loadThreadCatalogs()).rejects.toBeInstanceOf(DisconnectedError);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);

    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const round = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    groups.resolve([]);
    await round;
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('does not list a computer that has only groups kept', async () => {
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await loadThreadCatalogs();
    resetThreadCatalogsForTest();
    resetThreadsForTest();
    setBindingMock('ListThreads', offline);
    setBindingMock('ListThreadGroups', offline);
    await expect(loadThreadCatalogs()).rejects.toBeInstanceOf(DisconnectedError);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
  });

  it('keeps a computer whose thread read fails on its error row until a retry fails again, then lists its groups beside it', async () => {
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await expect(loadThreadCatalogs()).rejects.toBeInstanceOf(TransportError);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('keeps a computer whose group read fails on its error row until the retry brings both', async () => {
    let fail = true;
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', async () => {
      if (fail) throw new TransportError('method_error', 'list thread groups: disk I/O error');
      return [group('g1')];
    });
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    fail = false;
    await vi.advanceTimersByTimeAsync(1000);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
  });

  it('lists a computer whose group read is refused for want of a scope beside its threads', async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', async () => { throw new TransportError('scope_required', 'not granted'); });
    await loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it("forgets a detached computer's listing", async () => {
    stageBackend({ id: 'gpu' });
    await loadThreadCatalogs();
    expect(catalogTreeListed('gpu')).toBe(true);
    detachBackend('gpu');
    expect(catalogTreeListed('gpu')).toBe(false);
  });
});

describe('the group catalog', () => {
  beforeEach(() => {
    resetCatalogLoadForTest();
    resetThreadGroupsForTest();
  });

  it('shows a failed group read and retries it with the threads on the schedule until it loads', async () => {
    let fail = true;
    const threads = setBindingMock('ListThreads', async () => []);
    const list = setBindingMock('ListThreadGroups', async () => {
      if (fail) throw new TransportError('method_error', 'list thread groups: disk I/O error');
      return [{ id: 'g1', projectId: 'p1', name: 'g1', createdAt: 0, updatedAt: 0 }];
    });
    await loadThreadCatalogs();
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual({ phase: 'failed', error: 'Disk I/O error.' });
    expect(__catalogRetryCountForTest()).toBe(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect([threads, list].map((mock) => mock.mock.calls.length)).toEqual([2, 2]);
    fail = false;
    await vi.advanceTimersByTimeAsync(2000);
    expect(list).toHaveBeenCalledTimes(3);
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADED);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('keeps the retry armed when the threads answer and the groups fail', async () => {
    const list = setBindingMock('ListThreadGroups', async () => {
      throw new TransportError('method_error', 'list thread groups: disk I/O error');
    });
    await loadThreadCatalogs();
    expect(catalogLoadState(HOME_BACKEND, 'threads')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(list).toHaveBeenCalledTimes(2);
    expect(__catalogRetryCountForTest()).toBe(1);
  });

  // G2: an attempt is in flight until its pair concludes.
  const slowerThanEveryRetry = CATALOG_RETRY_MAX_MS + 2000;
  const answerAfter = <T>(ms: number, value: T) => new Promise<T>((resolve) => { setTimeout(() => resolve(value), ms); });
  const groupRow = (id: string): ThreadGroup => ({ id, projectId: 'p1', name: id, createdAt: 0, updatedAt: 0 });

  it.each(['threads', 'groups'] as const)(
    'lists a computer after one retry when its %s answer is slower than the longest retry interval',
    async (slow) => {
      const threads = setBindingMock('ListThreads', () => answerAfter(slow === 'threads' ? slowerThanEveryRetry : 0, [makeThread({ id: 't1' })]));
      const groups = setBindingMock('ListThreadGroups', () => answerAfter(slow === 'groups' ? slowerThanEveryRetry : 0, [groupRow('g1')]));
      void loadThreadCatalogs().catch(() => {});
      // The startup deadline, then the first retry.
      await vi.advanceTimersByTimeAsync(2500 + 1000);
      const slowMock = slow === 'threads' ? threads : groups;
      expect(slowMock).toHaveBeenCalledTimes(2);
      expect(__catalogRetryCountForTest()).toBe(0);
      await vi.advanceTimersByTimeAsync(slowerThanEveryRetry);
      expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
      expect(slowMock).toHaveBeenCalledTimes(2);
      expect(catalogLoadState(HOME_BACKEND, slow)).toEqual(LOADED);
      expect(__catalogRetryCountForTest()).toBe(0);
    },
  );

  it("arms no retry while the attempt's slow half is out, though its other half failed, and retries once it lands", async () => {
    let calls = 0;
    const threads = setBindingMock('ListThreads', async () => {
      calls++;
      if (calls > 1) throw new TransportError('method_error', 'list threads: disk I/O error');
      return [makeThread({ id: 't1' })];
    });
    const groups = setBindingMock('ListThreadGroups', () => answerAfter(slowerThanEveryRetry, [groupRow('g1')]));
    void loadThreadCatalogs().catch(() => {});
    await vi.advanceTimersByTimeAsync(2500 + 1000);
    expect([threads, groups].map((mock) => mock.mock.calls.length)).toEqual([2, 2]);
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(__catalogRetryCountForTest()).toBe(0);
    await vi.advanceTimersByTimeAsync(slowerThanEveryRetry);
    expect(groups).toHaveBeenCalledTimes(2);
    // The retry's thread half failed again, so its groups list alone.
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(__catalogRetryCountForTest()).toBe(1);
  });

  it('starts nothing on Retry while an attempt is in flight, and lists when its slow answer lands', async () => {
    // The projects reader has its own catalog; Retry reads it, and it answers.
    setBindingMock('ListProjects', async () => [project('p1')]);
    let calls = 0;
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    const groups = setBindingMock('ListThreadGroups', () => {
      calls++;
      if (calls === 1) return Promise.reject(new TransportError('method_error', 'list thread groups: disk I/O error'));
      if (calls === 2) return answerAfter(slowerThanEveryRetry, [groupRow('g1')]);
      return Promise.reject(new TransportError('method_error', 'list thread groups: disk I/O error'));
    });
    await loadThreadCatalogs();
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
    await vi.advanceTimersByTimeAsync(1000);
    expect(groups).toHaveBeenCalledTimes(2);

    retryCatalogLoad(HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    expect(groups).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(slowerThanEveryRetry);
    expect(groups).toHaveBeenCalledTimes(2);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('restarts the schedule on a Retry pressed while an attempt is in flight', async () => {
    setBindingMock('ListProjects', async () => [project('p1')]);
    let calls = 0;
    const groups = setBindingMock('ListThreadGroups', () => {
      calls++;
      return calls === 3
        ? answerAfter(1, []).then(() => { throw new TransportError('method_error', 'list thread groups: disk I/O error'); })
        : Promise.reject(new TransportError('method_error', 'list thread groups: disk I/O error'));
    });
    await loadThreadCatalogs();
    // The schedule moves on: retries at 1 s, then 2 s later.
    await vi.advanceTimersByTimeAsync(1000);
    expect(groups).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(2000);
    expect(groups).toHaveBeenCalledTimes(3);
    // The third read is out; Retry starts nothing but resets the schedule,
    // so the next retry comes 1 s after it fails, not 4 s.
    retryCatalogLoad(HOME_BACKEND);
    expect(groups).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(groups).toHaveBeenCalledTimes(4);
  });

  it('consumes the retry an attempt serves, so a round that takes the attempt over is not retried over', async () => {
    const groups = setBindingMock('ListThreadGroups', async () => {
      throw new TransportError('method_error', 'list thread groups: disk I/O error');
    });
    await loadThreadCatalogs();
    expect(__catalogRetryCountForTest()).toBe(1);
    groups.mockImplementation(() => new Promise<ThreadGroup[]>(() => {}));
    await vi.advanceTimersByTimeAsync(1000);
    expect(groups).toHaveBeenCalledTimes(2);
    // A gap resync takes the attempt's reads over and waits for its answers.
    groups.mockImplementation(() => answerAfter(slowerThanEveryRetry, [groupRow('g1')]));
    void loadThreadCatalogs(HOME_BACKEND).catch(() => {});
    await vi.advanceTimersByTimeAsync(slowerThanEveryRetry);
    expect(groups).toHaveBeenCalledTimes(3);
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('settles a read that fails after its deadline as the failure it is', async () => {
    const late = deferred<ThreadGroup[]>();
    setBindingMock('ListThreadGroups', () => late.promise);
    const round = loadThreadCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await round;
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADING);
    late.reject(new TransportError('timeout', 'RPC 7 timed out after 60000ms'));
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
  });
});
