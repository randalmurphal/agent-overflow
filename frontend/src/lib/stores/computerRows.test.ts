import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { deferred } from '../../test/helpers/providerAccounts';
import { takePinnedBackend, detachBackend } from '../transport/backends';
import { __resetEntityIndexForTest, currentThreadRow, noteThread, projectBackend, resolveThreadBackend } from '../transport/entityIndex';
import { addProjectLocal, getProjects, isLoaded, refreshProjects, resetProjectsForTest } from './projects.svelte';
import { getToasts } from './toast.svelte';
import { loadThreads, readThreadRows } from './threads.svelte';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { preferredProjectTarget, rememberProjectTarget } from './projectTargets';
import { readComputerRows, retainUnavailableComputerRows } from './computerRows';
import type { ProjectWithCounts, Thread } from '../types/models';

function row(id: string): ProjectWithCounts {
  return { project: { id, name: id, path: '/same/path', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false,
    remoteURL: 'https://example.test/owner/repo.git' }, threadCount: 0, lastActive: 0 };
}
beforeEach(() => { resetStagedBackends(); resetProjectsForTest(); __resetEntityIndexForTest(); });
afterEach(() => { resetStagedBackends(); vi.useRealTimers(); });

describe('unavailable computer catalogs', () => {
  it.each([false, true])('admits only the newest owner of a moved thread (home owns it: %s)', async (homeOwns) => {
    stageBackend({ id: 'gpu' });
    const result = await readComputerRows(async () => {
      const home = takePinnedBackend() !== 'gpu';
      return [{ id: 'moved', ownershipEpoch: home === homeOwns ? 2 : 1 }] as Thread[];
    }, (row, backend) => { noteThread(row.id, backend, row.ownershipEpoch); }, undefined, currentThreadRow);
    expect(result!.rows).toEqual([{ id: 'moved', ownershipEpoch: 2 }]);
    expect(resolveThreadBackend('moved')).toBe(homeOwns ? '' : 'gpu');
  });

  it('retains a failing computer, applies deletions from one that answered, and recovers on reconnect', async () => {
    stageBackend({ id: 'gpu' });
    let offline = false;
    let local = [row('mac')];
    setBindingMock('ListProjects', async () => {
      if (takePinnedBackend() === 'gpu') {
        if (offline) throw new Error('offline');
        return [row('gpu')];
      }
      return local;
    });
    await refreshProjects();
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['mac', 'gpu']);
    expect(projectBackend('gpu')).toBe('gpu');
    offline = true;
    local = [];
    await refreshProjects();
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['gpu']);
    offline = false;
    local = [row('new-mac')];
    await refreshProjects();
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['new-mac', 'gpu']);
  });

  it('drops late answers from a computer removed during the read', async () => {
    stageBackend({ id: 'gpu' });
    const remote = deferred<string[]>();
    const read = readComputerRows(async () => takePinnedBackend() === 'gpu' ? remote.promise : ['mac'], () => {});
    detachBackend('gpu');
    remote.resolve(['removed']);
    const result = await read;
    expect(result!.rows).toEqual(['mac']);
    expect(retainUnavailableComputerRows(['old-gpu'], result!, () => 'gpu')).toEqual(['mac']);
  });

  it('does not hold a healthy computer behind the first host’s cold-start dial', async () => {
    vi.useFakeTimers();
    stageBackend({ id: 'gpu' });
    const home = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', async () => takePinnedBackend() === 'gpu' ? [row('gpu')] : home.promise);
    const refresh = refreshProjects();
    await vi.advanceTimersByTimeAsync(2500);
    await refresh;
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['gpu']);
    home.resolve([row('late-home')]);
    await vi.advanceTimersByTimeAsync(1);
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['late-home', 'gpu']);
  });

  it('does not let a delayed snapshot erase a project added since the request began', async () => {
    vi.useFakeTimers();
    stageBackend({ id: 'gpu' });
    const home = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', async () => takePinnedBackend() === 'gpu' ? [row('gpu')] : home.promise);
    const refresh = refreshProjects();
    await vi.advanceTimersByTimeAsync(2500);
    await refresh;
    addProjectLocal(row('just-added').project);
    home.resolve([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['just-added', 'gpu']);
  });

  it('remembers the repository target by computer identity and keeps it while offline', async () => {
    const machine = stageBackend({ id: 'gpu' });
    setBackendIdentityFromBootstrap('gpu-uuid', 'db-generation', 'GPU', 'gpu');
    setBindingMock('ListProjects', async () => [row(takePinnedBackend() === 'gpu' ? 'gpu' : 'mac')]);
    await refreshProjects();
    rememberProjectTarget(row('mac').project, 'gpu');
    machine.setStatus('disconnected');
    expect(preferredProjectTarget(row('mac').project).id).toBe('gpu');
    detachBackend('gpu');
    expect(preferredProjectTarget(row('mac').project).id).toBe('mac');
  });

  it('discards a superseded read without reporting failure or marking an unknown catalog loaded', async () => {
    const old = deferred<ProjectWithCounts[]>();
    const next = deferred<ProjectWithCounts[]>();
    const read = setBindingMock('ListProjects', () => old.promise);
    const toasts = getToasts().length;
    const first = refreshProjects();
    read.mockImplementation(() => next.promise);
    const second = refreshProjects();
    old.resolve([row('obsolete')]);
    await first;
    expect(isLoaded()).toBe(false);
    expect(getProjects()).toEqual([]);
    expect(getToasts().slice(toasts)).toEqual([]);
    next.resolve([row('current')]);
    await second;
    expect(isLoaded()).toBe(true);
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['current']);
  });

  it.each([false, true])('startup follows the winning thread read before validating saved panes (winner finishes first: %s)', async (winnerFirst) => {
    const old = deferred<Thread[]>();
    const next = deferred<Thread[]>();
    const read = setBindingMock('ListThreads', () => old.promise);
    const startup = loadThreads();
    read.mockImplementation(() => next.promise);
    const reconnect = readThreadRows();
    const current = [{ id: 'saved-pane', ownershipEpoch: 1 }] as Thread[];
    if (winnerFirst) { next.resolve(current); await reconnect; }
    old.resolve([]);
    if (!winnerFirst) next.resolve(current);
    expect(await startup).toEqual(current);
    expect(await reconnect).toEqual(current);
  });
});
