// What leaves with a backend, end to end: the index forgets, the row
// stores drop, the panes close. One file rather than three, because the
// point is that a single detach reaches all of them; a per-store test
// would pass while the wiring between them was never installed.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { deferred } from '../../test/helpers/providerAccounts';

import {
  __attachBackendForTest,
  __resetBackendsForTest,
  detachBackend,
  type BackendDescriptor,
} from '../transport/backends';
import {
  __resetEntityIndexForTest,
  noteProject,
  noteThread,
  noteThreadGroup,
  threadBackend,
} from '../transport/entityIndex';
import { getThreads, prependThread, removeThread } from './threads.svelte';
import { catalogRevision } from './computerCatalogRevision';
import { setupEventListeners } from './events';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { addProjectLocal, getProjects, resetProjectsForTest } from './projects.svelte';
import {
  getThreadGroups,
  resetThreadGroupsForTest,
  upsertThreadGroup,
} from './threadGroups.svelte';
import {
  createPane,
  focusPane,
  getAllPanes,
  getFocusedThreadPaneId,
  resetPanesForTest,
  revealPane,
} from './panes.svelte';
import { getCompactScreen, setCompactLayoutForTest } from './layoutMode.svelte';
import {
  __resetSelectedBackendForTest,
  selectedBackend,
  setPaneBackend,
} from './selectedBackend.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { getToasts } from './toast.svelte';
import {
  getExistingThreadTerminalState,
  getThreadTerminalState,
} from '../components/terminal/terminalStore.svelte';
import type { Project, Thread, ThreadGroup } from '../types/models';

const LAPTOP = 'laptop';

function descriptor(overrides: Partial<BackendDescriptor> = {}): BackendDescriptor {
  return {
    id: LAPTOP,
    backendId: '99999999-8888-4777-8666-555555555555',
    name: 'Laptop',
    wsUrl: 'ws://localhost:3000/ws/backend/laptop',
    bootstrapUrl: '/bootstrap/laptop.json',
    ...overrides,
  };
}

function fakeClient(): unknown {
  return {
    callByID: async () => null,
    callByName: async () => null,
    subscribe: () => () => undefined,
    installStepUpProver: () => undefined,
    setLease: () => undefined,
    setWatchedThreads: () => undefined,
    getStatus: () => ({ status: 'connected', nextAttemptAt: null }),
    onStatusChange: () => () => undefined,
    getHello: () => null,
    onHelloChange: () => () => undefined,
    onReplay: () => () => undefined,
    close: () => undefined,
  };
}

function attachLaptop(): void {
  __attachBackendForTest(descriptor(), fakeClient() as never);
}

function makeThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: id,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

/** Everything `pane.switchThread` reaches for, so a pane can hold a thread
 * without a backend on the other end. */
function mockThreadSwitch(thread: Thread): void {
  setBindingMock('SwitchThread', async () => thread);
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('GetThreadLiveState', async () => null);
  setBindingMock('ListPendingInteractiveRequests', async () => null);
  setBindingMock('AutoResumeThread', async () => {});
}

function makeProject(id: string, path: string): Project {
  return {
    id,
    path,
    name: id,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function makeGroup(id: string, projectId: string): ThreadGroup {
  return { id, projectId, name: id, createdAt: 0, updatedAt: 0 };
}

beforeEach(() => {
  resetBindingMocks();
  __resetBackendsForTest();
  __resetEntityIndexForTest();
  __resetSelectedBackendForTest();
  resetPanesForTest();
  resetThreadGroupsForTest();
  for (const t of [...getThreads()]) removeThread(t.id);
  resetProjectsForTest();
});

describe('a backend detaching', () => {
  it('invalidates the former computer’s catalog when ownership changes', () => {
    attachLaptop();
    noteThread('moved', LAPTOP, 0);
    prependThread(makeThread('moved'));
    const revision = catalogRevision(LAPTOP, 'threads');
    noteThread('moved', '', 1);
    expect(catalogRevision(LAPTOP, 'threads')).toBeGreaterThan(revision);
  });

  it('invalidates the remote catalog before forgetting a deleted thread’s owner', () => {
    attachLaptop();
    noteThread('deleted', LAPTOP, 0);
    prependThread(makeThread('deleted'));
    const revision = catalogRevision(LAPTOP, 'threads');
    const stop = setupEventListeners();
    try {
      emitWailsEvent('thread:updated', { action: 'deleted', id: 'deleted' }, descriptor().backendId);
      expect(getThreads()).toEqual([]);
      expect(catalogRevision(LAPTOP, 'threads')).toBeGreaterThan(revision);
      expect(threadBackend('deleted')).toBeUndefined();
    } finally { stop(); }
  });

  it('rebinds a mounted moved thread and ignores a former owner’s delayed row', async () => {
    attachLaptop();
    const original = makeThread('moving', { ownershipEpoch: 0 });
    noteThread(original.id, '', 0);
    prependThread(original);
    const pane = createPane('moving-pane');
    mockThreadSwitch(original);
    await pane.switchThread(original);
    const first = deferred<Thread>();
    const second = deferred<Thread>();
    let reads = 0;
    setBindingMock('GetThread', () => ++reads === 1 ? first.promise : second.promise);
    const before = pane.switchGeneration;
    noteThread(original.id, LAPTOP, 1);
    expect(pane.switchGeneration).toBeGreaterThan(before);
    noteThread(original.id, '', 2);
    const returned = makeThread(original.id, { ownershipEpoch: 2, workspacePath: '/home/returned' });
    second.resolve(returned);
    await vi.waitFor(() => expect(pane.thread?.workspacePath).toBe('/home/returned'));
    first.resolve(makeThread(original.id, { ownershipEpoch: 1, workspacePath: '/remote/old' }));
    await Promise.resolve();
    await Promise.resolve();
    expect(pane.thread?.ownershipEpoch).toBe(2);
    expect(pane.thread?.workspacePath).toBe('/home/returned');
    expect(getAllPanes().get('moving-pane')).toBe(pane);
  });

  it('drops its threads, projects and groups from the row stores', () => {
    attachLaptop();
    prependThread(makeThread('t-laptop'));
    prependThread(makeThread('t-home'));
    addProjectLocal(makeProject('p-laptop', '/laptop'));
    addProjectLocal(makeProject('p-home', '/home'));
    upsertThreadGroup(makeGroup('g-laptop', 'p-laptop'));
    upsertThreadGroup(makeGroup('g-home', 'p-home'));
    noteThread('t-laptop', LAPTOP);
    noteThread('t-home', '');
    noteProject('p-laptop', LAPTOP);
    noteProject('p-home', '');
    noteThreadGroup('g-laptop', LAPTOP);
    noteThreadGroup('g-home', '');

    detachBackend(LAPTOP);

    expect(getThreads().map((t) => t.id)).toEqual(['t-home']);
    expect(getThreadGroups().map((g) => g.id)).toEqual(['g-home']);
    expect(getProjects().map((p) => p.project.id)).toEqual(['p-home']);
    expect(threadBackend('t-laptop')).toBeUndefined();
  });

  // A pane still showing a departed machine's thread is the worst of the
  // two failures: the composer re-enables (nothing is unreachable once the
  // registry entry is gone) and the next send resolves to the page's own
  // backend.
  it('closes every pane showing one of its threads, and leaves the rest', async () => {
    attachLaptop();
    const laptopThread = makeThread('t-laptop');
    const homeThread = makeThread('t-home');
    prependThread(laptopThread);
    prependThread(homeThread);
    noteThread('t-laptop', LAPTOP);
    noteThread('t-home', '');
    const left = createPane('left');
    const right = createPane('right');
    mockThreadSwitch(laptopThread);
    await left.switchThread(laptopThread);
    mockThreadSwitch(homeThread);
    await right.switchThread(homeThread);

    detachBackend(LAPTOP);

    expect([...getAllPanes().keys()]).toEqual(['right']);
  });

  // Compact has no pane close control, so the only way the thread screen
  // empties is something else taking the pane. An empty thread screen has
  // no back button; the list is the only place left to be.
  it('returns a compact client to the list when it took the only pane', async () => {
    setCompactLayoutForTest(true);
    try {
      attachLaptop();
      const laptopThread = makeThread('t-laptop');
      prependThread(laptopThread);
      noteThread('t-laptop', LAPTOP);
      const pane = createPane('only');
      mockThreadSwitch(laptopThread);
      await pane.switchThread(laptopThread);
      revealPane('only');
      expect(getCompactScreen()).toBe('thread');

      detachBackend(LAPTOP);

      expect(getAllPanes().size).toBe(0);
      expect(getCompactScreen()).toBe('list');
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  // A placeholder has no thread id, so the detached thread list cannot name
  // it. It routes through its project, and that project just left: kept
  // open, it would refuse to send or create the thread on another computer.
  it('closes every draft placeholder on one of its projects, and leaves the rest', () => {
    attachLaptop();
    const laptopProject = makeProject('p-laptop', '/laptop');
    const homeProject = makeProject('p-home', '/home');
    noteProject(laptopProject.id, LAPTOP);
    noteProject(homeProject.id, '');
    createPane('laptop-chat').startDraftPlaceholder(laptopProject, 'chat');
    createPane('laptop-plan').startDraftPlaceholder(laptopProject, 'plan');
    createPane('home-chat').startDraftPlaceholder(homeProject, 'chat');
    createPane('empty');

    detachBackend(LAPTOP);

    expect([...getAllPanes().keys()]).toEqual(['home-chat', 'empty']);
    expect(getAllPanes().get('home-chat')?.draftPlaceholder?.projectId).toBe(homeProject.id);
  });

  // The computer that ran the placeholder's shells is no longer reachable,
  // and no other one owns them: routed by the forgotten project, the close
  // would reach the wrong computer or fail with an error.
  it("drops a placeholder's terminals without asking any computer to close them", () => {
    attachLaptop();
    const project = makeProject('p-laptop', '/laptop');
    noteProject(project.id, LAPTOP);
    const pane = createPane('laptop-chat');
    pane.startDraftPlaceholder(project, 'chat');
    const placeholderId = pane.thread!.id;
    getThreadTerminalState(placeholderId).addTab({
      terminalID: 'term-1', threadID: placeholderId, shell: '/bin/sh', cwd: project.path,
      rows: 24, cols: 80, pid: 123, startedAt: 1, running: true, exitCode: 0, exitReason: '',
    });
    pane.setShowTerminal(true);
    const close = setBindingMock('CloseThreadTerminals', async () => {
      throw new Error('The computer that owns this item is unknown. Refresh it before continuing.');
    });
    const toastsBefore = getToasts().length;

    detachBackend(LAPTOP);

    expect(getAllPanes().size).toBe(0);
    expect(getExistingThreadTerminalState(placeholderId)).toBeNull();
    expect(close).not.toHaveBeenCalled();
    expect(getToasts().slice(toastsBefore)).toEqual([]);
  });

  it('closes a placeholder on each detach of a computer that attached again', () => {
    const project = makeProject('p-laptop', '/laptop');
    for (const paneId of ['first', 'second']) {
      attachLaptop();
      noteProject(project.id, LAPTOP);
      createPane(paneId).startDraftPlaceholder(project, 'chat');

      detachBackend(LAPTOP);

      expect(getAllPanes().size).toBe(0);
    }
  });

  it('returns a compact client to the list when a placeholder was the only pane', () => {
    setCompactLayoutForTest(true);
    try {
      attachLaptop();
      const project = makeProject('p-laptop', '/laptop');
      noteProject(project.id, LAPTOP);
      createPane('only').startDraftPlaceholder(project, 'chat');
      revealPane('only');
      expect(getCompactScreen()).toBe('thread');

      detachBackend(LAPTOP);

      expect(getAllPanes().size).toBe(0);
      expect(getCompactScreen()).toBe('list');
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  it('leaves the stores untouched when it owned nothing', () => {
    attachLaptop();
    prependThread(makeThread('t-home'));
    noteThread('t-home', '');

    detachBackend(LAPTOP);

    expect(getThreads().map((t) => t.id)).toEqual(['t-home']);
  });
});

// The `selected` route's pane override. It was armed by a setter nobody
// called on focus change, so every per-pane machine choice was dead; a
// resolver cannot be forgotten.
describe('the selected route reads the focused pane live', () => {
  it('follows focus from one pane to another without any focus-time write', () => {
    attachLaptop();
    createPane('left');
    createPane('right');
    setPaneBackend('left', LAPTOP);

    focusPane('left');
    expect(getFocusedThreadPaneId()).toBe('left');
    expect(selectedBackend()).toBe(LAPTOP);

    focusPane('right');
    expect(selectedBackend()).toBe('');
  });

  it('forgets a closed pane override rather than answering from a dead pane', () => {
    attachLaptop();
    createPane('left');
    setPaneBackend('left', LAPTOP);
    focusPane('left');
    expect(selectedBackend()).toBe(LAPTOP);

    resetPanesForTest();
    expect(selectedBackend()).toBe('');
  });

  it('keeps the absent target so dispatch refuses rather than using home', () => {
    attachLaptop();
    createPane('left');
    setPaneBackend('left', LAPTOP);
    focusPane('left');
    expect(selectedBackend()).toBe(LAPTOP);

    detachBackend(LAPTOP);
    expect(selectedBackend()).toBe(LAPTOP);
  });
});
