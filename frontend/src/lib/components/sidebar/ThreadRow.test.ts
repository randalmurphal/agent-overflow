import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThreadRow from './ThreadRow.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { refreshThreads, getThreads } from '../../stores/threads.svelte';
import {
  beginThreadLiveStateHydration,
  finishThreadLiveStateHydration,
  resetForTest as resetThreadStatuses,
  setThreadStatus,
} from '../../stores/threadStatuses.svelte';
import {
  resetKeybindingsStore,
  setKeybindingsForTest,
} from '../../stores/keybindings.svelte';
import {
  resetKeyboardModifiersForTest,
  subscribeJumpHints,
} from '../../stores/keyboardModifiers.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitItemEventUpsert } from '../../../test/helpers/chat';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test Thread',
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

async function primeSettings() {
  setBindingMock('GetSettings', async () => null);
  await loadSettings();
}

function nextFrame(): Promise<void> {
  return new Promise((resolve) => {
    requestAnimationFrame(() => resolve());
  });
}

describe('<ThreadRow> unarchive', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    resetKeybindingsStore();
    resetKeyboardModifiersForTest();
  });

  it('does not show the unarchive action for an active thread', async () => {
    const thread = makeThread({ archived: false });
    const pane = createThreadPane();
    const { queryByTestId, getByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(queryByTestId('thread-row-unarchive')).toBeNull();
    expect(getByTestId('thread-row-archive')).toBeInTheDocument();
  });

  it('shows the unarchive action for an archived thread', async () => {
    const thread = makeThread({ archived: true });
    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(getByTestId('thread-row-unarchive')).toBeInTheDocument();
    expect(queryByTestId('thread-row-archive')).toBeNull();
  });

  it('clicking Unarchive calls UnarchiveThread and patches the in-memory thread list', async () => {
    const thread = makeThread({ id: 'to-restore', title: 'Stale', archived: true, updatedAt: 10 });
    setBindingMock('ListThreads', async () => [thread]);
    await refreshThreads();

    const restoreMock = setBindingMock('UnarchiveThread', async () => ({
      ...thread,
      archived: false,
      updatedAt: 20,
    }));

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });
    await fireEvent.click(getByTestId('thread-row-unarchive'));
    // Drain microtasks so the store mutation in the click handler lands.
    for (let i = 0; i < 3; i += 1) await Promise.resolve();

    expect(restoreMock).toHaveBeenCalledWith('to-restore');
    const row = getThreads().find((t) => t.id === 'to-restore');
    expect(row?.archived).toBe(false);
    expect(row?.updatedAt).toBe(20);
  });

  it('unarchive failure surfaces the error through the pane state', async () => {
    const thread = makeThread({ id: 'fail-restore', archived: true });
    setBindingMock('UnarchiveThread', async () => {
      throw new Error('rpc down');
    });

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread, pane } });
    await fireEvent.click(getByTestId('thread-row-unarchive'));
    await Promise.resolve();
    await Promise.resolve();

    expect(pane.generalError ?? '').toMatch(/Rpc down\.|rpc down/i);
  });

  it('clicking the row with a modifier on an archived thread still invokes the select handler', async () => {
    const thread = makeThread({ id: 'archived-click', archived: true });
    let invokedWith: string | null = null;
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread,
        pane,
        onSelectClick: (modifier) => {
          invokedWith = modifier ?? 'single';
          return true;
        },
      },
    });
    await fireEvent.click(getByTestId('thread-row'), { metaKey: true });
    expect(invokedWith).toBe('toggle');
  });
});

describe('<ThreadRow> fork lineage affordance', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('is absent on a top-level (non-forked) thread', async () => {
    const thread = makeThread();
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, { props: { thread, pane } });
    expect(queryByTestId('thread-row-fork-lineage')).toBeNull();
  });

  it('is visible when forkedFromThreadId is set', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', title: 'Derived', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage');
    expect(forkIndicator).toBeInTheDocument();
    expect(forkIndicator.querySelector('svg')).not.toBeNull();
    expect(forkIndicator.textContent).not.toContain('F');
  });

  it('renders the fork indicator before the title like the left-side row icons', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', title: 'Derived', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage');
    const title = getByTestId('thread-row-title');

    expect(forkIndicator.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);
  });

  it('surfaces the parent title in the tooltip when the parent is loaded', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage') as HTMLButtonElement;
    expect(forkIndicator.title).toMatch(/"Original"/);
    expect(forkIndicator.disabled).toBe(false);
  });

  it('is disabled (with explanatory tooltip) when the parent is not in the sidebar view', async () => {
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent-not-loaded' });
    // Only the forked thread is in the list; the parent is absent.
    setBindingMock('ListThreads', async () => [forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const forkIndicator = getByTestId('thread-row-fork-lineage') as HTMLButtonElement;
    expect(forkIndicator.disabled).toBe(true);
    expect(forkIndicator.title).toMatch(/not loaded/i);
  });

  it('clicking the affordance switches the pane to the parent thread', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    setBindingMock('ListPayloadMetas', async () => []);

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    await fireEvent.click(getByTestId('thread-row-fork-lineage'));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(pane.threadId).toBe('parent');
  });

  it('affordance click does not trigger the row-level thread switch', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    setBindingMock('ListPayloadMetas', async () => []);

    let rowSelectCalled = 0;
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: forked,
        pane,
        onSelectClick: () => { rowSelectCalled += 1; return false; },
      },
    });
    await fireEvent.click(getByTestId('thread-row-fork-lineage'));
    for (let i = 0; i < 5; i += 1) await Promise.resolve();
    expect(rowSelectCalled).toBe(0);
    // Pane should be on the PARENT (not the forked thread we rendered).
    expect(pane.threadId).toBe('parent');
  });
});

describe('<ThreadRow> live status dot', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    resetThreadStatuses();
  });

  it('renders no dot at all when the thread is idle and read', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-idle' }), pane },
    });
    // Compact layout: idle + read = just the title and time, no dot.
    expect(queryByTestId('thread-row-status-dot')).toBeNull();
  });

  it('renders a success pulsing dot labelled Working when running in chat mode', () => {
    setThreadStatus('t-run', 'running');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-run', mode: 'chat' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
    expect(dot.getAttribute('title')).toBe('Working');
    expect(dot.classList.contains('bg-success')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(true);
  });

  // Regression: row must react LIVE to status changes pushed into the
  // projection store AFTER it mounted. This mirrors what happens in
  // production when a running row first starts streaming items while
  // the sidebar is already rendered. Before the fix, the pill never
  // appeared because the $derived wasn't recomputing on statuses-store
  // reassignment.
  it('reactively shows the pill when status flips AFTER mount', async () => {
    const pane = createThreadPane();
    const { queryByTestId, getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-post', mode: 'chat' }), pane },
    });
    expect(queryByTestId('thread-row-status-dot')).toBeNull();

    setThreadStatus('t-post', 'running');
    // Drain microtasks so the $derived recomputes and the DOM
    // reconciles.
    for (let i = 0; i < 3; i += 1) await Promise.resolve();

    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
  });

  // Full-stack regression: drive the exact wire-level path the live app
  // uses. provider:item_event upsert → applyItemStreamEvent (in events.ts) →
  // projectThreadItem → setThreadStatus → $derived → DOM. If this fails
  // but the direct-setThreadStatus test above passes, the break is in
  // the event plumbing, not the reactivity.
  it('flips the pill to Working when a streaming assistant_text arrives via provider:item_event', async () => {
    const { setupEventListeners } = await import('../../stores/events');
    const cleanup = setupEventListeners();
    try {
      const pane = createThreadPane();
      const { queryByTestId, getByTestId } = render(ThreadRow, {
        props: { thread: makeThread({ id: 't-stream', mode: 'chat' }), pane },
      });
      expect(queryByTestId('thread-row-status-dot')).toBeNull();

      emitItemEventUpsert({
        id: 'item-1',
        threadId: 't-stream',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'hello',
        createdAt: 1,
        updatedAt: 1,
      });
      await nextFrame();

      const dot = getByTestId('thread-row-status-dot');
      expect(dot.getAttribute('data-status')).toBe('running');
      expect(dot.getAttribute('aria-label')).toBe('Working');
    } finally {
      cleanup();
    }
  });

  // Regression: the mode-aware label must flip when the thread prop
  // updates. In production this comes from the sidebar's {#each} loop
  // re-rendering the row with a new thread object after replaceThread;
  // here we drive it via rerender({ thread: ... }).
  it('flips the pill label when thread.mode changes mid-turn', async () => {
    setThreadStatus('t-mode', 'running');
    const pane = createThreadPane();
    const chat = makeThread({ id: 't-mode', mode: 'chat' });
    const { getByTestId, rerender } = render(ThreadRow, {
      props: { thread: chat, pane },
    });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Working');

    await rerender({ thread: { ...chat, mode: 'plan' }, pane });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Planning');

    await rerender({ thread: { ...chat, mode: 'design' }, pane });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Designing');

    await rerender({ thread: { ...chat, mode: 'discussion' }, pane });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Discussing');
  });

  it('flips the pill to Working when a streaming thinking item arrives via provider:item_event', async () => {
    const { setupEventListeners } = await import('../../stores/events');
    const cleanup = setupEventListeners();
    try {
      const pane = createThreadPane();
      const { queryByTestId, getByTestId } = render(ThreadRow, {
        props: { thread: makeThread({ id: 't-think', mode: 'chat' }), pane },
      });
      expect(queryByTestId('thread-row-status-dot')).toBeNull();

      emitItemEventUpsert({
        id: 'thinking-1',
        threadId: 't-think',
        turnIndex: 0,
        itemIndex: 0,
        kind: 'thinking',
        role: 'assistant',
        status: 'streaming',
        summary: 'pondering',
        createdAt: 1,
        updatedAt: 1,
      });
      await nextFrame();

      const dot = getByTestId('thread-row-status-dot');
      expect(dot.getAttribute('data-status')).toBe('running');
    } finally {
      cleanup();
    }
  });

  it('labels the pill "Planning" when running in plan mode', () => {
    setThreadStatus('t-plan', 'running');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-plan', mode: 'plan' }), pane },
    });
    expect(getByTestId('thread-row-status-dot').getAttribute('aria-label')).toBe('Planning');
  });

  it('renders a warning dot labelled Pending approval when a blocking approval is pending', () => {
    setThreadStatus('t-approval', 'pending-approval');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-approval' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('pending-approval');
    expect(dot.getAttribute('aria-label')).toBe('Pending Approval');
    expect(dot.classList.contains('bg-warning')).toBe(true);
  });

  it('renders an info dot labelled Awaiting input for user-input requests', () => {
    setThreadStatus('t-input', 'awaiting-input');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-input' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('awaiting-input');
    expect(dot.getAttribute('aria-label')).toBe('Awaiting Input');
    expect(dot.classList.contains('bg-info')).toBe(true);
  });

  it('applies the pulsing warning glow class to the row when pending approval', () => {
    setThreadStatus('t-glow-approval', 'pending-approval');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-glow-approval' }), pane },
    });
    const row = getByTestId('thread-row');
    expect(row.classList.contains('status-glow-warning')).toBe(true);
    expect(row.classList.contains('status-glow-info')).toBe(false);
  });

  it('applies the pulsing info glow class to the row when awaiting input', () => {
    setThreadStatus('t-glow-input', 'awaiting-input');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-glow-input' }), pane },
    });
    const row = getByTestId('thread-row');
    expect(row.classList.contains('status-glow-info')).toBe(true);
    expect(row.classList.contains('status-glow-warning')).toBe(false);
  });

  it('does not apply a glow class when the row is merely running', () => {
    setThreadStatus('t-glow-run', 'running');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-glow-run' }), pane },
    });
    const row = getByTestId('thread-row');
    expect(row.classList.contains('status-glow-warning')).toBe(false);
    expect(row.classList.contains('status-glow-info')).toBe(false);
  });

  it('renders a non-pulsing accent dot labelled Plan ready when a plan is waiting', () => {
    setThreadStatus('t-plan-ready', 'plan-ready');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-plan-ready' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('plan-ready');
    expect(dot.getAttribute('aria-label')).toBe('Plan Ready');
    expect(dot.classList.contains('bg-accent')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(false);
  });

  it('renders durable Plan ready from the thread row without a live event', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ id: 't-durable-plan', hasActionableProposedPlan: true }),
        pane,
      },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('plan-ready');
    expect(dot.getAttribute('aria-label')).toBe('Plan Ready');
  });

  it('renders durable Interrupted from the thread row without a live event', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ id: 't-interrupted', hasIncompleteTurn: true }),
        pane,
      },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('interrupted');
    expect(dot.getAttribute('aria-label')).toBe('Interrupted');
    expect(dot.classList.contains('bg-warning')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(false);
  });

  it('does not render durable Interrupted while server live state is hydrating', () => {
    const token = beginThreadLiveStateHydration('t-hydrating');
    try {
      const pane = createThreadPane();
      const { queryByTestId } = render(ThreadRow, {
        props: {
          thread: makeThread({ id: 't-hydrating', hasIncompleteTurn: true }),
          pane,
        },
      });
      expect(queryByTestId('thread-row-status-dot')).toBeNull();
    } finally {
      finishThreadLiveStateHydration('t-hydrating', token);
    }
  });

  it('live running overrides durable Interrupted', () => {
    setThreadStatus('t-live-over-durable', 'running');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ id: 't-live-over-durable', hasIncompleteTurn: true }),
        pane,
      },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Working');
  });

  it('renders an error dot labelled Failed when the thread has errored', () => {
    setThreadStatus('t-err', 'error');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-err' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('error');
    expect(dot.getAttribute('aria-label')).toBe('Failed');
    expect(dot.classList.contains('bg-error')).toBe(true);
  });

  it('uses the thread id (not some shared row instance) for the lookup', () => {
    setThreadStatus('t-a', 'running');
    setThreadStatus('t-b', 'error');
    const pane = createThreadPane();
    // Scope each query to its own render's baseElement so duplicate
    // data-testid attributes in the shared document body don't collide.
    const rowA = render(ThreadRow, { props: { thread: makeThread({ id: 't-a' }), pane } });
    const dotA = rowA.container.querySelector<HTMLElement>('[data-testid="thread-row-status-dot"]');
    expect(dotA?.getAttribute('data-status')).toBe('running');
    rowA.unmount();

    const rowB = render(ThreadRow, { props: { thread: makeThread({ id: 't-b' }), pane } });
    const dotB = rowB.container.querySelector<HTMLElement>('[data-testid="thread-row-status-dot"]');
    expect(dotB?.getAttribute('data-status')).toBe('error');
    rowB.unmount();
  });

  it('renders the active thread-jump keybinding in the jump hint', async () => {
    vi.useFakeTimers();
    try {
      setKeybindingsForTest([{ key: 'ctrl+alt+2', command: 'thread.jump.1' }]);
      const release = subscribeJumpHints();
      const pane = createThreadPane();
      const { getByTestId } = render(ThreadRow, {
        props: { thread: makeThread({ id: 'jump-target' }), pane },
      });

      window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Control', bubbles: true }));
      vi.advanceTimersByTime(101);
      await tick();

      expect(getByTestId('thread-row-jump-hint').textContent?.trim()).toBe('Ctrl+Alt+2');
      release();
    } finally {
      resetKeyboardModifiersForTest();
      resetKeybindingsStore();
      vi.useRealTimers();
    }
  });
});

describe('<ThreadRow> nested row chrome', () => {
  beforeEach(async () => {
    resetPanesForTest();
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
  });

  it('renders no chevron when hasChildren is false', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread(), pane, hasChildren: false },
    });
    expect(queryByTestId('thread-row-expand')).toBeNull();
  });

  it('renders the chevron (not rotated) when hasChildren is true and expanded is false', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ discussionId: 'def-1' }),
        pane,
        hasChildren: true,
        expanded: false,
      },
    });
    const btn = getByTestId('thread-row-expand');
    expect(btn.getAttribute('aria-expanded')).toBe('false');
    expect(btn.querySelector('svg')?.classList.contains('rotate-90')).toBe(false);
  });

  it('rotates the chevron when expanded is true', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ discussionId: 'def-1' }),
        pane,
        hasChildren: true,
        expanded: true,
      },
    });
    const btn = getByTestId('thread-row-expand');
    expect(btn.getAttribute('aria-expanded')).toBe('true');
    expect(btn.querySelector('svg')?.classList.contains('rotate-90')).toBe(true);
  });

  it('chevron click calls onToggleExpand and not the row-click path', async () => {
    const onToggleExpand = vi.fn();
    const onSelectClick = vi.fn(() => false);
    const pane = createThreadPane();
    setBindingMock('SwitchThread', async () => {});
    setBindingMock('ListItems', async () => []);
    setBindingMock('ListPayloadMetas', async () => []);

    const { getByTestId } = render(ThreadRow, {
      props: {
        thread: makeThread({ discussionId: 'def-1' }),
        pane,
        hasChildren: true,
        expanded: false,
        onToggleExpand,
        onSelectClick,
      },
    });
    await fireEvent.click(getByTestId('thread-row-expand'));
    expect(onToggleExpand).toHaveBeenCalledTimes(1);
    expect(onSelectClick).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
  });

  it('applies indent via padding-left on the outer container', () => {
    const pane = createThreadPane();
    const { container } = render(ThreadRow, {
      props: { thread: makeThread(), pane, indent: 2 },
    });
    const outer = container.querySelector('[role="button"]') as HTMLElement;
    // Compact layout: depth 1 sits flush against the rail; depth 2+
    // steps 8px per level. indent=2 → 8px.
    expect(outer.style.paddingLeft).toBe('8px');
  });
});
