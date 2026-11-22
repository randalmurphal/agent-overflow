import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { deferred } from '../../test/helpers/providerAccounts';
import { takePinnedBackend, detachBackend } from '../transport/backends';
import { __resetEntityIndexForTest, currentThreadRow, noteThread, projectBackend, resolveThreadBackend } from '../transport/entityIndex';
import { addProjectLocal, getProjects, isLoaded, resetProjectsForTest } from './projects.svelte';
import { getToasts } from './toast.svelte';
import { loadSidebarCatalogs } from './sidebarCatalogs';
import { readSidebarCatalogs } from '../../test/helpers/sidebarCatalogs';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { writeFrontendValue } from './frontendStorage';
import { preferredProjectTarget, rememberProjectTarget } from './projectTargets';
import { readComputerRows, retainUnavailableComputerRows, type ComputerRows } from './computerRows';
import type { ProjectWithCounts, Thread } from '../types/models';

function row(id: string): ProjectWithCounts {
  return { project: { id, name: id, path: '/same/path', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false,
    repositoryID: 'github:github.com:123' }, threadCount: 0, lastActive: 0 };
}
beforeEach(() => { resetStagedBackends(); resetProjectsForTest(); __resetEntityIndexForTest(); });
afterEach(() => { resetStagedBackends(); vi.useRealTimers(); });

describe('unavailable computer catalogs', () => {
  it.each([false, true])('admits only the newest owner of a moved thread (home owns it: %s)', async (homeOwns) => {
    stageBackend({ id: 'gpu' });
    const commits: ComputerRows<Thread>[] = [];
    expect(await readComputerRows(async () => {
      const home = takePinnedBackend() !== 'gpu';
      return [{ id: 'moved', ownershipEpoch: home === homeOwns ? 2 : 1 }] as Thread[];
    }, (row, backend) => { noteThread(row.id, backend, row.ownershipEpoch); }, (result) => { commits.push(result); },
    { admit: currentThreadRow })).toBe(true);
    expect(commits.map((result) => result.rows)).toEqual([[{ id: 'moved', ownershipEpoch: 2 }]]);
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
    await readSidebarCatalogs();
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['mac', 'gpu']);
    expect(projectBackend('gpu')).toBe('gpu');
    offline = true;
    local = [];
    await readSidebarCatalogs();
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['gpu']);
    offline = false;
    local = [row('new-mac')];
    await readSidebarCatalogs();
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['new-mac', 'gpu']);
  });

  it('drops late answers from a computer removed during the read', async () => {
    stageBackend({ id: 'gpu' });
    const remote = deferred<string[]>();
    const commits: ComputerRows<string>[] = [];
    const read = readComputerRows(async () => takePinnedBackend() === 'gpu' ? remote.promise : ['mac'], () => {},
      (result) => { commits.push(result); });
    detachBackend('gpu');
    remote.resolve(['removed']);
    await read;
    expect(commits.map((result) => result.rows)).toEqual([['mac']]);
    expect(retainUnavailableComputerRows(['old-gpu'], commits[0], () => 'gpu')).toEqual(['mac']);
  });

  it('does not hold a healthy computer behind the first host’s cold-start dial', async () => {
    vi.useFakeTimers();
    stageBackend({ id: 'gpu' });
    const home = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', async () => takePinnedBackend() === 'gpu' ? [row('gpu')] : home.promise);
    const refresh = readSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await refresh;
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['gpu']);
    home.resolve([row('late-home')]);
    await vi.advanceTimersByTimeAsync(1);
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['late-home', 'gpu']);
  });

  it('waits for a computer’s first dial but not for one whose reconnect ladder is running', async () => {
    vi.useFakeTimers();
    const gpu = stageBackend({ id: 'gpu', status: 'disconnected' });
    const read = setBindingMock('ListProjects', async () => takePinnedBackend() === 'gpu' ? new Promise<never>(() => {}) : [row('mac')]);
    // Never dialed: the read goes out and the deadline bounds it.
    let refresh = readSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await refresh;
    expect(read).toHaveBeenCalledTimes(2);
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['mac']);
    // First attempt failed, ladder running: offline like any other
    // disconnected computer, so no read is issued and nothing waits.
    gpu.setStatus('reconnecting');
    refresh = readSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(0);
    await refresh;
    expect(read).toHaveBeenCalledTimes(3);
    expect(getProjects().map((entry) => entry.project.id)).toEqual(['mac']);
  });

  it('does not let a delayed snapshot erase a project added since the request began', async () => {
    vi.useFakeTimers();
    stageBackend({ id: 'gpu' });
    const home = deferred<ProjectWithCounts[]>();
    setBindingMock('ListProjects', async () => takePinnedBackend() === 'gpu' ? [row('gpu')] : home.promise);
    const refresh = readSidebarCatalogs();
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
    await readSidebarCatalogs();
    rememberProjectTarget(row('mac').project, 'gpu');
    machine.setStatus('disconnected');
    expect(preferredProjectTarget(row('mac').project).id).toBe('gpu');
    detachBackend('gpu');
    expect(preferredProjectTarget(row('mac').project).id).toBe('mac');
  });

  it('keeps a project-key preference when verification joins previously separate projects', async () => {
    stageBackend({ id: 'gpu' });
    setBackendIdentityFromBootstrap('gpu-uuid', 'db-generation', 'GPU', 'gpu');
    // Explicitly seed the former per-project choice; no URL/ancestry key is adopted.
    writeFrontendValue('project-targets', [['project:mac', 'gpu-uuid']]);
    setBindingMock('ListProjects', async () => [row(takePinnedBackend() === 'gpu' ? 'gpu' : 'mac')]);
    await readSidebarCatalogs();
    expect(preferredProjectTarget(row('mac').project).id).toBe('gpu');
  });

  it('discards retired URL and root preferences instead of matching on them', async () => {
    stageBackend({ id: 'gpu' });
    setBackendIdentityFromBootstrap('gpu-uuid', 'db-generation', 'GPU', 'gpu');
    setBindingMock('ListProjects', async () => {
      const id = takePinnedBackend() === 'gpu' ? 'gpu' : 'mac';
      return [{ ...row(id), project: { ...row(id).project, repositoryID: `github:github.com:${id}` } }];
    });
    await readSidebarCatalogs();
    writeFrontendValue('project-targets', [['commit:shared', 'gpu-uuid'], ['remote:github.com/me/app', 'gpu-uuid']]);
    expect(preferredProjectTarget(row('mac').project).id).toBe('mac');
  });

  it('discards a superseded read without reporting failure or marking an unknown catalog loaded', async () => {
    const old = deferred<ProjectWithCounts[]>();
    const next = deferred<ProjectWithCounts[]>();
    const read = setBindingMock('ListProjects', () => old.promise);
    const toasts = getToasts().length;
    const first = readSidebarCatalogs();
    read.mockImplementation(() => next.promise);
    const second = readSidebarCatalogs();
    old.resolve([row('obsolete')]);
    await first;
    expect(isLoaded()).toBe(false);
    expect(getProjects()).toEqual([]);
    expect(getToasts().slice(toasts)).toEqual([]);
    next.resolve([row('current')]);
    // The round resolves on its thread window, so the project answer's
    // commit is awaited for itself.
    await second;
    await vi.waitFor(() => expect(getProjects().map((entry) => entry.project.id)).toEqual(['current']));
    expect(isLoaded()).toBe(true);
  });

  it.each([false, true])('startup follows the winning thread read before validating saved panes (winner finishes first: %s)', async (winnerFirst) => {
    const old = deferred<Thread[]>();
    const next = deferred<Thread[]>();
    const read = setBindingMock('ListThreads', () => old.promise);
    const startup = loadSidebarCatalogs();
    read.mockImplementation(() => next.promise);
    const reconnect = loadSidebarCatalogs();
    const current = [{ id: 'saved-pane', ownershipEpoch: 1 }] as Thread[];
    if (winnerFirst) { next.resolve(current); await reconnect; }
    old.resolve([]);
    if (!winnerFirst) next.resolve(current);
    expect((await startup).map((thread) => thread.id)).toEqual(['saved-pane']);
    expect((await reconnect).map((thread) => thread.id)).toEqual(['saved-pane']);
  });
});
