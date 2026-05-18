// ChatView structural sanity tests. The old responsive-header behavior
// (inline ModelPicker / BranchToolbar / RuntimeModePicker at wide widths,
// CompactHeaderMenu at narrow widths) is gone — those pickers moved to
// the composer toolbar + below-composer bar in Waves 3a/3b. What's left
// is "does ChatView wire the right children?". This file asserts the
// visible contract that's still meaningful after the rewrite.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import appCss from '../../../app.css?raw';
import ChatView from './ChatView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { resetComposerDraftSnapshotsForTest } from '../../stores/composerDraft.svelte';
import { getThreads, refreshThreads } from '../../stores/threads.svelte';
import {
  getThreadStatus,
  projectTurnStarted,
  projectTurnCompleted,
  projectThreadItem,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';
import type { Item, Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem } from '../../../test/helpers/chat';
import { resetLayoutMetricsForTest, setPaneWidth } from '../../stores/layoutMetrics.svelte';

beforeAll(() => {
  // Svelte transitions used by children call element.animate; happy-dom
  // doesn't implement it. Keep a minimal shim — the chat directory's
  // tests have relied on this for several waves.
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
});

beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', {
    configurable: true,
    writable: true,
    value: 1400,
  });
  resetLayoutMetricsForTest();
  resetPanesForTest();
  resetThreadStatuses();
  resetComposerDraftSnapshotsForTest();
});

function seedThread(): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function mockDrafts(contents: Map<string, string> = new Map()): Map<string, string> {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId,
    content: contents.get(threadId) ?? '',
    attachmentIds: [],
    terminalChips: [],
    sourceProposedPlan: null,
    updatedAt: 0,
  }));
  return contents;
}

async function buildPane(
  thread: Thread = seedThread(),
  items: Item[] = [],
  paneId = 'pane',
): Promise<ReturnType<typeof createThreadPane>> {
  setBindingMock('SwitchThread', async () => thread);
  // ChatView's auto-mark-read $effect fires on every pane.thread change.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  setBindingMock('ListItems', async () => items);
  setBindingMock('ListThreadSliceAround', async () => ({
    items,
    oldestTurnIndex: items.length > 0 ? items[0].turnIndex : -1,
    hasMore: false,
  }));
  setBindingMock('ListRecentThreadItems', async () => ({
    items,
    oldestTurnIndex: items.length > 0 ? items[0].turnIndex : -1,
    hasMore: false,
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  // Thread-wide aggregate bindings — PlanSidebar / ActivityRail fetch
  // these on mount. Default to empty; tests that need a populated rail
  // override these before rendering.
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  // GitActionsControl calls GetGitStatus on mount; return "not a repo"
  // so the control renders nothing — we don't need a branch chip.
  setBindingMock('GetGitStatus', async () => ({
    isRepo: false,
    branch: '',
    hasChanges: false,
    hasUpstream: false,
    isDefaultBranch: false,
    aheadCount: 0,
    behindCount: 0,
    openPrUrl: '',
    dirty: false,
    files: [],
  }));
  // BranchPicker calls GitListBranches on mount.
  setBindingMock('GitListBranches', async () => []);
  mockDrafts(new Map([[thread.id, '']]));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);

  const pane = createThreadPane({ paneId });
  // ChatView's read-mark + attention-clear effects are now gated on
  // getFocusedPaneId() === pane.paneId. Register + focus the test pane
  // so tests that exercise "user is viewing this thread" behavior have
  // the focus precondition satisfied. Tests that need to assert the
  // background-pane behavior (the user is NOT focused on this pane)
  // can override focus after this returns.
  registerPaneForTest(paneId, pane);
  focusPane(paneId);
  await pane.switchThread(thread);
  return pane;
}

describe('<ChatView>', () => {
  it('renders the chat header with title + always-visible controls', async () => {
    const pane = await buildPane();
    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(getByTestId('chat-header')).toBeInTheDocument();
    expect(getByTestId('chat-header-title')).toBeInTheDocument();
    expect(getByTestId('diff-panel-toggle')).toBeInTheDocument();
    expect(queryByTestId('plan-sidebar-toggle')).toBeNull();
  });

  it('renders the in-card workspace strip', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();
    expect(getByTestId('composer-workspace-strip')).toBeInTheDocument();
  });

  it.each([
    ['conversation-and-files', 'revert-conversation-and-files'] as const,
    ['conversation-only', 'revert-conversation-only'] as const,
  ])('opens the message revert popover from a user-message action and applies %s', async (mode, actionTestId) => {
    const thread = seedThread();
    const userItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'Update one of the lines',
    });
    const pane = await buildPane(thread, [userItem]);
    pane.diffPanel.setCheckpoints([{
      id: 'checkpoint-1',
      threadId: thread.id,
      userItemId: userItem.id,
      turnIndex: userItem.turnIndex,
      status: 'ready',
      files: [],
      capturedAt: 1,
    }]);
    const preview = setBindingMock('GetMessageCheckpointRevertDiff', async () => `diff --git a/scratch.txt b/scratch.txt
--- a/scratch.txt
+++ b/scratch.txt
@@ -1 +1 @@
-old
+new
`);
    const draftContent = mockDrafts(new Map([[thread.id, '']]));
    const revert = setBindingMock('RevertToMessageCheckpoint', async () => {
      draftContent.set(thread.id, userItem.summary);
    });

    const { getByLabelText, findByTestId, getByTestId } = render(ChatView, { props: { pane } });
    await fireEvent.click(getByLabelText('Revert to this message'));

    expect(await findByTestId('user-message-revert-popover')).toBeInTheDocument();
    expect(preview).toHaveBeenCalledWith(thread.id, userItem.id);
    expect(getByTestId('revert-conversation-and-files')).toHaveTextContent('+1');
    expect(getByTestId('revert-conversation-and-files')).toHaveTextContent('-1');

    await fireEvent.click(getByTestId(actionTestId));
    await waitFor(() => {
      expect(revert).toHaveBeenCalledWith(thread.id, userItem.id, mode);
      expect(getByLabelText('Message Input')).toHaveValue(userItem.summary);
    });
  });

  it('drops a pending message revert target when the pane switches threads', async () => {
    const thread = seedThread();
    const userItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'Update one of the lines',
    });
    const pane = await buildPane(thread, [userItem]);
    pane.diffPanel.setCheckpoints([{
      id: 'checkpoint-1',
      threadId: thread.id,
      userItemId: userItem.id,
      turnIndex: userItem.turnIndex,
      status: 'ready',
      files: [],
      capturedAt: 1,
    }]);
    setBindingMock('GetMessageCheckpointRevertDiff', async () => 'diff --git a/scratch.txt b/scratch.txt\n');

    const { getByLabelText, findByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await fireEvent.click(getByLabelText('Revert to this message'));
    expect(await findByTestId('user-message-revert-popover')).toBeInTheDocument();

    const otherThread = { ...thread, id: 'thread-2', title: 'Other thread' };
    setBindingMock('SwitchThread', async () => otherThread);
    await pane.switchThread(otherThread);
    await tick();

    expect(queryByTestId('user-message-revert-popover')).toBeNull();
  });

  it('clears the open message revert target before loading a different preview', async () => {
    const thread = seedThread();
    const firstItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'First update',
    });
    const secondItem = makeItem({
      id: 'user:2',
      threadId: thread.id,
      turnIndex: 2,
      itemIndex: 1,
      kind: 'user_text',
      role: 'user',
      summary: 'Second update',
    });
    const pane = await buildPane(thread, [firstItem, secondItem]);
    pane.diffPanel.setCheckpoints([
      {
        id: 'checkpoint-1',
        threadId: thread.id,
        userItemId: firstItem.id,
        turnIndex: firstItem.turnIndex,
        status: 'ready',
        files: [],
        capturedAt: 1,
      },
      {
        id: 'checkpoint-2',
        threadId: thread.id,
        userItemId: secondItem.id,
        turnIndex: secondItem.turnIndex,
        status: 'ready',
        files: [],
        capturedAt: 2,
      },
    ]);
    let rejectSecondPreview: (err: Error) => void = () => {};
    const secondPreview = new Promise<string>((_, reject) => {
      rejectSecondPreview = reject;
    });
    const preview = setBindingMock('GetMessageCheckpointRevertDiff', async (_threadId: string, itemId: string) => {
      if (itemId === firstItem.id) return 'diff --git a/first.txt b/first.txt\n';
      return secondPreview;
    });
    const revert = setBindingMock('RevertToMessageCheckpoint', async () => {});

    const { getAllByLabelText, findByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    const revertButtons = getAllByLabelText('Revert to this message');

    await fireEvent.click(revertButtons[0]);
    expect(await findByTestId('user-message-revert-popover')).toBeInTheDocument();

    await fireEvent.click(revertButtons[1]);
    await waitFor(() => {
      expect(preview).toHaveBeenCalledWith(thread.id, secondItem.id);
    });
    expect(queryByTestId('user-message-revert-popover')).toBeNull();

    rejectSecondPreview(new Error('preview failed'));
    await waitFor(() => {
      expect(queryByTestId('user-message-revert-popover')).toBeNull();
    });
    expect(revert).not.toHaveBeenCalled();
  });

  it('closes an open message revert popover when the thread starts working', async () => {
    const thread = seedThread();
    const userItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'Update one of the lines',
    });
    const pane = await buildPane(thread, [userItem]);
    pane.diffPanel.setCheckpoints([{
      id: 'checkpoint-1',
      threadId: thread.id,
      userItemId: userItem.id,
      turnIndex: userItem.turnIndex,
      status: 'ready',
      files: [],
      capturedAt: 1,
    }]);
    setBindingMock('GetMessageCheckpointRevertDiff', async () => 'diff --git a/scratch.txt b/scratch.txt\n');
    const revert = setBindingMock('RevertToMessageCheckpoint', async () => {});

    const { getByLabelText, findByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await fireEvent.click(getByLabelText('Revert to this message'));
    expect(await findByTestId('user-message-revert-popover')).toBeInTheDocument();

    projectTurnStarted(thread.id, 'turn-active', 2, Date.now());
    await tick();

    expect(queryByTestId('user-message-revert-popover')).toBeNull();
    expect(revert).not.toHaveBeenCalled();
  });

  it('forks from a user-message action through the chat-level handler', async () => {
    const thread = seedThread();
    const userItem = makeItem({
      id: 'user:1',
      threadId: thread.id,
      turnIndex: 1,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      summary: 'Update one of the lines',
    });
    const pane = await buildPane(thread, [userItem]);
    pane.diffPanel.setCheckpoints([{
      id: 'checkpoint-1',
      threadId: thread.id,
      userItemId: userItem.id,
      turnIndex: userItem.turnIndex,
      status: 'ready',
      files: [],
      capturedAt: 1,
    }]);
    const forked = {
      ...thread,
      id: 'fork-1',
      projectId: 'project-1',
      title: 'Forked thread',
    };
    mockDrafts(new Map([
      [thread.id, ''],
      [forked.id, userItem.summary],
    ]));
    const fork = setBindingMock('ForkThreadFromMessage', async () => forked);
    setBindingMock('SwitchThread', async () => forked);

    const { getByLabelText } = render(ChatView, { props: { pane } });
    await fireEvent.click(getByLabelText('Fork from this message'));

    await waitFor(() => {
      expect(fork).toHaveBeenCalledWith(thread.id, userItem.id);
      expect(pane.thread?.id).toBe('fork-1');
      expect(getByLabelText('Message Input')).toHaveValue(userItem.summary);
    });
  });

  it('keeps one stable right-sidebar shell while swapping panel content', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '',
      nextOffset: 0,
      totalSize: 0,
      isComplete: true,
    }));
    const pane = await buildPane();
    setPaneWidth(pane.paneId, 1400);
    pane.setShowPlanSidebar(true);
    pane.setRhsSidebarWidthLive(620);

    const { getByTestId, queryAllByTestId, findByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(queryAllByTestId('rhs-sidebar-shell')).toHaveLength(1);
    expect(getByTestId('rhs-sidebar-shell')).toHaveStyle({ width: '620px' });
    expect(getByTestId('plan-sidebar')).toBeInTheDocument();

    pane.setDiffPanelOpen(true);
    await tick();
    expect(queryAllByTestId('rhs-sidebar-shell')).toHaveLength(1);
    expect(getByTestId('rhs-sidebar-shell')).toHaveStyle({ width: '620px' });
    expect(getByTestId('diff-panel-drawer')).toBeInTheDocument();

    pane.openDiffSidebar({ payloadId: 'payload-1' });
    await tick();
    expect(queryAllByTestId('rhs-sidebar-shell')).toHaveLength(1);
    expect(getByTestId('rhs-sidebar-shell')).toHaveStyle({ width: '620px' });
    expect(await findByTestId('diff-sidebar')).toBeInTheDocument();
  });

  it('renders RHS panels as a pane-local overlay below 880px', async () => {
    const pane = await buildPane();
    registerPaneForTest(pane.paneId, pane);
    focusPane(pane.paneId);
    setPaneWidth(pane.paneId, 700);
    pane.setShowPlanSidebar(true);

    const { getByTestId, queryByTestId } = render(ChatView, { props: { pane } });
    await tick();

    const shell = getByTestId('rhs-sidebar-shell');
    expect(shell.dataset.rhsMode).toBe('overlay');
    expect(shell).toHaveClass('absolute');
    expect(queryByTestId('rhs-sidebar-resizer')).toBeNull();

    pane.closeRhsPanel();
    await tick();

    await waitFor(() => expect(queryByTestId('rhs-sidebar-shell')).toBeNull());
  });

  it('renders design preview through the RHS shell only after explicit toggle', async () => {
    setBindingMock('EnsureDesignWorkdir', async () => {});
    setBindingMock('LatestDesignOptionSet', async () => null);
    const pane = await buildPane({ ...seedThread(), mode: 'design' });
    const { getByTestId, queryByTestId, queryAllByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(queryByTestId('design-split')).toBeNull();
    expect(queryByTestId('design-split-resizer')).toBeNull();
    expect(queryByTestId('rhs-sidebar-shell')).toBeNull();

    await fireEvent.click(getByTestId('design-preview-toggle'));

    await waitFor(() => expect(queryAllByTestId('rhs-sidebar-shell')).toHaveLength(1));
    expect(queryByTestId('design-split')).toBeNull();
    await waitFor(() => expect(getByTestId('design-preview-iframe')).toBeInTheDocument());

    const shell = getByTestId('rhs-sidebar-shell');
    pane.setActiveOptionSet({ setId: 'set-1', optionPaths: ['options/set-1/alpha'] });
    await tick();

    expect(queryAllByTestId('rhs-sidebar-shell')).toHaveLength(1);
    expect(getByTestId('rhs-sidebar-shell')).toBe(shell);
    expect(getByTestId('design-options-panel')).toBeInTheDocument();
  });

  it('keeps design clarification controls in the chat column', async () => {
    setBindingMock('EnsureDesignWorkdir', async () => {});
    setBindingMock('LatestDesignOptionSet', async () => null);
    const pane = await buildPane({ ...seedThread(), mode: 'design' });
    pane.setPendingClarification({
      requestId: 'clarify-1',
      threadId: pane.threadId ?? 'thread-1',
      intro: 'Pick a direction',
      questions: [{
        id: 'direction',
        prompt: 'Which direction should the agent take?',
        choices: [{ id: 'simple', label: 'Simpler' }],
      }],
    });

    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    const overlay = getByTestId('composer-overlay');
    const picker = getByTestId('design-clarification-picker');
    expect(overlay).toContainElement(picker);

    await fireEvent.click(getByTestId('design-preview-toggle'));
    await waitFor(() => expect(getByTestId('rhs-sidebar-shell')).toBeInTheDocument());

    const shell = getByTestId('rhs-sidebar-shell');
    expect(shell).not.toContainElement(picker);
  });

  it('does not render interaction-mode / runtime-mode / branch pickers in the header', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ChatView, { props: { pane } });
    await tick();
    // These IDs belonged to the old header chrome; they must be gone
    // from ChatView entirely (the mode cycle button is the composer
    // toolbar's concern now, and the branch picker lives below the
    // composer).
    expect(queryByTestId('interaction-mode-badge')).toBeNull();
    expect(queryByTestId('runtime-mode-trigger')).toBeNull();
    expect(queryByTestId('chat-header-compact')).toBeNull();
    expect(queryByTestId('compact-header-menu-trigger')).toBeNull();
  });

  it('renders a minimal placeholder when no thread is selected', async () => {
    const pane = createThreadPane();
    const { queryByTestId, getByText } = render(ChatView, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header')).toBeNull();
    expect(queryByTestId('chat-empty')).not.toBeNull();
    expect(getByText('Select a thread or create a new one to get started.')).toBeInTheDocument();
  });

  it('keeps the bounded chat background on timeline and empty states', async () => {
    const activePane = await buildPane();
    const active = render(ChatView, { props: { pane: activePane } });
    await tick();

    // The scroll element is now wrapped in a non-scrolling
    // `relative h-full` container that anchors the floating
    // ScrollToBottomButton outside the scroll viewport. We check the
    // chat-surface-ground class on the nearest ancestor with that
    // class, not strictly the parentElement, so the wrapper insertion
    // doesn't break the contract this test cares about: the timeline
    // ground sits behind the timeline.
    expect(active.getByTestId('message-timeline-scroll').closest('.chat-surface-ground'))
      .not.toBeNull();
    active.unmount();

    const emptyPane = createThreadPane();
    const empty = render(ChatView, { props: { pane: emptyPane } });
    await tick();

    expect(empty.getByTestId('chat-empty')).toHaveClass('chat-surface-ground');
  });

  it('does not reintroduce a global blended app overlay', () => {
    expect(appCss).not.toMatch(/body::before/);
    expect(appCss).not.toMatch(/body::after/);
    expect(appCss).not.toMatch(/mix-blend-mode/);
    expect(appCss).not.toMatch(/repeating-linear-gradient/);
  });

  it('marks the active thread read locally and coalesces persisted writes when completed turns arrive', async () => {
    vi.useFakeTimers();
    try {
      const thread = { ...seedThread(), latestTurnCompletedAt: 1_000 };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane } });
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
      expect(getThreads()[0]?.lastReadAt).toBe(1_000);
      // The pane attention-dot overlay reads lastReadAt from pane.thread;
      // the sidebar reads it from the global threads registry. Keeping
      // both in sync is what stops the pane dot from showing a stale
      // "Completed" green pip after the user is already looking at the
      // thread.
      expect(pane.thread?.lastReadAt).toBe(1_000);

      vi.setSystemTime(1_010);
      pane.replaceThread({ ...pane.thread!, updatedAt: 1_010, latestTurnCompletedAt: 1_010 });
      await tick();

      expect(getThreads()[0]?.lastReadAt).toBe(1_010);
      expect(pane.thread?.lastReadAt).toBe(1_010);
      expect(markRead).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(100);

      expect(markRead).toHaveBeenCalledTimes(2);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
    } finally {
      vi.useRealTimers();
    }
  });

  it('clears interrupted read state locally when the interrupted thread is opened', async () => {
    vi.useFakeTimers();
    try {
      const thread = { ...seedThread(), hasIncompleteTurn: true };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane } });
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
      expect(getThreads()[0]?.lastReadAt).toBe(1_000);
      expect(getThreads()[0]?.hasIncompleteTurn).toBe(false);
      expect(pane.thread?.hasIncompleteTurn).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('clamps the local read marker to the latest completed turn', async () => {
    vi.useFakeTimers();
    try {
      const latestTurnCompletedAt = 2_000;
      const thread = { ...seedThread(), latestTurnCompletedAt };
      setBindingMock('ListThreads', async () => [thread]);
      await refreshThreads();
      const pane = await buildPane(thread);
      setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane } });
      await tick();

      expect(getThreads()[0]?.lastReadAt).toBe(latestTurnCompletedAt);
    } finally {
      vi.useRealTimers();
    }
  });

  it('leaves an unfocused pane unread when a turn completes on its thread', async () => {
    vi.useFakeTimers();
    try {
      // Two panes: 'main' is focused (default after resetPanesForTest),
      // 'background' is mounted but not focused. A turn completes on
      // the background pane's thread; the attention dot must stay
      // because the user hasn't actually seen it yet.
      const focusedThread = { ...seedThread(), id: 'thread-focused', latestTurnCompletedAt: 500 };
      const backgroundThread = { ...seedThread(), id: 'thread-background', latestTurnCompletedAt: 1_000 };
      setBindingMock('ListThreads', async () => [focusedThread, backgroundThread]);
      await refreshThreads();

      // Register a 'main' pane so we can actually focus it; focusPane
      // is a no-op if the target isn't in the registry.
      const mainPane = createThreadPane({ paneId: 'main' });
      registerPaneForTest('main', mainPane);
      const backgroundPane = await buildPane(backgroundThread, [], 'background');
      // buildPane focuses the pane it just built; flip focus back to
      // 'main' so the background pane is genuinely unfocused. Set the
      // markRead spy AFTER buildPane because buildPane installs its own
      // no-op MarkThreadRead mock.
      focusPane('main');
      const markRead = setBindingMock('MarkThreadRead', async () => {});

      vi.setSystemTime(1_000);
      render(ChatView, { props: { pane: backgroundPane } });
      await tick();

      // No auto read-mark fires because the pane isn't focused.
      expect(markRead).not.toHaveBeenCalled();
      expect(backgroundPane.thread?.lastReadAt).toBeUndefined();
      expect(getThreads().find((t) => t.id === 'thread-background')?.lastReadAt).toBeUndefined();

      // A new turn completes on the background pane's thread.
      vi.setSystemTime(2_000);
      backgroundPane.replaceThread({
        ...backgroundPane.thread!,
        latestTurnCompletedAt: 2_000,
        updatedAt: 2_000,
      });
      await tick();

      // Still no read-mark — the user hasn't focused the pane yet.
      expect(markRead).not.toHaveBeenCalled();
      expect(backgroundPane.thread?.lastReadAt).toBeUndefined();

      // User focuses the background pane. The read-mark fires now.
      focusPane('background');
      await tick();

      expect(markRead).toHaveBeenCalledTimes(1);
      expect(markRead).toHaveBeenLastCalledWith('thread-background');
      expect(backgroundPane.thread?.lastReadAt).toBe(2_000);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not rewrite read state when the active thread is already read', async () => {
    const thread = { ...seedThread(), latestTurnCompletedAt: 1_000, lastReadAt: 1_500 };
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    const markRead = setBindingMock('MarkThreadRead', async () => {});

    render(ChatView, { props: { pane } });
    await tick();

    expect(getThreads()[0]?.lastReadAt).toBe(1_500);
    expect(markRead).not.toHaveBeenCalled();
  });

  it('clears a stale sidebar error status once the thread is open', async () => {
    const thread = seedThread();
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    projectThreadItem(makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      status: 'completed',
    }));

    render(ChatView, { props: { pane } });
    await tick();

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('clears a stale sidebar interrupted status once the thread is open', async () => {
    const thread = seedThread();
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();
    const pane = await buildPane(thread);
    projectTurnStarted('thread-1', 'turn-1', 0, 0);
    projectTurnCompleted('thread-1', 'turn-1', { aborted: true });

    render(ChatView, { props: { pane } });
    await tick();

    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('flushes a pending composer draft when the chat view unmounts', async () => {
    const pane = await buildPane();
    const saveDraft = setBindingMock('SaveDraft', async () => {});
    const { getByLabelText, unmount } = render(ChatView, { props: { pane } });
    await tick();

    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'pending draft' } });

    unmount();

    await waitFor(() => {
      expect(saveDraft).toHaveBeenCalledWith('thread-1', 'pending draft', [], [], null);
    });
  });

  it('clicking a background tray row does NOT scroll the timeline (rows are informational)', async () => {
    // Phase 5 of the background-tasks plan removed click-to-scroll on
    // tray rows: they are now purely informational, with per-row Stop
    // buttons and a header-level Stop-all button as the only
    // affordances. A plain click on the row body must NOT publish a
    // scroll request on the pane or invoke scrollIntoView on the
    // timeline item.
    const scrollSpy = vi.fn();
    const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = scrollSpy as typeof HTMLElement.prototype.scrollIntoView;
    try {
      const launch: Item = {
        id: 'launch-a',
        threadId: 'thread-1',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'tool_call',
        role: 'assistant',
        status: 'running',
        summary: 'Bash: sleep 30',
        isBackground: true,
        toolName: 'Bash',
        createdAt: Date.now() - 1_000,
        updatedAt: Date.now() - 1_000,
      };
      const pane = await buildPane();
      setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
      pane.upsertItem(launch);

      const { getByTestId } = render(ChatView, { props: { pane } });
      await tick();
      await tick();

      // Background section defaults to collapsed in production — expand
      // it via the rail toggle before reaching for the row.
      await fireEvent.click(getByTestId('activity-rail-background-toggle'));
      await tick();

      const row = getByTestId('background-task-tray-row');
      expect(row.getAttribute('data-row-id')).toBe('launch-a');
      expect(row.tagName).not.toBe('BUTTON');

      await fireEvent.click(row);
      await tick();
      await tick();

      expect(scrollSpy).not.toHaveBeenCalled();
    } finally {
      HTMLElement.prototype.scrollIntoView = originalScrollIntoView;
    }
  });
});
