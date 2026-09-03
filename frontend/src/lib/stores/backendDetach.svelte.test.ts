// What leaves with a backend, end to end: the index forgets, the row
// stores drop, the panes close. One file rather than three, because the
// point is that a single detach reaches all of them; a per-store test
// would pass while the wiring between them was never installed.

import { beforeEach, describe, expect, it } from 'vitest';

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

  it('answers home once the staged backend has detached', () => {
    attachLaptop();
    createPane('left');
    setPaneBackend('left', LAPTOP);
    focusPane('left');
    expect(selectedBackend()).toBe(LAPTOP);

    detachBackend(LAPTOP);
    expect(selectedBackend()).toBe('');
  });
});
