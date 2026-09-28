import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setupEventListeners } from './events';
import { applyTransportGap } from './eventsTransportGap';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import type { RecoveryPhase } from './transportRecovery';
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

// The recovery listeners events.ts registers, so a test can end a replay.
const recovery = vi.hoisted(() => new Set<(backend: string, phase: RecoveryPhase) => void>());
vi.mock('./transportRecovery', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./transportRecovery')>()),
  onBackendRecovery: (fn: (backend: string, phase: RecoveryPhase) => void) => {
    recovery.add(fn);
    return () => recovery.delete(fn);
  },
}));
vi.mock('../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

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

// A removal frame can be lost (a transport gap) or never sent (the computer
// restarted and its first registry read seeds without announcing). The
// worktree list then decides which parked drafts sit on a directory that is
// gone.
describe('draft worktree revalidation', () => {
  function listing(items: Array<{ path: string; branch: string; missing?: boolean }>) {
    const calls: string[] = [];
    setBindingMock('GitListWorktrees', async (ws: { projectId: string }) => {
      calls.push(ws.projectId);
      return items.map((item) => ({ head: 'abc', deleteBlocked: false, missing: false, ...item }));
    });
    return calls;
  }

  function parkedDrafts() {
    const repo = project('project-1', '/repo');
    return {
      unlisted: draftPane('pane-unlisted', repo, '/repo-wt/gone', 'gone'),
      missing: draftPane('pane-missing', repo, '/repo-wt/deleted', 'deleted'),
      live: draftPane('pane-live', repo, '/repo-wt/live', 'live'),
      atRoot: draftPane('pane-root', repo, '/repo', 'develop'),
    };
  }

  const rootAndLive = [
    { path: '/repo', branch: 'develop' },
    { path: '/repo-wt/live', branch: 'live' },
    { path: '/repo-wt/deleted', branch: 'deleted', missing: true },
  ];

  function expectGoneDraftsMoved(drafts: ReturnType<typeof parkedDrafts>) {
    for (const pane of [drafts.unlisted, drafts.missing]) {
      expect(pane.thread?.workspacePath).toBe('/repo');
      expect(pane.thread?.worktreePath).toBe('');
      expect(pane.thread?.branch).toBe('develop');
    }
    expect(drafts.live.thread?.workspacePath).toBe('/repo-wt/live');
    expect(drafts.atRoot.thread?.workspacePath).toBe('/repo');
  }

  it('re-reads the list on a gap in the channel and moves drafts off gone worktrees', async () => {
    const drafts = parkedDrafts();
    setThreadEnvMode(drafts.unlisted.thread!, 'new-worktree');
    const calls = listing(rootAndLive);

    applyTransportGap({ channel: 'worktree:removed', seq: 4 });

    await vi.waitFor(() => expect(drafts.unlisted.thread?.workspacePath).toBe('/repo'));
    expectGoneDraftsMoved(drafts);
    expect(hasStagedWorktreeIntent(drafts.unlisted.thread)).toBe(false);
    expect(calls).toEqual(['project-1']);
  });

  it('re-reads the list when a reconnect finishes its replay', async () => {
    const drafts = parkedDrafts();
    listing(rootAndLive);

    for (const listener of recovery) listener('', 'complete');

    await vi.waitFor(() => expect(drafts.missing.thread?.workspacePath).toBe('/repo'));
    expectGoneDraftsMoved(drafts);
  });

  it('reads nothing when no draft sits in a linked worktree', async () => {
    draftPane('pane-root', project('project-1', '/repo'), '/repo', 'main');
    const calls = listing(rootAndLive);

    for (const listener of recovery) listener('', 'complete');
    await Promise.resolve();

    expect(calls).toEqual([]);
  });

  it('moves nothing and reports when the list cannot be read', async () => {
    const drafts = parkedDrafts();
    vi.mocked(reportFrontendDiagnostic).mockClear();
    setBindingMock('GitListWorktrees', async () => { throw new Error('git failed'); });

    for (const listener of recovery) listener('', 'complete');

    await vi.waitFor(() => expect(reportFrontendDiagnostic).toHaveBeenCalledOnce());
    expect(drafts.unlisted.thread?.workspacePath).toBe('/repo-wt/gone');
    expect(drafts.missing.thread?.workspacePath).toBe('/repo-wt/deleted');
  });
});
