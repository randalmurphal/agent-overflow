import { beforeEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import Sidebar from '../Sidebar.svelte';
import { resetAppStorageForTest } from '../../../stores/appStorage';
import {
  isSidebarCollapsed,
  resetSidebarLayoutForTest,
  setSidebarCollapsed,
  setSidebarWidth,
} from '../../../stores/sidebarLayout.svelte';
import {
  formatChord,
  resetKeybindingsStore,
  setKeybindingsForTest,
} from '../../../stores/keybindings.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  collapseProject,
  isProjectExpanded,
  resetSidebarForTest,
} from '../../../stores/sidebar.svelte';
import {
  projectTurnStarted,
  resetForTest as resetThreadStatuses,
} from '../../../stores/threadStatuses.svelte';
import {
  refreshThreads,
} from '../../../stores/threads.svelte';
import {
  resetProjectsForTest,
} from '../../../stores/projects.svelte';
import type { Project, ProjectWithCounts, Thread } from '../../../types/models';
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

function markThreadRunning(threadId: string): void {
  projectTurnStarted(threadId, `turn:${threadId}`, 0, 0);
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
    setBindingMock('ListRecentTurns', async () => []);
  });

  it('does not auto-expand the collapsed project containing the active running thread', async () => {
    // Projects default to expanded — we explicitly collapse so the test
    // exercises the collapsed-render path (status dot + active pin
    // surfacing the running work without requiring the user to expand).
    collapseProject('p1');
    const runningThread = thread('t-running', { title: 'Active work' });
    const projectRow = project('p1', { name: 'Project One' });
    setBindingMock('ListThreads', async () => [runningThread]);
    setBindingMock('ListProjects', async () => [projectRow]);
    setBindingMock('SwitchThread', async () => runningThread);

    await refreshThreads();
    const pane = createThreadPane();
    await pane.switchThread(runningThread);
    markThreadRunning(runningThread.id);

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

// --- collapse / expand affordance (t3 7.2) ---
//
// Collapsed renders a RAIL, not nothing: the expand control has to
// exist in every app state, including the ones where no chat header is
// mounted. The branch lives in Sidebar's template so the component's
// project fetch and palette registrations survive the transition.

describe('<Sidebar> collapse toggle', () => {
  beforeEach(() => {
    resetSidebarForTest();
    resetProjectsForTest();
    resetBindingMocks();
    resetAppStorageForTest();
    resetSidebarLayoutForTest();
    resetKeybindingsStore();
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  it('renders a collapse button in the expanded sidebar chrome', () => {
    const { getByTestId } = render(Sidebar, { props: { pane: null } });
    expect(getByTestId('sidebar')).toBeInTheDocument();
    const toggle = getByTestId('sidebar-collapse-toggle');
    expect(toggle.getAttribute('aria-label')).toContain('Collapse Sidebar');
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
  });

  it('clicking it collapses to a rail that still offers the expand control', async () => {
    const { getByTestId, queryByTestId } = render(Sidebar, { props: { pane: null } });

    await fireEvent.click(getByTestId('sidebar-collapse-toggle'));

    expect(isSidebarCollapsed()).toBe(true);
    expect(queryByTestId('sidebar')).toBeNull();
    expect(queryByTestId('sidebar-thread-search')).toBeNull();
    expect(queryByTestId('sidebar-resizer')).toBeNull();
    const rail = getByTestId('sidebar-rail');
    expect(rail).toBeInTheDocument();
    const toggle = getByTestId('sidebar-collapse-toggle');
    expect(toggle.getAttribute('aria-label')).toContain('Expand Sidebar');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('expanding from the rail restores the stored width and the resizer', async () => {
    setSidebarWidth(330);
    const { getByTestId } = render(Sidebar, { props: { pane: null } });

    await fireEvent.click(getByTestId('sidebar-collapse-toggle'));
    await fireEvent.click(getByTestId('sidebar-collapse-toggle'));

    expect(isSidebarCollapsed()).toBe(false);
    expect(getByTestId('sidebar').getAttribute('style')).toContain('width: 330px');
    expect(getByTestId('sidebar-resizer')).toBeInTheDocument();
  });

  it('shows the live chord for sidebar.toggle in the control label', () => {
    setKeybindingsForTest([{ key: 'mod+alt+s', command: 'sidebar.toggle' }]);
    const { getByTestId } = render(Sidebar, { props: { pane: null } });
    expect(getByTestId('sidebar-collapse-toggle').getAttribute('aria-label'))
      .toBe(`Collapse Sidebar (${formatChord('mod+alt+s')})`);
  });

  it('re-registers a live search focuser after an expand', async () => {
    const focusers: Array<() => void> = [];
    const { getByTestId } = render(Sidebar, {
      props: { pane: null, registerFocusSearch: (focus: () => void) => { focusers.push(focus); } },
    });
    await tick();

    setSidebarCollapsed(true);
    await tick();
    setSidebarCollapsed(false);
    await tick();

    // This is the ordering `withSidebarVisible` relies on: by the time a
    // tick has passed, the focuser points at the input that is on screen.
    expect(focusers.length).toBeGreaterThan(1); // the remount re-registered
    focusers.at(-1)?.();
    expect(document.activeElement).toBe(getByTestId('sidebar-thread-search'));
  });
});
