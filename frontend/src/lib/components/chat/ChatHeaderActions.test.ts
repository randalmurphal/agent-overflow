import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatHeaderActions from './ChatHeaderActions.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetEditorsForTest } from '../../stores/editors.svelte';
import type { GitStatus } from '../../types/git';
import type { Project, Thread } from '../../types/models';
import { createThreadPane } from '../../stores/thread.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import {
  buildPane as buildRegisteredPane,
  makeThread as makeBaseThread,
  stubScrollController,
} from '../../../test/helpers/chat';

vi.mock('../../stores/threadCreation.svelte', () => ({ openTerminalThread: vi.fn() }));

// Transport is pinned connected globally (src/test/setup.ts) so the git-status
// store sources instead of staying suspended. It is not mocked here: the store
// is a `.svelte.ts` importer of transportStatus, and vi.mock does not reliably
// reach those (frontend/CLAUDE.md § Testing).

// Svelte transitions (Popover/Menu inside GitActionsControl) poke
// Element.animate on mount; jsdom lacks it.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

const WORKSPACE = '/workspace';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    title: 'Example',
    workspacePath: WORKSPACE,
    projectPath: WORKSPACE,
    branch: 'main',
    ...overrides,
  });
}

function makeProject(): Project {
  return {
    id: 'project-1',
    path: WORKSPACE,
    name: 'Example',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function status(overrides: Partial<GitStatus> = {}): GitStatus {
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
    forge: 'github',
    ...overrides,
  };
}

async function buildPane(thread = makeThread()) {
  return buildRegisteredPane(thread);
}

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

// The backend addresses status pushes by canonical cwd, so the subscribe
// result carries it. Defaulting it to the fixture workspace is what lets a
// `git:status` push in these tests route back to the thread's entry.
function installSubscribeMock(initial: GitStatus, id = 'sub-1', cwd = WORKSPACE) {
  const subscribeFn = setBindingMock('GitStatusSubscribe', async () => ({
    id,
    cwd,
    status: initial,
  }));
  setBindingMock('GitStatusUnsubscribe', async () => {});
  return { id, cwd, subscribeFn };
}

describe('<ChatHeaderActions> badge gating', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetEditorsForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread());
    // The Open-in-editor control loads this catalog on mount.
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    await loadSettings();
  });

  it('shows the PR badge and workspace +/- on a chat thread with an open PR', async () => {
    const pane = await buildPane();
    installSubscribeMock(
      status({
        insertions: 4,
        deletions: 1,
        forge: 'github',
        openPrUrl: 'https://github.com/o/r/pull/7',
        openPrNumber: 7,
      }),
    );
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(getByTestId('chat-header-pr-badge').textContent?.replace(/\s+/g, '')).toBe('PR#7');
    expect(getByTestId('review-toggle')).toBeTruthy();
    expect(getByTestId('workspace-diff-counts').textContent).toContain('+4');
    expect(getByTestId('workspace-diff-counts').textContent).toContain('-1');
  });

  it('toggles every activity run in the thread, and says which way', async () => {
    // The one visible affordance for the collapse mechanic: a single run is
    // toggled by its rail, which consumes no width and so shows nothing.
    const pane = await buildPane();
    installSubscribeMock(status({}));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();
    const toggle = getByTestId('activity-runs-toggle');

    expect(pane.activityRuns.bulkCollapsed).toBe(false);
    expect(toggle.getAttribute('aria-label')).toBe('Collapse all activity runs');

    await fireEvent.click(toggle);
    await flush();

    expect(pane.activityRuns.bulkCollapsed).toBe(true);
    expect(toggle.getAttribute('aria-label')).toBe('Expand all activity runs');

    await fireEvent.click(toggle);
    await flush();

    expect(pane.activityRuns.bulkCollapsed).toBe(false);
  });

  it('runs the bulk toggle inside the viewport-bottom transaction', async () => {
    // Collapse-all is the largest height change in the app. Applied bare it
    // pushes the reader's rows down the page and, from the bottom, springs the
    // viewport across the whole delta.
    const pane = await buildPane();
    const held: Array<() => void> = [];
    pane.attachScrollController(
      stubScrollController({
        preserveViewportBottom: (change) => {
          held.push(change);
        },
      }),
    );
    installSubscribeMock(status({}));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    await fireEvent.click(getByTestId('activity-runs-toggle'));
    await flush();

    // Withheld, so the toggle demonstrably did not reach the registry on its
    // own — the transaction owns when it applies.
    expect(held).toHaveLength(1);
    expect(pane.activityRuns.bulkCollapsed).toBe(false);

    held[0]();
    expect(pane.activityRuns.bulkCollapsed).toBe(true);
  });

  it('hides the run toggle on a design thread, which has no transcript runs', async () => {
    const pane = await buildPane(makeThread({ mode: 'design' }));
    installSubscribeMock(status({}));
    const { queryByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(queryByTestId('activity-runs-toggle')).toBeNull();
  });

  it('hides the PR badge but keeps the workspace +/- when there is no open PR', async () => {
    const pane = await buildPane();
    installSubscribeMock(status({ openPrUrl: '' }));
    const { queryByTestId, getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
    expect(getByTestId('review-toggle')).toBeTruthy();
  });

  it('hides PR + workspace +/- and shows Design Preview on a design thread', async () => {
    const pane = await buildPane(makeThread({ mode: 'design' }));
    installSubscribeMock(
      status({ openPrUrl: 'https://github.com/o/r/pull/7', openPrNumber: 7 }),
    );
    const { queryByTestId, getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
    expect(queryByTestId('review-toggle')).toBeNull();
    expect(getByTestId('design-preview-toggle')).toBeTruthy();
  });

  it('clicking the workspace +/- opens review on the workspace scope', async () => {
    const pane = await buildPane();
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 1 }]);
    installSubscribeMock(status({ insertions: 2, deletions: 0 }));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();

    expect(pane.showReviewPane).toBe(false);
    await fireEvent.click(getByTestId('review-toggle'));
    expect(pane.showReviewPane).toBe(true);
  });
});

describe('<ChatHeaderActions> subscription effect', () => {
  beforeEach(async () => {
    resetPanesForTest();
    resetEditorsForTest();
    setBindingMock('GetSettings', async () => null);
    setBindingMock('GetProviderStatuses', async () => []);
    setBindingMock('UpdateThreadBranch', async () => makeThread());
    // The Open-in-editor control loads this catalog on mount.
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    await loadSettings();
  });

  it('does NOT re-subscribe when pane.replaceThread updates unrelated metadata', async () => {
    // Regression guard for the per-token flicker: the attach effect tracks the
    // $derived store key, whose value-equality short-circuits re-runs when only
    // token usage / mode changed.
    const thread = makeThread({ workspacePath: WORKSPACE });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status({ hasChanges: true }));
    render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    pane.replaceThread({ ...thread, lastTokenUsage: '{"used":1234}' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(getBindingMock('GitStatusUnsubscribe')).not.toHaveBeenCalled();
  });

  it('does NOT re-subscribe when the pane switches threads inside one workspace', async () => {
    // The entity is the WORKSPACE. Switching which thread a pane shows says
    // nothing about the checkout, so a release + re-attach here would bounce
    // the backend refcount through zero: the fs watcher torn down and
    // rebuilt, and every badge in the header blanked, for nothing.
    const thread = makeThread({ id: 'thread-a', workspacePath: WORKSPACE });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status({ insertions: 3 }));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    pane.replaceThread(makeThread({ id: 'thread-b', workspacePath: WORKSPACE }));
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(1);
    expect(getBindingMock('GitStatusUnsubscribe')).not.toHaveBeenCalled();
    // And the observation is still there — no blank frame on the switch.
    expect(getByTestId('workspace-diff-counts').textContent).toContain('+3');
  });

  it('subscribes against the thread the pane holds NOW, not the one it attached with', async () => {
    // The reference outlives any one thread, so a re-source has to name a
    // thread that still exists — the id it attached with may since have been
    // deleted along with its pane's previous conversation.
    const pane = await buildPane(makeThread({ id: 'thread-a', workspacePath: WORKSPACE }));
    const { subscribeFn } = installSubscribeMock(status());
    render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledWith('thread-a');

    pane.replaceThread(makeThread({ id: 'thread-b', workspacePath: WORKSPACE }));
    await flush();

    // A reconnect re-acquires every key; the store asks the ctx again.
    __setTransportStatusForTest({ status: 'reconnecting', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();

    expect(subscribeFn).toHaveBeenLastCalledWith('thread-b');
  });

  it('resubscribes when the workspace path changes', async () => {
    const thread = makeThread({ workspacePath: WORKSPACE });
    const pane = await buildPane(thread);
    const { subscribeFn } = installSubscribeMock(status());
    render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    // A worktree move rewrites workspace_path — a different checkout, so a
    // different entity and a different watcher.
    pane.replaceThread({ ...thread, workspacePath: '/wt/branch-a', worktreePath: '/wt/branch-a' });
    await flush();

    expect(subscribeFn).toHaveBeenCalledTimes(2);
    expect(getBindingMock('GitStatusUnsubscribe')).toHaveBeenCalledTimes(1);
  });

  it('loads the worktree PR badge when a placeholder gains its durable row', async () => {
    const pane = createThreadPane();
    pane.startDraftPlaceholder(makeProject(), 'chat', {
      provider: 'claude',
      model: 'm',
      workspacePath: '/wt/branch-a',
      branch: 'branch-a',
    });
    const created = makeThread({
      id: 'draft-row',
      isDraft: true,
      workspacePath: '/wt/branch-a',
      worktreePath: '/wt/branch-a',
      branch: 'branch-a',
    });
    setBindingMock('CreateThread', async () => created);
    const { subscribeFn } = installSubscribeMock(
      status({
        branch: 'branch-a',
        forge: 'gitlab',
        openPrUrl: 'https://gitlab.com/o/r/-/merge_requests/9',
        openPrNumber: 9,
      }),
      'sub-worktree',
      '/wt/branch-a',
    );

    const { getByTestId, queryByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();
    expect(queryByTestId('chat-header-pr-badge')).toBeNull();
    expect(subscribeFn).not.toHaveBeenCalled();

    await pane.ensureMaterializedThread();
    await flush();

    expect(subscribeFn).toHaveBeenCalledWith('draft-row');
    expect(getByTestId('chat-header-pr-badge').textContent?.replace(/\s+/g, '')).toBe('MR!9');
  });

  it('updates the workspace +/- when a live git:status push arrives', async () => {
    // End-to-end through the mounted component: ChatHeaderActions attaches the
    // workspace entity, the store routes the push by canonical cwd, and the
    // badge re-renders off the shared observation. The store unit test covers
    // the routing in isolation; this adds the mount + render path on top.
    const pane = await buildPane();
    const { cwd } = installSubscribeMock(status({ insertions: 1, deletions: 0 }));
    const { getByTestId } = render(ChatHeaderActions, { props: { pane } });
    await flush();
    // Initial value comes from the subscribe result, not the listener.
    expect(getByTestId('workspace-diff-counts').textContent).toContain('+1');

    emitWailsEvent('git:status', {
      cwd,
      status: status({ insertions: 99, deletions: 7 }),
    });
    await flush();

    expect(getByTestId('workspace-diff-counts').textContent).toContain('+99');
    expect(getByTestId('workspace-diff-counts').textContent).toContain('-7');
  });

  it('two panes on one workspace share a single subscription', async () => {
    // The defect the entity keying exists to make impossible: a second pane on
    // the same worktree used to open its own watcher and hold a private copy,
    // so the two headers could disagree about whether there was anything to
    // commit until both happened to refresh.
    const left = await buildPane(makeThread({ id: 'thread-left' }));
    const right = await buildPane(makeThread({ id: 'thread-right' }));
    const { cwd, subscribeFn } = installSubscribeMock(status({ insertions: 1 }));

    const a = render(ChatHeaderActions, { props: { pane: left } });
    const b = render(ChatHeaderActions, { props: { pane: right } });
    await flush();
    expect(subscribeFn).toHaveBeenCalledTimes(1);

    emitWailsEvent('git:status', { cwd, status: status({ insertions: 42 }) });
    await flush();
    // Scoped to each render's own container: both headers are in one document,
    // so the default body-wide queries would match twice.
    expect(within(a.container).getByTestId('workspace-diff-counts').textContent).toContain('+42');
    expect(within(b.container).getByTestId('workspace-diff-counts').textContent).toContain('+42');

    a.unmount();
    await flush();
    expect(getBindingMock('GitStatusUnsubscribe')).not.toHaveBeenCalled();
    b.unmount();
    await flush();
    expect(getBindingMock('GitStatusUnsubscribe')).toHaveBeenCalledTimes(1);
  });
});
