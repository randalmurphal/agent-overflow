import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import ProjectThreadList from '../ProjectThreadList.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import { resetSidebarForTest } from '../../../stores/sidebar.svelte';
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
    let capturedId: string | null = null;
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectThreadList, {
      props: {
        projectId: 'proj-42',
        threads: [],
        pane,
        onNewThread: (id: string) => {
          capturedId = id;
        },
      },
    });
    const btn = getByTestId('project-thread-list-empty');
    btn.click();
    expect(capturedId).toBe('proj-42');
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
});
