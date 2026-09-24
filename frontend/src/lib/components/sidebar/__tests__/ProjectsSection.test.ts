import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

import ProjectsSection from '../ProjectsSection.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  getProjectSortMode,
  resetSidebarForTest,
  setProjectSortMode,
} from '../../../stores/sidebar.svelte';
import {
  refreshProjects,
  resetProjectsForTest,
  touchProjectActivity,
} from '../../../stores/projects.svelte';
import { refreshThreads } from '../../../stores/threads.svelte';
import {
  ensureMainPane,
  getAllPanes,
  getFocusedPaneId,
  resetPanesForTest,
  syncThread,
} from '../../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../../stores/paneLayout.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import type { Project, ProjectWithCounts } from '../../../types/models';
import { setThreadFilterQuery } from '../../../stores/threadFilter.svelte';
import {
  consumePendingGroupRename,
  resetThreadGroupsForTest,
} from '../../../stores/threadGroups.svelte';
import { createThreadGroupAction } from '../threadGroupActions';
import { resetCatalogLoadForTest } from '../../../stores/catalogLoad.svelte';
import { __setTransportStatusForTest } from '../../../stores/transportStatus.svelte';
import { TransportError } from '../../../transport/wsClient';

function mkProject(id: string, overrides: Partial<Project> = {}): Project {
  return {
    id,
    path: `/tmp/${id}`,
    name: id,
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function seedProjects(items: ProjectWithCounts[]): Promise<void> {
  setBindingMock('ListProjects', async () => items);
  await refreshProjects();
}

describe('<ProjectsSection>', () => {
  beforeEach(async () => {
    resetSidebarForTest();
    resetProjectsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetThreadGroupsForTest();
    setThreadFilterQuery('');
    setBindingMock('ListProjects', async () => []);
    setBindingMock('ListThreads', async () => []);
    setBindingMock('GetThreadDefaults', async () => ({
      provider: 'claude',
      model: 'claude-sonnet-4-6',
      reasoningEffort: '',
      fastMode: false,
      contextWindow: 0,
      runtimeMode: '',
      branch: 'main',
      workspacePath: '/tmp/ws',
    }));
    // Reset the threads store so threads seeded by one test don't leak
    // into the next. Both reads also settle HOME's catalogs as loaded, so
    // the list renders instead of its loading row.
    await refreshThreads();
    await refreshProjects();
  });

  it('renders the PROJECTS header and control icons', async () => {
    const pane = createThreadPane();
    const { getByText, getByTestId, getByRole } = render(ProjectsSection, {
      props: { pane },
    });
    expect(getByText('Projects')).toBeInTheDocument();
    // IconButton renders a button with aria-label; both buttons carry
    // dedicated icon data-testids.
    expect(getByTestId('sidebar-add-project-icon')).toBeInTheDocument();
    expect(getByTestId('sidebar-sort-icon')).toBeInTheDocument();
    expect(getByRole('button', { name: 'Add Project' })).toBeInTheDocument();
  });

  it('clicking + opens the Add Project modal', async () => {
    setBindingMock('BrowseDirectory', async () => ({
      path: '/Users/me',
      parent: '/Users',
      separator: '/',
      entries: [],
      truncated: false,
    }));
    const pane = createThreadPane();
    const { getByRole, queryByRole } = render(ProjectsSection, {
      props: { pane },
    });
    // Modal is mounted but closed -> role="dialog" is absent until opened.
    expect(queryByRole('dialog')).toBeNull();
    await fireEvent.click(getByRole('button', { name: 'Add Project' }));
    await tick();
    await Promise.resolve();
    expect(getByRole('dialog')).toBeInTheDocument();
    // Heading inside the modal matches the configured title.
    const heading = document.querySelector('[role="dialog"] h2');
    expect(heading?.textContent).toBe('Add Project');
  });

  it('shows a minimal hint when there are no projects', async () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectsSection, { props: { pane } });
    await tick();
    const hint = getByTestId('sidebar-projects-empty');
    expect(hint.textContent).toMatch(/No projects yet\..*Click \+ to add one\./);
  });

  describe('before the catalogs load', () => {
    const report = {
      phase: 'store.migrate',
      detail: 'Applying migration 3 of 7 add_index',
      step: 3,
      steps: 7,
      elapsedMs: 72_000,
      updatingTo: '',
    };

    beforeEach(() => {
      // Back to a computer that has not answered yet.
      resetCatalogLoadForTest();
    });

    it('shows a loading row and never the empty hint until both catalogs load', async () => {
      const { getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane: null } });
      await tick();
      expect(getByTestId('sidebar-catalog-loading-label')).toHaveTextContent('Loading projects…');
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      await refreshProjects();
      await tick();
      // Threads have not answered: still incomplete, still not empty.
      expect(getByTestId('sidebar-catalog-loading')).toBeInTheDocument();
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      await refreshThreads();
      await tick();
      expect(queryByTestId('sidebar-catalog-status')).toBeNull();
      expect(getByTestId('sidebar-projects-empty')).toBeInTheDocument();
    });

    it('keeps the rows it has visible beside the loading row', async () => {
      await seedProjects([
        { project: mkProject('p1', { name: 'Project One' }), threadCount: 0, lastActive: 0 },
      ]);
      resetCatalogLoadForTest();
      const { getByTestId, getAllByTestId } = render(ProjectsSection, { props: { pane: null } });
      await tick();
      expect(getByTestId('sidebar-catalog-loading')).toBeInTheDocument();
      expect(getAllByTestId('project-item').map((el) => el.getAttribute('data-project-id'))).toEqual(['p1']);
    });

    it('names the boot phase while the computer is starting', async () => {
      __setTransportStatusForTest({ status: 'starting', nextAttemptAt: null, startup: report });
      const { getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane: null } });
      await tick();
      expect(getByTestId('sidebar-catalog-loading')).toHaveAttribute('data-status', 'starting');
      expect(getByTestId('sidebar-catalog-loading-label')).toHaveTextContent('Applying migration 3 of 7 add_index');
      expect(getByTestId('sidebar-catalog-loading-meta')).toHaveTextContent('Step 3 of 7 · 1:12 elapsed');
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      __setTransportStatusForTest({
        status: 'starting', nextAttemptAt: null, startup: { ...report, updatingTo: '1.4.0' },
      });
      await tick();
      expect(getByTestId('sidebar-catalog-loading-label'))
        .toHaveTextContent('Finishing update to v1.4.0: applying migration 3 of 7 add_index');
    });

    it('shows a failed load with its error, and Retry loads it', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      setBindingMock('ListProjects', async () => {
        throw new TransportError('method_error', 'list projects: database disk image is malformed');
      });
      await refreshProjects();
      await refreshThreads();
      const { getByTestId, queryByTestId, findByTestId } = render(ProjectsSection, { props: { pane: null } });
      await tick();
      expect(getByTestId('sidebar-catalog-failed')).toHaveTextContent(/database disk image is malformed/i);
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      setBindingMock('ListProjects', async () => [
        { project: mkProject('p1', { name: 'Project One' }), threadCount: 0, lastActive: 0 },
      ]);
      await fireEvent.click(getByTestId('sidebar-catalog-retry'));
      expect(await findByTestId('project-item')).toHaveAttribute('data-project-id', 'p1');
      expect(queryByTestId('sidebar-catalog-status')).toBeNull();
    });
  });

  it('defaults to lastActivity sort mode', async () => {
    expect(getProjectSortMode()).toBe('lastActivity');
    const pane = createThreadPane();
    render(ProjectsSection, { props: { pane } });
  });

  it('sorts projects by lastActivity desc by default', async () => {
    await seedProjects([
      { project: mkProject('p-stale', { name: 'Stale' }), threadCount: 1, lastActive: 100 },
      { project: mkProject('p-fresh', { name: 'Fresh' }), threadCount: 1, lastActive: 9000 },
    ]);
    const pane = createThreadPane();
    const { container } = render(ProjectsSection, { props: { pane } });
    await tick();
    const ids = Array.from(
      container.querySelectorAll('[data-testid="project-item"]'),
    ).map((el) => el.getAttribute('data-project-id'));
    expect(ids).toEqual(['p-fresh', 'p-stale']);
  });

  it('re-sorts when a project receives newer live activity', async () => {
    await seedProjects([
      { project: mkProject('p-stale', { name: 'Stale' }), threadCount: 1, lastActive: 100 },
      { project: mkProject('p-fresh', { name: 'Fresh' }), threadCount: 1, lastActive: 9000 },
    ]);
    const pane = createThreadPane();
    const { container } = render(ProjectsSection, { props: { pane } });
    await tick();

    touchProjectActivity('p-stale', 10_000);
    await tick();

    const ids = Array.from(
      container.querySelectorAll('[data-testid="project-item"]'),
    ).map((el) => el.getAttribute('data-project-id'));
    expect(ids).toEqual(['p-stale', 'p-fresh']);
  });

  it('does not re-sort when syncThread carries a setting/config update', async () => {
    // syncThread carries the result of in-place setters (model swap,
    // worktree path change, branch checkout, rename, etc.) which do
    // NOT count as activity — backend MarkThreadActivity is the only
    // legitimate sort-bump path. The frontend mirrors that contract:
    // syncThread no longer touches project activity, so projects keep
    // their existing order across setting changes on any of their
    // threads.
    await seedProjects([
      { project: mkProject('p-stale', { name: 'Stale' }), threadCount: 1, lastActive: 100 },
      { project: mkProject('p-fresh', { name: 'Fresh' }), threadCount: 1, lastActive: 9000 },
    ]);
    const pane = createThreadPane();
    const { container } = render(ProjectsSection, { props: { pane } });
    await tick();

    syncThread({
      id: 'thread-stale',
      title: 'Stale thread',
      provider: 'claude',
      workspacePath: '/tmp/stale',
      projectPath: '/tmp/stale',
      projectId: 'p-stale',
      mode: 'chat',
      model: 'claude-sonnet-4-6',
      createdAt: 0,
      updatedAt: 10_000,
      archived: false,
    });
    await tick();

    const ids = Array.from(
      container.querySelectorAll('[data-testid="project-item"]'),
    ).map((el) => el.getAttribute('data-project-id'));
    expect(ids).toEqual(['p-fresh', 'p-stale']);
  });

  it('switches to createdAt sort and re-orders projects', async () => {
    await seedProjects([
      { project: mkProject('p-old', { name: 'Old', createdAt: 100 }), threadCount: 0, lastActive: 0 },
      { project: mkProject('p-new', { name: 'New', createdAt: 9000 }), threadCount: 0, lastActive: 0 },
    ]);
    setProjectSortMode('createdAt');
    const pane = createThreadPane();
    const { container } = render(ProjectsSection, { props: { pane } });
    await tick();
    const ids = Array.from(
      container.querySelectorAll('[data-testid="project-item"]'),
    ).map((el) => el.getAttribute('data-project-id'));
    expect(ids).toEqual(['p-new', 'p-old']);
  });

  it('honors manual sort by sortPosition asc when mode is manual', async () => {
    await seedProjects([
      { project: mkProject('p-a', { name: 'A', sortPosition: 2 }), threadCount: 0, lastActive: 0 },
      { project: mkProject('p-b', { name: 'B', sortPosition: 0 }), threadCount: 0, lastActive: 0 },
      { project: mkProject('p-c', { name: 'C', sortPosition: 1 }), threadCount: 0, lastActive: 0 },
    ]);
    setProjectSortMode('manual');
    const pane = createThreadPane();
    const { container } = render(ProjectsSection, { props: { pane } });
    await tick();
    const ids = Array.from(
      container.querySelectorAll('[data-testid="project-item"]'),
    ).map((el) => el.getAttribute('data-project-id'));
    expect(ids).toEqual(['p-b', 'p-c', 'p-a']);
  });

  it('opens the inline rename on the row a group create just mounted', async () => {
    // Why the request lives INSIDE createThreadGroupAction: the store write
    // schedules the flush that mounts the row, and that flush runs before the
    // action's caller resumes — so the row has already asked and been answered
    // by the time the two assertions below run. A caller that requested the
    // rename after awaiting always asked too late.
    await seedProjects([
      { project: mkProject('p1', { name: 'Project One' }), threadCount: 0, lastActive: 0 },
    ]);
    setBindingMock('CreateThreadGroup', async (projectId: string, name: string) => ({
      id: 'g-new',
      projectId,
      name,
      createdAt: 0,
      updatedAt: 0,
    }));
    const { getByLabelText } = render(ProjectsSection, { props: { pane: null } });
    await tick();

    await createThreadGroupAction('p1');

    expect(getByLabelText('Rename Group')).toBeInTheDocument();
    expect(consumePendingGroupRename('g-new')).toBe(false);
  });

  it('ctrl-clicking a project new-thread button opens the draft in a new pane', async () => {
    await seedProjects([
      { project: mkProject('p1', { name: 'Project One' }), threadCount: 0, lastActive: 0 },
    ]);
    const pane = ensureMainPane();
    const { getByTestId } = render(ProjectsSection, { props: { pane } });
    await tick();

    await fireEvent.click(getByTestId('project-item-new-thread'), { ctrlKey: true });
    await tick();

    expect(getAllPanes().size).toBe(2);
    expect(getFocusedPaneId()).toBe('pane-1');
  });
});
