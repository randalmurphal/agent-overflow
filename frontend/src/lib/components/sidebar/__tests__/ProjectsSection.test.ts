import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ProjectsSection from '../ProjectsSection.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  getSortDirection,
  resetSidebarForTest,
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

function wrap(p: Project, threadCount = 0): ProjectWithCounts {
  return { project: p, threadCount, lastActive: 0 };
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
    expect(getByRole('button', { name: 'Add project' })).toBeInTheDocument();
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
    await fireEvent.click(getByRole('button', { name: 'Add project' }));
    await tick();
    await Promise.resolve();
    expect(getByRole('dialog')).toBeInTheDocument();
    // Heading inside the modal matches the configured title.
    const heading = document.querySelector('[role="dialog"] h2');
    expect(heading?.textContent).toBe('Add project');
  });

  it('sort toggle flips asc/desc and is persisted via the store', async () => {
    expect(getSortDirection()).toBe('desc');
    const pane = createThreadPane();
    const { getByRole } = render(ProjectsSection, { props: { pane } });
    const sortBtn = getByRole('button', {
      name: /Sort (ascending|descending)/,
    });
    await fireEvent.click(sortBtn);
    expect(getSortDirection()).toBe('asc');
  });

  it('sorts projects by name respecting direction', async () => {
    await seedProjects([
      wrap(mkProject('p-beta', { name: 'Beta' })),
      wrap(mkProject('p-alpha', { name: 'Alpha' })),
    ]);
    const pane = createThreadPane();
    const { container } = render(ProjectsSection, { props: { pane } });
    await tick();
    // Default is desc — "Beta" before "Alpha".
    const initial = Array.from(
      container.querySelectorAll('[data-testid="project-item"]'),
    ).map((el) => el.getAttribute('data-project-id'));
    expect(initial).toEqual(['p-beta', 'p-alpha']);
  });
});
