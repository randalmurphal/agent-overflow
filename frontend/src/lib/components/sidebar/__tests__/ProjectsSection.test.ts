import { beforeEach, describe, expect, it } from 'vitest';
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
} from '../../../stores/projects.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import type { Project, ProjectWithCounts } from '../../../types/models';
import { setThreadFilterQuery } from '../../../stores/threadFilter.svelte';

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
  beforeEach(() => {
    resetSidebarForTest();
    resetProjectsForTest();
    setThreadFilterQuery('');
    setBindingMock('ListProjects', async () => []);
    setBindingMock('ListThreads', async () => []);
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
});
