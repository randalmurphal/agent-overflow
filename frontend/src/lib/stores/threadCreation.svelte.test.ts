// Tests for resolveDraftTargetProject — the small helper that the
// global Ctrl+N keybinding calls in App.svelte to decide which
// project the next draft thread lands in when no pane has the
// context. Critical because the original code path was "no thread →
// toast and bail", which left Ctrl+N inert from a fresh app launch.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  openDraftThreadForProject,
  resolveDraftTargetProject,
} from './threadCreation.svelte';
import { createThreadPane } from './thread.svelte';
import {
  addProjectLocal,
  resetProjectsForTest,
} from './projects.svelte';
import { getPaneLayoutItems, resetPaneLayoutForTest } from './paneLayout.svelte';
import { getCompactScreen, setCompactLayoutForTest, showCompactList } from './layoutMode.svelte';
import { focusPane, getFocusedPaneId } from './panes.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { ThreadDefaults } from './bindings';
import type { Project, Thread } from '../types/models';

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'project-1',
    path: '/tmp/p1',
    name: 'Project One',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp/p1',
    projectPath: '/tmp/p1',
    projectId: 'project-1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeDefaults(
  overrides: Partial<ThreadDefaults> = {},
): ThreadDefaults {
  return {
    provider: 'codex',
    model: 'gpt-5.4',
    reasoningEffort: 'high',
    fastMode: false,
    contextWindow: 200000,
    runtimeMode: 'full-access',
    branch: 'main',
    workspacePath: '/tmp/p1',
    ...overrides,
  };
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

describe('resolveDraftTargetProject', () => {
  beforeEach(() => {
    resetProjectsForTest();
  });

  it('uses the focused pane thread project when one is present', () => {
    addProjectLocal(makeProject({ id: 'project-2', path: '/tmp/p2', name: 'Project Two' }));
    addProjectLocal(makeProject({ id: 'project-1', path: '/tmp/p1', name: 'Project One' }));
    const pane = createThreadPane({ paneId: 'main' });
    pane.replaceThread(makeThread({ projectId: 'project-2' }));

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toEqual({ projectId: 'project-2' });
  });

  it('falls back to the most recently active project when the pane has no thread', () => {
    // addProjectLocal prepends, so the LAST add is index 0 (most recent).
    addProjectLocal(makeProject({ id: 'older', path: '/tmp/old', name: 'Older' }));
    addProjectLocal(makeProject({ id: 'newer', path: '/tmp/new', name: 'Newer' }));
    const pane = createThreadPane({ paneId: 'main' });

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toEqual({ projectId: 'newer' });
  });

  it('falls back to the most recently active project when target pane is null', () => {
    addProjectLocal(makeProject({ id: 'older', path: '/tmp/old', name: 'Older' }));
    addProjectLocal(makeProject({ id: 'newer', path: '/tmp/new', name: 'Newer' }));

    const resolved = resolveDraftTargetProject(null);

    expect(resolved).toEqual({ projectId: 'newer' });
  });

  it('returns null when no projects exist at all', () => {
    const pane = createThreadPane({ paneId: 'main' });

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toBeNull();
  });

  it('returns null when no projects exist and target pane is also null', () => {
    expect(resolveDraftTargetProject(null)).toBeNull();
  });

});

describe('openDraftThreadForProject', () => {
  beforeEach(() => {
    resetProjectsForTest();
    resetPaneLayoutForTest();
  });

  afterEach(() => setCompactLayoutForTest(false));

  it('reveals a reused draft pane every time New Thread is requested from the compact list', async () => {
    const project = makeProject();
    addProjectLocal(project);
    const pane = createThreadPane({ paneId: 'main' });
    setBindingMock('GetThreadDefaults', async () => makeDefaults());
    setCompactLayoutForTest(true);

    for (let lap = 0; lap < 3; lap++) {
      showCompactList();
      focusPane('other-pane');
      await expect(openDraftThreadForProject({ projectId: project.id, targetPane: pane })).resolves.toBe(pane);

      expect(getCompactScreen()).toBe('thread');
      expect(getFocusedPaneId()).toBe(pane.paneId);
      expect(getPaneLayoutItems().map(item => item.paneId)).toEqual([pane.paneId]);
      expect(pane.hasDraftPlaceholder).toBe(true);
    }
  });

  it('waits for authoritative composer defaults before opening the placeholder', async () => {
    const project = makeProject();
    addProjectLocal(project);
    const pane = createThreadPane({ paneId: 'main' });
    pane.replaceThread(makeThread());
    const pendingDefaults = deferred<ThreadDefaults>();
    setBindingMock('GetThreadDefaults', () => pendingDefaults.promise);

    const opening = openDraftThreadForProject({
      projectId: project.id,
      targetPane: pane,
    });

    expect(pane.thread?.id).toBe('thread-1');

    pendingDefaults.resolve(makeDefaults());
    await expect(opening).resolves.toBe(pane);

    expect(pane.thread).toMatchObject({
      projectId: project.id,
      model: 'gpt-5.4',
      reasoningEffort: 'high',
      runtimeMode: 'full-access',
      branch: 'main',
      isDraft: true,
    });
  });

  it('does not let an older defaults response replace a newer draft request', async () => {
    const firstProject = makeProject();
    const secondProject = makeProject({
      id: 'project-2',
      path: '/tmp/p2',
      name: 'Project Two',
    });
    addProjectLocal(firstProject);
    addProjectLocal(secondProject);
    const pane = createThreadPane({ paneId: 'main' });
    const firstDefaults = deferred<ThreadDefaults>();
    const secondDefaults = deferred<ThreadDefaults>();
    setBindingMock('GetThreadDefaults', (opts: { projectId: string }) => {
      return opts.projectId === firstProject.id
        ? firstDefaults.promise
        : secondDefaults.promise;
    });

    const firstOpening = openDraftThreadForProject({
      projectId: firstProject.id,
      targetPane: pane,
    });
    const secondOpening = openDraftThreadForProject({
      projectId: secondProject.id,
      targetPane: pane,
    });

    secondDefaults.resolve(makeDefaults({
      model: 'gpt-5.4-mini',
      branch: 'feature/newer',
      workspacePath: secondProject.path,
    }));
    await expect(secondOpening).resolves.toBe(pane);
    firstDefaults.resolve(makeDefaults({
      model: 'stale-model',
      branch: 'stale-branch',
    }));
    await expect(firstOpening).resolves.toBeNull();

    expect(pane.thread).toMatchObject({
      projectId: secondProject.id,
      mode: 'chat',
      model: 'gpt-5.4-mini',
      branch: 'feature/newer',
    });
  });

  it('does not replace a thread selected while defaults are loading', async () => {
    const project = makeProject();
    addProjectLocal(project);
    const pane = createThreadPane({ paneId: 'main' });
    const pendingDefaults = deferred<ThreadDefaults>();
    setBindingMock('GetThreadDefaults', () => pendingDefaults.promise);

    const opening = openDraftThreadForProject({
      projectId: project.id,
      targetPane: pane,
    });

    const selectedThread = makeThread({
      id: 'selected-thread',
      title: 'Selected while loading',
    });
    pane.clear();
    pane.replaceThread(selectedThread);
    pendingDefaults.resolve(makeDefaults());
    await opening;

    expect(pane.thread).toEqual(selectedThread);
    expect(pane.hasDraftPlaceholder).toBe(false);
  });

  it('opens a usable placeholder when defaults cannot be loaded', async () => {
    const project = makeProject();
    addProjectLocal(project);
    const pane = createThreadPane({ paneId: 'main' });
    setBindingMock('GetThreadDefaults', async () => {
      throw new Error('defaults unavailable');
    });
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

    try {
      await expect(openDraftThreadForProject({
        projectId: project.id,
        targetPane: pane,
      })).resolves.toBe(pane);
    } finally {
      warn.mockRestore();
    }

    expect(pane.hasDraftPlaceholder).toBe(true);
    expect(pane.thread).toMatchObject({
      projectId: project.id,
      provider: 'codex',
      model: '',
      isDraft: true,
    });
  });
});
