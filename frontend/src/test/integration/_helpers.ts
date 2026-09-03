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
import { resetPanesForTest } from '../../lib/stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../lib/stores/paneLayout.svelte';
import { resetPaneLayoutPersistenceForTest } from '../../lib/stores/paneLayoutPersistence';
import {
  clearThreadSelection,
  setThreadFilterQuery,
  setWorkspaceFilter,
} from '../../lib/stores/threadFilter.svelte';
import { setBindingMock } from '../mocks/bindings-app';
import type { Project, ProjectWithCounts, Thread } from '../../lib/types/models';
import type { GitStatus } from '../../lib/types/git';
import { resetProjectsForTest } from '../../lib/stores/projects.svelte';
import { resetAppStorageForTest } from '../../lib/stores/appStorage';
import { resetSidebarForTest, expandProject } from '../../lib/stores/sidebar.svelte';
import { resetProviderModelsForTest } from '../../lib/stores/providerModels.svelte';
import { resetSettingsForTest } from '../../lib/stores/settings.svelte';
import { resetThreadActionConfirmationsForTest } from '../../lib/stores/threadActionConfirmations.svelte';
import { resetSettingsOverlayForTest } from '../../lib/stores/settingsOverlay.svelte';
import { resetWorkflowsOverlayForTest } from '../../lib/stores/workflowsOverlay.svelte';
import { closeAccountSwitcher } from '../../lib/stores/accountSwitcher.svelte';
import {
  getQueueForThread,
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
} from '../../lib/stores/sendQueue.svelte';
import { getThreads } from '../../lib/stores/threads.svelte';
import {
  resetThreadTerminalStatesForTest,
  resetTerminalFocusForTest,
} from '../../lib/components/terminal/terminalStore.svelte';
import { clearThreadScrollSnapshotsForTest } from '../../lib/utils/threadScrollSnapshots';
import { makeSettings } from '../helpers/settings';
import { idleWorkspaceActivity } from '../helpers/workspaceLock';

/**
 * The one workspace every integration fixture lives in: the project root,
 * the thread's workspace path, and the canonical cwd the git-status
 * subscription reports. Those three agreeing is what lets a pushed
 * `git:status` event route to the thread's git-status entry.
 */
export const INTEGRATION_WORKSPACE = '/tmp/ws';

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
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetPaneLayoutPersistenceForTest();
  setThreadFilterQuery('');
  setWorkspaceFilter(null);
  clearThreadSelection();
  // Reset the projects-first sidebar state so tests that expect a clean
  // project list / collapsed chevrons don't inherit from a prior case.
  resetProjectsForTest();
  resetAppStorageForTest();
  resetSidebarForTest();
  resetSettingsForTest();
  resetThreadActionConfirmationsForTest();
  // Both full-height overlays are module state now, so an open one would
  // otherwise ride into the next test (and each closes the other on open).
  resetSettingsOverlayForTest();
  resetWorkflowsOverlayForTest();
  closeAccountSwitcher();
  resetProviderModelsForTest();
  // Per-thread send queue is in-memory only; clear it between tests
  // so a stale queued item from a prior case doesn't drain into the
  // next test's first SendMessage call.
  resetSendQueueForTest();
  resetThreadTerminalStatesForTest();
  // The terminal-focus registry is a module-scoped counter shared across
  // every pane; clearing the tab map above does not zero it, so a focused
  // terminal in one test would otherwise leave `terminalFocus` stuck true
  // for the next.
  resetTerminalFocusForTest();
  clearThreadScrollSnapshotsForTest();
}

// Every binding that App (or anything App mounts during bootstrap) calls on
// mount. Tests that don't assert on these still need them to resolve so the
// App renders without the "called without a mock" explosion.
export function installAppDefaults(): void {
  setBindingMock('GetSettings', async () => null);
  setBindingMock('UpdateSettings', async (patch: unknown) => makeSettings(patch as Parameters<typeof makeSettings>[0]));
  setBindingMock('Version', async () => '0.0.1');
  setBindingMock('ListThreads', async () => []);
  // Sidebar boot loads thread groups beside the threads they contain.
  setBindingMock('ListThreadGroups', async () => []);
  setBindingMock('GetKeybindings', async () => ({ bindings: [] }));
  setBindingMock('GetProviderStatuses', async () => []);
  setBindingMock('GetModelsForProvider', async () => []);
  setBindingMock('GetRateLimitsSnapshots', async () => []);
  setBindingMock('ListProviderAccounts', async () => []);
  // Sidebar fetches projects on mount. Default to an empty list — tests
  // that need visible threads should seed a project via seedProject().
  setBindingMock('ListProjects', async () => []);
  // Usage surfaces (composer UsageChip, sidebar UsageFooter) fetch
  // ledger aggregates on mount. Default to no usage recorded.
  setBindingMock('GetUsageStats', async () => []);
  // App boot hydrates the per-client appStorage bucket. Default to an
  // empty bucket + no-op writes; tests that assert on persisted view
  // state install their own stateful mocks.
  setBindingMock('GetUIState', async () => ({}));
  setBindingMock('SetUIState', async () => null);
  setBindingMock('DeleteUIState', async () => null);
  // App boot warms the highlight schema-version + class-name tables
  // (warmHighlightTables) so history rows' persisted-span ingest can
  // seed synchronously.
  setBindingMock('HighlightSchemaVersion', async () => 'hv-integration');
  setBindingMock('HighlightClassNames', async () => ['none']);
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
    path: INTEGRATION_WORKSPACE,
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
// mounts GitActionsControl (header) and the in-card ComposerWorkspaceStrip
// hosts BranchPicker, which call GetGitStatus / GitListBranches in
// $effect. Tests that switch into a thread need these mocked even if they
// don't assert on git UI.
export function installThreadViewDefaults(): void {
  setBindingMock('SwitchThread', async (threadId: unknown) => {
    const id = typeof threadId === 'string' ? threadId : 'thread-1';
    const listedThread = getThreads().find((thread) => thread.id === id);
    if (listedThread) return listedThread;
    return makeThread({ id });
  });
  // ChatView may fire MarkThreadRead while keeping the active thread row
  // read as completed turns settle; default it to a no-op so tests that
  // do not care about read-state do not need their own mock.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  setBindingMock('AutoResumeThread', async () => {});
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('GetThreadLiveState', async (threadId: string) => ({
    threadId,
    activeTurn: null,
    queueItems: [...getQueueForThread(threadId)],
    interactive: { approvals: [], userInputs: [] },
    todo: null,
  }));
  setBindingMock('ListItems', async () => []);
  // The message-nav rail reads its whole-thread tick baseline once per
  // thread switch — default to none so unrelated tests stay quiet.
  setBindingMock('GetThreadUserMessageTicks', async () => []);
  // The composer toolbar's MCP trigger holds the pane's MCP entity while it
  // is mounted, so switching into a thread lists its servers once.
  setBindingMock('ListThreadMcpServers', async () => []);
  setBindingMock('ListWorkspaceMcpServers', async () => []);
  // switchThread rehydrates latestSettledTurn via ListRecentTurns — default
  // to an empty list so tests that don't care about turn history don't
  // need to set the mock themselves.
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('GetGitStatus', async () => makeGitStatus());
  // The header subscribes to backend gitwatch instead of polling. Default to
  // a successful subscribe returning the same status as GetGitStatus so the
  // header renders the split-button. `cwd` is the canonical directory the
  // backend watches and the value `git:status` pushes are addressed by; it
  // matches makeThread's workspacePath so a pushed event routes to the
  // thread's store key.
  setBindingMock('GitStatusSubscribe', async () => ({
    id: 'integration-sub',
    cwd: INTEGRATION_WORKSPACE,
    status: makeGitStatus(),
  }));
  setBindingMock('GitStatusUnsubscribe', async () => {});
  // The first observation for a workspace reconciles its branch onto every
  // thread row there. Echo the rows back with the branch applied, so the
  // reconciliation settles instead of erroring in suites that never look at
  // git at all.
  setBindingMock('UpdateThreadBranch', async (workspacePath: unknown, branch: unknown) =>
    getThreads()
      .filter((thread) => thread.workspacePath === workspacePath)
      .map((thread) => ({ ...thread, branch: branch as string })));
  setBindingMock('GitListBranches', async () => []);
  // Thread-wide aggregate surfaces (PlanSidebar / ActivityRail) fetch
  // these bindings on mount / thread-switch.
  // Default to empty lists so tests that don't assert on those
  // surfaces don't have to stub each one themselves.
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListProposedPlanComments', async () => []);
  setBindingMock('GetPayloadData', async () => ({ data: '# Plan\n\nBody' }));
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetWorkspaceActivity', async () => idleWorkspaceActivity());
  setBindingMock('AppendUIRenderTraceBatch', async () => '/tmp/ui-render.jsonl');
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
    root: INTEGRATION_WORKSPACE,
  }));
  // Default RegisterQueueItem mock — the backend handler stores the
  // item and emits provider:queue_state_changed which seeds Zone 1.
  // The mock simulates that round-trip by both returning the wire item
  // AND directly seeding the local store, so integration tests that
  // exercise the mid-turn queue path don't need to reproduce the event
  // plumbing themselves. Tests that need to spy on or reject the call
  // can override with their own setBindingMock.
  let queueSeq = 0;
  setBindingMock('RegisterQueueItem', async (
    targetThreadId: string,
    message: string,
    opts: {
      attachmentIds?: string[];
      sourceProposedPlan?: unknown;
      revisionSourceProposedPlan?: unknown;
      revisionSourceCommentIds?: string[];
    } = {},
  ) => {
    queueSeq += 1;
    const wire = {
      id: `q-${queueSeq}`,
      threadId: targetThreadId,
      message,
      attachmentIds: opts.attachmentIds ? [...opts.attachmentIds] : [],
      sourceProposedPlan: opts.sourceProposedPlan ?? null,
      revisionSourceProposedPlan: opts.revisionSourceProposedPlan ?? null,
      revisionSourceCommentIds: opts.revisionSourceCommentIds
        ? [...opts.revisionSourceCommentIds]
        : undefined,
      enqueuedAt: queueSeq,
    };
    const current = getQueueForThread(targetThreadId);
    replaceQueueForThread(targetThreadId, [
      ...current,
      {
        id: wire.id,
        threadId: wire.threadId,
        message: wire.message,
        attachmentIds: wire.attachmentIds,
        sourceProposedPlan: wire.sourceProposedPlan as never,
        revisionSourceProposedPlan: wire.revisionSourceProposedPlan as never,
        revisionSourceCommentIds: wire.revisionSourceCommentIds,
        enqueuedAt: wire.enqueuedAt,
      },
    ]);
    return wire;
  });
  setBindingMock('GetQueueState', async (targetThreadId: string) => {
    return [...getQueueForThread(targetThreadId)];
  });
}

export function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Integration Thread',
    provider: 'claude',
    workspacePath: INTEGRATION_WORKSPACE,
    projectPath: INTEGRATION_WORKSPACE,
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
    // Default to a github-classified origin so tests that exercise the
    // PR creation flow don't trip over the unsupported-forge gate. Tests
    // that specifically want to exercise the unsupported case can pass
    // `{ forge: '' }` via overrides.
    forge: 'github',
    ...overrides,
  };
}

/**
 * Element.animate shim. happy-dom doesn't implement it, and Svelte's
 * transitions fall over when they probe for it. Suites that mount dialogs
 * with transition:fade or scale should call this from beforeAll.
 */
export function installAnimateShim(): void {
  if (typeof (Element.prototype as unknown as { getAnimations?: unknown }).getAnimations !== 'function') {
    (Element.prototype as unknown as { getAnimations: () => unknown[] }).getAnimations =
      function fakeGetAnimations() {
        return [];
      };
  }
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
