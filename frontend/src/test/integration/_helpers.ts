// Shared helpers for App-level integration tests.
//
// These tests mount the full <App> against mocked Wails bindings. A lot of
// the app's state lives in module-scoped $state (panes, command registry,
// threads, settings, etc.), which persists across vitest tests in the same
// worker. Every test should call `resetAppState()` in beforeEach to return
// to a known-good baseline.

import { tick } from 'svelte';
import { clearCommandRegistry } from '../../lib/stores/commandRegistry.svelte';
import { closePalette } from '../../lib/stores/palette.svelte';
import { resetKeybindingsStore } from '../../lib/stores/keybindings.svelte';
import { getMainPane } from '../../lib/stores/panes.svelte';
import {
  clearThreadSelection,
  setIncludeArchived,
  setThreadFilterQuery,
  setWorkspaceFilter,
} from '../../lib/stores/threadFilter.svelte';
import { setBindingMock } from '../mocks/bindings-app';
import type { Project, ProjectWithCounts, Thread } from '../../lib/types/models';
import type { GitStatus } from '../../lib/types/git';
import { resetProjectsForTest } from '../../lib/stores/projects.svelte';
import { resetSidebarForTest, expandProject } from '../../lib/stores/sidebar.svelte';
import { getAllDrafts, resetForTest as resetDraftThreadsForTest } from '../../lib/stores/draftThreads.svelte';
import { resetRuntimeModeDraftsForTest } from '../../lib/stores/runtimeModeDraft.svelte';
import { getThreads } from '../../lib/stores/threads.svelte';

// Drain microtasks + Svelte reactions so $effects and async mounts settle.
// `n` should be generous for integration tests that depend on $effects
// cascading through App -> Sidebar -> ChatView -> Composer.
export async function flush(n = 6): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

export function resetAppState(): void {
  clearCommandRegistry();
  resetKeybindingsStore();
  closePalette();
  getMainPane().clear();
  setThreadFilterQuery('');
  setIncludeArchived(false);
  setWorkspaceFilter(null);
  clearThreadSelection();
  // Reset the projects-first sidebar state so tests that expect a clean
  // project list / collapsed chevrons don't inherit from a prior case.
  resetProjectsForTest();
  resetSidebarForTest();
  // Module-scoped draft pointers persist across tests in a worker, so
  // clear them here — otherwise test A's draft thread can be reused
  // by test B's "New Thread" click and the backend mock for the
  // second test never fires.
  resetDraftThreadsForTest();
  resetRuntimeModeDraftsForTest();
}

// Every binding that App (or anything App mounts during bootstrap) calls on
// mount. Tests that don't assert on these still need them to resolve so the
// App renders without the "called without a mock" explosion.
export function installAppDefaults(): void {
  setBindingMock('GetSettings', async () => null);
  setBindingMock('ListThreads', async () => []);
  setBindingMock('GetKeybindings', async () => []);
  setBindingMock('GetProviderStatuses', async () => []);
  // Sidebar fetches projects on mount. Default to an empty list — tests
  // that need visible threads should seed a project via seedProject().
  setBindingMock('ListProjects', async () => []);
}

/**
 * Sidebar tests that want to see threads in the sidebar need to declare a
 * project for those threads and expand it. This helper wires both steps in
 * one call; callers pass the project metadata + the threads they intend to
 * list via ListThreads and we handle the plumbing.
 */
export function seedSidebarProject(threads: Thread[]): Project {
  const project: Project = {
    id: 'proj-int',
    path: '/tmp/ws',
    name: 'Integration Project',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
  const pwc: ProjectWithCounts = {
    project,
    threadCount: threads.length,
    lastActive: 0,
  };
  setBindingMock('ListProjects', async () => [pwc]);
  expandProject(project.id);
  return project;
}

// Bindings that start firing the moment a thread becomes active: ChatView
// mounts GitActionsControl (header) and the below-composer BranchPicker,
// which call GetGitStatus / GitListBranches in $effect. Tests that
// switch into a thread need these mocked even if they don't assert on
// git UI.
export function installThreadViewDefaults(): void {
  setBindingMock('SwitchThread', async (threadId: unknown) => {
    const id = typeof threadId === 'string' ? threadId : 'thread-1';
    const listedThread = getThreads().find((thread) => thread.id === id);
    if (listedThread) return listedThread;
    for (const draft of getAllDrafts().values()) {
      if (draft.id === id) return draft;
    }
    return makeThread({ id });
  });
  // ChatView may fire MarkThreadRead while keeping the active thread row
  // read as completed turns settle; default it to a no-op so tests that
  // do not care about read-state do not need their own mock.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  setBindingMock('ListRecentThreadItems', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListItems', async () => []);
  // switchThread rehydrates latestSettledTurn via ListRecentTurns — default
  // to an empty list so tests that don't care about turn history don't
  // need to set the mock themselves.
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  setBindingMock('GetGitStatus', async () => makeGitStatus());
  setBindingMock('GitListBranches', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);
  // Thread-wide aggregate surfaces (PlanSidebar / BackgroundTaskTray) fetch
  // these bindings on mount / thread-switch.
  // Default to empty lists so tests that don't assert on those
  // surfaces don't have to stub each one themselves.
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
}

// Composer-adjacent bindings that fire on mount. NOT included: SendMessage
// and InterruptTurn — callers should set those themselves so per-test mocks
// aren't overwritten by the helper.
export function installComposerDefaults(threadId: string): void {
  setBindingMock('GetDraft', async () => ({
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('SearchWorkspaceFiles', async () => ({
    files: [],
    truncated: false,
    root: '/tmp/ws',
  }));
}

export function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Integration Thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    // All post-Wave-1 threads carry a projectId; default to the seeded
    // integration project so the sidebar renders the row under its group.
    projectId: 'proj-int',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 1_700_000_000_000,
    updatedAt: 1_700_000_000_000,
    archived: false,
    ...overrides,
  };
}

export function makeGitStatus(overrides: Partial<GitStatus> = {}): GitStatus {
  return {
    isRepo: true,
    branch: 'main',
    isDefaultBranch: true,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    ...overrides,
  };
}

/**
 * Element.animate shim. happy-dom doesn't implement it, and Svelte's
 * transitions fall over when they probe for it. Suites that mount dialogs
 * with transition:fade or scale should call this from beforeAll.
 */
export function installAnimateShim(): void {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        let onfinish: (() => void) | null = null;
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {},
          finish() { onfinish?.(); },
          play() {},
          pause() {},
          reverse() {},
          addEventListener(type: string, cb: EventListener) {
            if (type === 'finish') onfinish = cb as unknown as () => void;
          },
          removeEventListener() {},
          get onfinish() { return onfinish; },
          set onfinish(cb: (() => void) | null) {
            onfinish = cb;
            if (cb) queueMicrotask(cb);
          },
        };
      };
  }
}
