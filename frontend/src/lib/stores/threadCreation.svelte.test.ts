// Tests for resolveDraftTargetProject — the small helper that the
// global Ctrl+N keybinding calls in App.svelte to decide which
// project the next draft thread lands in when no pane has the
// context. Critical because the original code path was "no thread →
// toast and bail", which left Ctrl+N inert from a fresh app launch.

import { beforeEach, describe, expect, it } from 'vitest';
import { resolveDraftTargetProject } from './threadCreation.svelte';
import { createThreadPane } from './thread.svelte';
import {
  addProjectLocal,
  resetProjectsForTest,
} from './projects.svelte';
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

describe('resolveDraftTargetProject', () => {
  beforeEach(() => {
    resetProjectsForTest();
  });

  it('uses the focused pane thread project when one is present', () => {
    addProjectLocal(makeProject({ id: 'project-2', path: '/tmp/p2', name: 'Project Two' }));
    addProjectLocal(makeProject({ id: 'project-1', path: '/tmp/p1', name: 'Project One' }));
    const pane = createThreadPane({ paneId: 'main' });
    pane.replaceThread(makeThread({ projectId: 'project-2', mode: 'design' }));
    pane.setActiveTab('design');

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toEqual({ projectId: 'project-2', mode: 'design' });
  });

  it('falls back to the most recently active project when the pane has no thread', () => {
    // addProjectLocal prepends, so the LAST add is index 0 (most recent).
    addProjectLocal(makeProject({ id: 'older', path: '/tmp/old', name: 'Older' }));
    addProjectLocal(makeProject({ id: 'newer', path: '/tmp/new', name: 'Newer' }));
    const pane = createThreadPane({ paneId: 'main' });

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toEqual({ projectId: 'newer', mode: 'chat' });
  });

  it('falls back to the most recently active project when target pane is null', () => {
    addProjectLocal(makeProject({ id: 'older', path: '/tmp/old', name: 'Older' }));
    addProjectLocal(makeProject({ id: 'newer', path: '/tmp/new', name: 'Newer' }));

    const resolved = resolveDraftTargetProject(null);

    expect(resolved).toEqual({ projectId: 'newer', mode: 'chat' });
  });

  it('returns null when no projects exist at all', () => {
    const pane = createThreadPane({ paneId: 'main' });

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toBeNull();
  });

  it('returns null when no projects exist and target pane is also null', () => {
    expect(resolveDraftTargetProject(null)).toBeNull();
  });

  it('preserves the pane activeTab when falling back to the recent project', () => {
    addProjectLocal(makeProject({ id: 'newer', path: '/tmp/new', name: 'Newer' }));
    const pane = createThreadPane({ paneId: 'main' });
    pane.setActiveTab('design');

    const resolved = resolveDraftTargetProject(pane);

    expect(resolved).toEqual({ projectId: 'newer', mode: 'design' });
  });
});
