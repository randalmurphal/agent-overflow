import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ProjectThreadList from '../ProjectThreadList.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';

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
});
