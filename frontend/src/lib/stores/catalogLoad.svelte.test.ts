import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { makeThread } from '../../test/helpers/chat';
import { HOME_BACKEND, detachBackend, takePinnedBackend } from '../transport/backends';
import { __resetEntityIndexForTest } from '../transport/entityIndex';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import type { ProjectWithCounts, Thread } from '../types/models';
import {
  __catalogRetryCountForTest,
  catalogLoadState,
  catalogTreeListed,
  CATALOG_RETRY_DELAYS_MS,
  CATALOG_RETRY_MAX_MS,
  resetCatalogLoadForTest,
  retryCatalogLoad,
  settleCatalogRead,
} from './catalogLoad.svelte';
import { getThreadGroups, loadThreadGroups, resetThreadGroupsForTest } from './threadGroups.svelte';
import type { ThreadGroup } from '../types/models';
import { refreshSidebarProjections } from './eventsThreadRows';
import { getProjects, refreshProjects, resetProjectsForTest } from './projects.svelte';
import { getThreads, loadThreads, resetThreadsForTest } from './threads.svelte';

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
    await loadThreads();
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
    await loadThreads().catch(() => null);
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
    const boot = loadThreads().catch((error: unknown) => error);
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

  it('keeps a loaded catalog loaded when a later refresh fails', async () => {
    let fail = false;
    setBindingMock('ListProjects', async () => {
      if (fail) throw new TransportError('method_error', 'list projects: disk I/O error');
      return [project('p1')];
    });
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    fail = true;
    await refreshProjects();
    expect(catalogLoadState(HOME_BACKEND, 'projects')).toEqual(LOADED);
    expect(__catalogRetryCountForTest()).toBe(0);
    expect(getProjects().map((row) => row.project.id)).toEqual(['p1']);
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
    const groupRead = loadThreadGroups();
    await loadThreads();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    groups.resolve([group('g1')]);
    await groupRead;
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('keeps a computer unlisted while a read is past its deadline, until the late answer', async () => {
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const groupRead = loadThreadGroups().catch((error: unknown) => error);
    await loadThreads();
    await vi.advanceTimersByTimeAsync(2500);
    expect(await groupRead).toBeInstanceOf(ReadDeadlineError);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    groups.resolve([group('g1')]);
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
  });

  it('lists a computer offline with its thread rows kept, with or without groups', async () => {
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    await loadThreads();
    resetCatalogLoadForTest();
    setBindingMock('ListThreads', offline);
    setBindingMock('ListThreadGroups', offline);
    await loadThreads();
    await expect(loadThreadGroups()).rejects.toBeInstanceOf(DisconnectedError);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('holds a computer offline with nothing kept until both reads of its connection answer', async () => {
    setBindingMock('ListThreads', async () => []);
    await loadThreads();
    resetCatalogLoadForTest();
    resetThreadsForTest();
    setBindingMock('ListThreads', offline);
    setBindingMock('ListThreadGroups', offline);
    await expect(loadThreads()).rejects.toBeInstanceOf(DisconnectedError);
    await expect(loadThreadGroups()).rejects.toBeInstanceOf(DisconnectedError);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);

    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => [makeThread({ id: 't1' })]);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const groupRead = loadThreadGroups();
    await loadThreads();
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
    groups.resolve([]);
    await groupRead;
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('does not list a computer that has only groups kept', async () => {
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await loadThreadGroups();
    resetCatalogLoadForTest();
    resetThreadsForTest();
    setBindingMock('ListThreads', offline);
    setBindingMock('ListThreadGroups', offline);
    await expect(loadThreads()).rejects.toBeInstanceOf(DisconnectedError);
    await loadThreadGroups();
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
  });

  it('lists a computer whose thread read fails with an error of its own beside its groups', async () => {
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    setBindingMock('ListThreadGroups', async () => [group('g1')]);
    await expect(loadThreads()).rejects.toBeInstanceOf(TransportError);
    await loadThreadGroups();
    expect(catalogLoadState(HOME_BACKEND, 'threads').phase).toBe('failed');
    expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
  });

  it('ignores a computer that is not attached and forgets a detached one', async () => {
    settleCatalogRead('threads', 'ghost', { answered: true });
    expect(catalogTreeListed('ghost')).toBe(false);

    stageBackend({ id: 'gpu' });
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListThreadGroups', async () => []);
    await loadThreads();
    await loadThreadGroups();
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

  it('shows a failed group read and retries it on the schedule until it loads', async () => {
    let fail = true;
    const list = setBindingMock('ListThreadGroups', async () => {
      if (fail) throw new TransportError('method_error', 'list thread groups: disk I/O error');
      return [{ id: 'g1', projectId: 'p1', name: 'g1', createdAt: 0, updatedAt: 0 }];
    });
    await expect(loadThreadGroups()).rejects.toBeInstanceOf(TransportError);
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual({ phase: 'failed', error: 'Disk I/O error.' });
    expect(__catalogRetryCountForTest()).toBe(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(list).toHaveBeenCalledTimes(2);
    fail = false;
    await vi.advanceTimersByTimeAsync(2000);
    expect(list).toHaveBeenCalledTimes(3);
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADED);
    expect(getThreadGroups().map((row) => row.id)).toEqual(['g1']);
    expect(__catalogRetryCountForTest()).toBe(0);
  });

  it('settles a read that fails after its deadline as the failure it is', async () => {
    const late = deferred<ThreadGroup[]>();
    setBindingMock('ListThreadGroups', () => late.promise);
    const read = loadThreadGroups().catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(2500);
    expect(await read).toBeInstanceOf(ReadDeadlineError);
    expect(catalogLoadState(HOME_BACKEND, 'groups')).toEqual(LOADING);
    late.reject(new TransportError('timeout', 'RPC 7 timed out after 60000ms'));
    await vi.advanceTimersByTimeAsync(0);
    expect(catalogLoadState(HOME_BACKEND, 'groups').phase).toBe('failed');
  });
});
