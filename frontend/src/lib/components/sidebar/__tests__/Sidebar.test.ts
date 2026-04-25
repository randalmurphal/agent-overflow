import { beforeEach, describe, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import Sidebar from '../Sidebar.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  isProjectExpanded,
  resetSidebarForTest,
} from '../../../stores/sidebar.svelte';
import {
  resetForTest as resetThreadStatuses,
  setThreadStatus,
} from '../../../stores/threadStatuses.svelte';
import {
  refreshThreads,
} from '../../../stores/threads.svelte';
import {
  resetProjectsForTest,
} from '../../../stores/projects.svelte';
import type { Item, Project, ProjectWithCounts, Thread } from '../../../types/models';
import {
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function project(id: string, overrides: Partial<Project> = {}): ProjectWithCounts {
  return {
    project: {
      id,
      path: `/tmp/${id}`,
      name: overrides.name ?? id,
      sortPosition: 0,
      createdAt: 0,
      updatedAt: 0,
      archived: false,
      ...overrides,
    },
    threadCount: 1,
    lastActive: 0,
  };
}

function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: `Thread ${id}`,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    projectId: 'p1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('<Sidebar>', () => {
  beforeEach(() => {
    resetSidebarForTest();
    resetThreadStatuses();
    resetProjectsForTest();
    resetBindingMocks();
    setBindingMock('ListRecentThreadItems', async () => ({
      items: [] as Item[],
      oldestTurnIndex: -1,
      hasMore: false,
    }));
    setBindingMock('ListRecentTurns', async () => []);
  });

  it('does not auto-expand the collapsed project containing the active running thread', async () => {
    const runningThread = thread('t-running', { title: 'Active work' });
    const projectRow = project('p1', { name: 'Project One' });
    setBindingMock('ListThreads', async () => [runningThread]);
    setBindingMock('ListProjects', async () => [projectRow]);
    setBindingMock('SwitchThread', async () => runningThread);

    await refreshThreads();
    const pane = createThreadPane();
    await pane.switchThread(runningThread);
    setThreadStatus(runningThread.id, 'running');

    const { getByTestId, queryByTestId, getByText } = render(Sidebar, {
      props: { pane },
    });

    await waitFor(() => {
      expect(getByText('Project One')).toBeInTheDocument();
      expect(getByTestId('project-item-status-dot').getAttribute('data-status')).toBe('running');
    });

    expect(isProjectExpanded('p1')).toBe(false);
    expect(getByTestId('project-item-chevron').getAttribute('aria-expanded')).toBe('false');
    expect(getByTestId('project-item-active-pin')).toBeInTheDocument();
    expect(getByText('Active work')).toBeInTheDocument();
    expect(queryByTestId('project-thread-list')).toBeNull();
  });
});
