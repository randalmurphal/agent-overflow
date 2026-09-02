import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import ProjectThreadList from '../ProjectThreadList.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../../stores/panes.svelte';
import type { Thread } from '../../../types/models';
import { resetSidebarForTest } from '../../../stores/sidebar.svelte';
import { replaceAllThreads, touchThreadActivity } from '../../../stores/threads.svelte';
import { tick } from 'svelte';
import { resetForTest as resetThreadStatuses } from '../../../stores/threadStatuses.svelte';

function mkThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: `Thread ${id}`,
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

describe('<ProjectThreadList>', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetSidebarForTest();
    resetThreadStatuses();
  });

  it('shows a "+ New Thread" empty-state button when threads is empty', () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectThreadList, {
      props: { projectId: 'p1', threads: [], pane },
    });
    expect(getByTestId('project-thread-list-empty')).toHaveTextContent(
      /New thread/i,
    );
  });

  it('empty-state button calls onNewThread with the projectId', async () => {
    const onNewThread = vi.fn();
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'proj-42',
        threads: [],
        pane,
        onNewThread,
      },
    });
    const btn = getByTestId('project-thread-list-empty');
    await fireEvent.click(btn);
    expect(onNewThread).toHaveBeenCalledWith('proj-42', { openInNewPane: false });
  });

  it('ctrl-clicking the empty-state button requests a new pane', async () => {
    const onNewThread = vi.fn();
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'proj-42',
        threads: [],
        pane,
        onNewThread,
      },
    });

    await fireEvent.click(getByTestId('project-thread-list-empty'), { ctrlKey: true });

    expect(onNewThread).toHaveBeenCalledWith('proj-42', { openInNewPane: true });
  });

  it('cmd-clicking the empty-state button requests a new pane', async () => {
    const onNewThread = vi.fn();
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'proj-42',
        threads: [],
        pane,
        onNewThread,
      },
    });

    await fireEvent.click(getByTestId('project-thread-list-empty'), { metaKey: true });

    expect(onNewThread).toHaveBeenCalledWith('proj-42', { openInNewPane: true });
  });

  it('renders thread rows when threads are present', () => {
    const pane = createThreadPane();
    const { getByText, queryByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'p1',
        threads: [mkThread('t1', { title: 'Alpha' }), mkThread('t2', { title: 'Beta' })],
        pane,
      },
    });
    expect(getByText('Alpha')).toBeInTheDocument();
    expect(getByText('Beta')).toBeInTheDocument();
    expect(queryByTestId('project-thread-list-empty')).toBeNull();
  });

  it('renders one thin divider only when both pin blocks are present', async () => {
    const pane = createThreadPane();
    const { getByTestId, rerender } = render(ProjectThreadList, {
      props: {
        projectId: 'p1',
        threads: [
          mkThread('front', { pinnedAt: 1, pinGroup: 0 }),
          mkThread('back', { pinnedAt: 2, pinGroup: 1 }),
        ],
        pane,
      },
    });
    const list = getByTestId('project-thread-list');
    expect(list.querySelectorAll('[data-testid="thread-pin-group-divider"]')).toHaveLength(1);
    expect(list.querySelector('[data-testid="thread-pin-group-divider"]')?.className).toContain(
      'border-border-subtle',
    );

    await rerender({
      projectId: 'p1',
      threads: [
        mkThread('front-a', { pinnedAt: 1, pinGroup: 0 }),
        mkThread('front-b', { pinnedAt: 2, pinGroup: 0 }),
      ],
      pane,
    });
    expect(list.querySelectorAll('[data-testid="thread-pin-group-divider"]')).toHaveLength(0);
  });

  it('renders a child durable Interrupted status on the collapsed parent row', () => {
    const pane = createThreadPane();
    const parent = mkThread('parent', { title: 'Parent' });
    const child = mkThread('child', {
      parentThreadId: 'parent',
      title: 'Child',
      hasIncompleteTurn: true,
    });
    const { getByTestId, queryByText } = render(ProjectThreadList, {
      props: {
        projectId: 'p1',
        threads: [parent, child],
        pane,
      },
    });

    expect(queryByText('Child')).toBeNull();
    const dot = getByTestId('thread-row-status-dot');
    expect(dot.getAttribute('data-status')).toBe('interrupted');
    expect(dot.getAttribute('aria-label')).toBe('Interrupted');
  });

  it('shows 6 threads before the show-more row, then reveals 20 more per click', async () => {
    const pane = createThreadPane();
    const threads = Array.from({ length: 31 }, (_, i) => mkThread(`t${i}`, {
      title: `Thread ${i}`,
      updatedAt: 100 - i,
    }));
    const { getByTestId, queryByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'p1',
        threads,
        pane,
      },
    });

    const list = getByTestId('project-thread-list');
    expect(list.querySelectorAll('[role="listitem"]')).toHaveLength(6);
    const firstShowMore = getByTestId('project-thread-list-show-more');
    expect(firstShowMore).toHaveTextContent('Show 20 More (25)');
    expect(firstShowMore.className).toContain('pl-6');

    await fireEvent.click(firstShowMore);
    expect(list.querySelectorAll('[role="listitem"]')).toHaveLength(26);
    const secondShowMore = getByTestId('project-thread-list-show-more');
    expect(secondShowMore).toHaveTextContent('Show 5 More');

    await fireEvent.click(secondShowMore);
    expect(list.querySelectorAll('[role="listitem"]')).toHaveLength(31);
    expect(queryByTestId('project-thread-list-show-more')).toBeNull();
  });

  it('reveals 20 hidden threads when the active thread is already floated into view', async () => {
    const pane = createThreadPane();
    registerPaneForTest('main', pane);
    const threads = Array.from({ length: 31 }, (_, i) => mkThread(`t${i}`, {
      title: `Thread ${i}`,
      updatedAt: 100 - i,
    }));
    pane.replaceThread(threads[10]);

    const { getByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'p1',
        threads,
        pane,
      },
    });

    const list = getByTestId('project-thread-list');
    expect(list.querySelectorAll('[role="listitem"]')).toHaveLength(7);
    const firstShowMore = getByTestId('project-thread-list-show-more');
    expect(firstShowMore).toHaveTextContent('Show 20 More (24)');

    await fireEvent.click(firstShowMore);
    expect(list.querySelectorAll('[role="listitem"]')).toHaveLength(27);
    expect(getByTestId('project-thread-list-show-more')).toHaveTextContent('Show 4 More');
  });

  it('floats a thread open in a NON-focused pane above the cut, marked open but not focused', async () => {
    // The sidebar is handed the focused pane only; a thread mounted in any
    // other pane must still escape "Show N More", and its row must carry
    // the open marker without the focused fill.
    const focused = createThreadPane();
    const other = createThreadPane();
    registerPaneForTest('main', focused);
    registerPaneForTest('right', other);
    const threads = Array.from({ length: 31 }, (_, i) => mkThread(`t${i}`, {
      title: `Thread ${i}`,
      updatedAt: 100 - i,
    }));
    focused.replaceThread(threads[0]);
    other.replaceThread(threads[20]);

    const { getByTestId } = render(ProjectThreadList, {
      props: { projectId: 'p1', threads, pane: focused },
    });

    const list = getByTestId('project-thread-list');
    const rows = Array.from(list.querySelectorAll<HTMLElement>('[data-sidebar-thread-id]'));
    expect(rows.map((row) => row.dataset.sidebarThreadId)).toEqual([
      't0', 't1', 't2', 't3', 't4', 't5', 't20',
    ]);
    expect(getByTestId('project-thread-list-show-more')).toHaveTextContent('Show 20 More (24)');

    const shells = Array.from(list.querySelectorAll<HTMLElement>('[data-testid="thread-row-shell"]'));
    const shellFor = (id: string) => shells.find(
      (shell) => shell.querySelector<HTMLElement>('[data-sidebar-thread-id]')?.dataset.sidebarThreadId === id,
    )!;
    expect(shellFor('t0').dataset.open).toBe('true');
    expect(shellFor('t0').dataset.focused).toBe('true');
    expect(shellFor('t20').dataset.open).toBe('true');
    expect(shellFor('t20').dataset.focused).toBeUndefined();
    expect(shellFor('t1').dataset.open).toBeUndefined();
    expect(shellFor('t1').dataset.focused).toBeUndefined();
  });

  it('a live-activity beat does not reconcile the FLIP each-block; a real reorder does', async () => {
    // Tripwire for the streaming machine-gun stutter class (2026-08-26):
    // per-flush activity bumps used to rewrite the threads array, and the
    // animated each-block then ran svelte's FLIP measure pass — a forced
    // synchronous layout (getBoundingClientRect per visible row) on every
    // streamed item. A beat that changes no ordering must stay identity-
    // stable all the way to the each; a genuine overtake must still
    // reconcile so FLIP animates it.
    const t1 = mkThread('t1', { updatedAt: 1000 });
    const t2 = mkThread('t2', { updatedAt: 2000 });
    replaceAllThreads([t1, t2]);
    const pane = createThreadPane();
    render(ProjectThreadList, {
      props: { projectId: 'p1', threads: [t1, t2], pane },
    });
    await tick();
    const spy = vi.spyOn(Element.prototype, 'getBoundingClientRect');

    // Same-order beat (what every streaming flush does).
    touchThreadActivity('t1', 1500);
    await tick();
    expect(spy).not.toHaveBeenCalled();

    // Real overtake: t1 passes t2, the each reconciles, FLIP measures.
    touchThreadActivity('t1', 3000);
    await tick();
    expect(spy).toHaveBeenCalled();
    spy.mockRestore();
  });
});
