import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { setupEventListeners } from './events';
import { refreshThreads } from './threads.svelte';
import { createThreadPane } from './thread.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import {
  hasStagedWorktreeIntent,
  resetForTest as resetWorktreeIntent,
  setThreadEnvMode,
} from './worktreeIntent.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { makeThread } from '../../test/helpers/chat';
import type { Project } from '../types/models';

// worktree:removed is the one signal every worktree removal sends, whoever
// made it. Thread rows ride thread:updated; the draft composers parked in the
// removed directory have no row, so this is what moves them to the root.

function project(id: string, path: string): Project {
  return { id, path, name: id, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false };
}

function draftPane(paneId: string, owner: Project, workspacePath: string, branch: string) {
  const pane = createThreadPane({ paneId });
  pane.startDraftPlaceholder(owner, 'chat', { provider: 'claude', model: 'm', workspacePath, branch });
  registerPaneForTest(paneId, pane);
  return pane;
}

let cleanupEvents: (() => void) | null = null;

beforeEach(async () => {
  resetPanesForTest();
  resetBindingMocks();
  resetWorktreeIntent();
  setBindingMock('ListThreads', async () => []);
  await refreshThreads();
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanupEvents?.();
  cleanupEvents = null;
});

describe('worktree:removed', () => {
  it('moves the drafts parked in the removed worktree to the root and drops staged worktree choices', () => {
    const repo = project('project-1', '/repo');
    const inRemoved = draftPane('pane-a', repo, '/repo-wt/feature', 'feature');
    const alsoInRemoved = draftPane('pane-b', repo, '/repo-wt/feature', 'feature');
    const elsewhere = draftPane('pane-c', repo, '/repo-wt/other', 'other');
    const otherProject = draftPane('pane-d', project('project-2', '/other'), '/repo-wt/feature', 'feature');
    setThreadEnvMode(inRemoved.thread!, 'new-worktree');
    setThreadEnvMode(elsewhere.thread!, 'new-worktree');
    const movedThread = makeThread({ id: 'thread-moved' });
    const untouchedThread = makeThread({ id: 'thread-untouched' });
    setThreadEnvMode(movedThread, 'new-worktree');
    setThreadEnvMode(untouchedThread, 'new-worktree');

    emitWailsEvent('worktree:removed', {
      projectId: 'project-1',
      path: '/repo-wt/feature',
      branch: 'main',
      threadIds: ['thread-moved'],
    });

    for (const pane of [inRemoved, alsoInRemoved]) {
      expect(pane.thread?.workspacePath).toBe('/repo');
      expect(pane.thread?.worktreePath).toBe('');
      expect(pane.thread?.branch).toBe('main');
    }
    expect(hasStagedWorktreeIntent(inRemoved.thread)).toBe(false);
    expect(hasStagedWorktreeIntent(movedThread)).toBe(false);

    expect(elsewhere.thread?.workspacePath).toBe('/repo-wt/other');
    expect(hasStagedWorktreeIntent(elsewhere.thread)).toBe(true);
    expect(otherProject.thread?.workspacePath).toBe('/repo-wt/feature');
    expect(hasStagedWorktreeIntent(untouchedThread)).toBe(true);
  });

  it('is idempotent: a second frame for the same removal changes nothing', () => {
    const repo = project('project-1', '/repo');
    const pane = draftPane('pane-a', repo, '/repo-wt/feature', 'feature');
    const frame = { projectId: 'project-1', path: '/repo-wt/feature', branch: 'main', threadIds: [] };

    emitWailsEvent('worktree:removed', frame);
    const after = { ...pane.thread };
    emitWailsEvent('worktree:removed', frame);

    expect(pane.thread?.workspacePath).toBe(after.workspacePath);
    expect(pane.thread?.branch).toBe('main');
  });
});
