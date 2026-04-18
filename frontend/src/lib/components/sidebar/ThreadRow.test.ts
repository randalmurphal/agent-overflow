import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ThreadRow from './ThreadRow.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { refreshThreads, getThreads } from '../../stores/threads.svelte';
import { resetForTest as resetThreadStatuses, setThreadStatus } from '../../stores/threadStatuses.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test Thread',
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    interactionMode: 'default',
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

describe('<ThreadRow> unarchive', () => {
  beforeEach(async () => {
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
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

    expect(pane.error ?? '').toMatch(/Failed to unarchive thread/);
  });

  it('clicking the row with a modifier on an archived thread still invokes the select handler', async () => {
    const thread = makeThread({ id: 'archived-click', archived: true });
    let invokedWith: string | null = null;
    const pane = createThreadPane();
    const { getByRole } = render(ThreadRow, {
      props: {
        thread,
        pane,
        onSelectClick: (modifier) => {
          invokedWith = modifier ?? 'single';
          return true;
        },
      },
    });
    await fireEvent.click(getByRole('button', { pressed: false }), { metaKey: true });
    expect(invokedWith).toBe('toggle');
  });
});

describe('<ThreadRow> fork lineage badge', () => {
  beforeEach(async () => {
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
    expect(getByTestId('thread-row-fork-lineage')).toBeInTheDocument();
  });

  it('surfaces the parent title in the tooltip when the parent is loaded', async () => {
    const parent = makeThread({ id: 'parent', title: 'Original' });
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent' });
    setBindingMock('ListThreads', async () => [parent, forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const badge = getByTestId('thread-row-fork-lineage') as HTMLButtonElement;
    expect(badge.title).toMatch(/"Original"/);
    expect(badge.disabled).toBe(false);
  });

  it('is disabled (with explanatory tooltip) when the parent is not in the sidebar view', async () => {
    const forked = makeThread({ id: 'fork', forkedFromThreadId: 'parent-not-loaded' });
    // Only the forked thread is in the list; the parent is absent.
    setBindingMock('ListThreads', async () => [forked]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, { props: { thread: forked, pane } });
    const badge = getByTestId('thread-row-fork-lineage') as HTMLButtonElement;
    expect(badge.disabled).toBe(true);
    expect(badge.title).toMatch(/not loaded/i);
  });

  it('clicking the badge switches the pane to the parent thread', async () => {
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

  it('badge click does not trigger the row-level thread switch', async () => {
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
    await primeSettings();
    setBindingMock('ListThreads', async () => []);
    await refreshThreads();
    resetThreadStatuses();
  });

  it('renders a placeholder (no dot) when the thread is idle', () => {
    const pane = createThreadPane();
    const { queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-idle' }), pane },
    });
    expect(queryByTestId('thread-row-status-dot')).toBeNull();
    expect(queryByTestId('thread-row-status-placeholder')).toBeInTheDocument();
  });

  it('renders an amber pulsing dot when the thread is running', () => {
    setThreadStatus('t-run', 'running');
    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-run' }), pane },
    });
    expect(queryByTestId('thread-row-status-placeholder')).toBeNull();
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('running');
    expect(dot.getAttribute('aria-label')).toBe('Running');
    expect(dot.getAttribute('title')).toBe('Running');
    expect(dot.classList.contains('bg-warning')).toBe(true);
    expect(dot.classList.contains('animate-pulse')).toBe(true);
  });

  it('renders an accent dot when an approval is pending', () => {
    setThreadStatus('t-approval', 'pending-approval');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-approval' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('pending-approval');
    expect(dot.getAttribute('aria-label')).toBe('Pending approval');
    expect(dot.classList.contains('bg-accent')).toBe(true);
  });

  it('renders an error dot when the thread has errored', () => {
    setThreadStatus('t-err', 'error');
    const pane = createThreadPane();
    const { getByTestId } = render(ThreadRow, {
      props: { thread: makeThread({ id: 't-err' }), pane },
    });
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('error');
    expect(dot.getAttribute('aria-label')).toBe('Error');
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
});

describe('<ThreadRow> nested row chrome', () => {
  beforeEach(async () => {
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
      props: { thread: makeThread(), pane, indent: 1 },
    });
    const outer = container.querySelector('[role="button"]') as HTMLElement;
    expect(outer.style.paddingLeft).toMatch(/calc\(0\.75rem \+ 0\.9rem\)/);
  });
});
