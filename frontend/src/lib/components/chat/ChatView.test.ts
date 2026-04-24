// ChatView structural sanity tests. The old responsive-header behavior
// (inline ModelPicker / BranchToolbar / RuntimeModePicker at wide widths,
// CompactHeaderMenu at narrow widths) is gone — those pickers moved to
// the composer toolbar + below-composer bar in Waves 3a/3b. What's left
// is "does ChatView wire the right children?". This file asserts the
// visible contract that's still meaningful after the rewrite.

import { describe, expect, it, beforeAll, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatView from './ChatView.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { getThreads, refreshThreads } from '../../stores/threads.svelte';
import type { Item, Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

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

async function buildPane(thread: Thread = seedThread()): Promise<ReturnType<typeof createThreadPane>> {
  setBindingMock('SwitchThread', async () => thread);
  // ChatView's auto-mark-read $effect fires on every pane.thread change.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListRecentThreadItems', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  // Thread-wide aggregate bindings — PlanSidebar / DiffPanelDrawer /
  // BackgroundTaskTray fetch these on mount. Default to empty; tests
  // that need a populated tray override these before rendering.
  setBindingMock('ListThreadProposedPlans', async () => []);
  setBindingMock('ListThreadDiffPayloads', async () => []);
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
  // Composer fetches slash commands lazily when the user types `/` —
  // not on mount — but the binding mock throws on unexpected calls, so
  // stub it defensively.
  setBindingMock('GetThreadSlashCommands', async () => []);

  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}

describe('<ChatView>', () => {
  it('renders the chat header with title + always-visible controls', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();

    expect(getByTestId('chat-header')).toBeInTheDocument();
    expect(getByTestId('chat-header-title')).toBeInTheDocument();
    expect(getByTestId('chat-header-provider')).toBeInTheDocument();
    expect(getByTestId('diff-panel-toggle')).toBeInTheDocument();
    expect(getByTestId('plan-sidebar-toggle')).toBeInTheDocument();
  });

  it('renders the below-composer bar', async () => {
    const pane = await buildPane();
    const { getByTestId } = render(ChatView, { props: { pane } });
    await tick();
    expect(getByTestId('below-composer-bar')).toBeInTheDocument();
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

  it('renders the empty-state when no thread is selected', async () => {
    const pane = createThreadPane();
    const { queryByTestId, getByText } = render(ChatView, { props: { pane } });
    await tick();
    expect(queryByTestId('chat-header')).toBeNull();
    expect(getByText('Select or create a thread')).toBeInTheDocument();
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

      vi.setSystemTime(1_010);
      pane.replaceThread({ ...pane.thread!, updatedAt: 1_010, latestTurnCompletedAt: 1_010 });
      await tick();

      expect(getThreads()[0]?.lastReadAt).toBe(1_010);
      expect(markRead).toHaveBeenCalledTimes(1);

      await vi.advanceTimersByTimeAsync(100);

      expect(markRead).toHaveBeenCalledTimes(2);
      expect(markRead).toHaveBeenLastCalledWith('thread-1');
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
