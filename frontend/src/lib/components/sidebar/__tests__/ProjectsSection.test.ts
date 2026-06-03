import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';

// Stub ThreadRow so the Terminals group's rows are assertable without
// ThreadRow's heavy store graph. Safe here: the project tests seed no threads
// (ListThreads → []), so nothing else in this file renders a ThreadRow.
vi.mock('../ThreadRow.svelte', async () => ({
  default: (await import('../../../../test/mocks/StubThreadRow.svelte')).default,
}));

import ProjectsSection from '../ProjectsSection.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import {
  collapseTerminalsGroup,
  getProjectSortMode,
  isTerminalsGroupExpanded,
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
import type { Project, ProjectWithCounts, Thread } from '../../../types/models';
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

function standaloneTerminal(id: string, title: string, workspacePath: string): Thread {
  return {
    id,
    title,
    provider: 'claude',
    workspacePath,
    projectPath: '',
    projectId: undefined,
    mode: 'terminal',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

describe('<ProjectsSection>', () => {
  beforeEach(async () => {
    resetSidebarForTest();
    resetProjectsForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
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
    // Reset the threads store so a standalone terminal seeded by one test
    // doesn't leak into the next.
    await refreshThreads();
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

  it('always shows the Terminals group at rest, even with zero standalone terminals', async () => {
    const pane = createThreadPane();
    const { getByTestId } = render(ProjectsSection, { props: { pane } });
    await tick();
    // Header (and its global +terminal) is always reachable at rest.
    expect(getByTestId('sidebar-terminals-group')).toBeInTheDocument();
  });

  it('search reveals matching standalone terminals and removes the group when none match', async () => {
    // Default-expanded state: this pins the behavior the carve-out exists for —
    // a *matching* terminal row stays VISIBLE during search, not just the group
    // container. (Collapsed-while-searching hides matches; that gap is tracked
    // separately — see the auto-expand discussion.)
    setBindingMock('ListThreads', async () => [
      standaloneTerminal('term-notes', 'notes', '/home/me'),
      standaloneTerminal('term-logs', 'scratch', '/var/logs'),
    ]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId, queryByTestId, getAllByTestId } = render(ProjectsSection, {
      props: { pane },
    });
    await tick();

    // At rest (expanded by default): both standalone terminals render as rows.
    expect(getByTestId('sidebar-terminals-group')).toBeInTheDocument();
    expect(
      getAllByTestId('stub-thread-row').map((r) => r.getAttribute('data-thread-id')),
    ).toEqual(['term-notes', 'term-logs']);

    // Search matching only 'notes' (by title): that row stays VISIBLE and the
    // non-matching one is filtered out — the actual point of the carve-out.
    setThreadFilterQuery('notes');
    await tick();
    expect(getByTestId('sidebar-terminals-group')).toBeInTheDocument();
    expect(
      getAllByTestId('stub-thread-row').map((r) => r.getAttribute('data-thread-id')),
    ).toEqual(['term-notes']);

    // Search matching nothing: the whole group (header + rows) disappears, so no
    // dangling "Terminals" header is left in the results.
    setThreadFilterQuery('zzz');
    await tick();
    expect(queryByTestId('sidebar-terminals-group')).toBeNull();
    expect(queryByTestId('stub-thread-row')).toBeNull();

    // Uppercase query still matches — exercises the lowercase-normalize boundary
    // (query = getThreadFilterQuery().trim().toLowerCase()).
    setThreadFilterQuery('NOTES');
    await tick();
    expect(
      getAllByTestId('stub-thread-row').map((r) => r.getAttribute('data-thread-id')),
    ).toEqual(['term-notes']);
  });

  it('auto-expands a collapsed Terminals group to reveal a search match, then rolls back on clear', async () => {
    // Mirrors the per-project auto-expand: a collapsed group would otherwise
    // swallow search hits. We own this expansion (we flipped it), so clearing
    // the query must undo it.
    collapseTerminalsGroup();
    setBindingMock('ListThreads', async () => [
      standaloneTerminal('term-notes', 'notes', '/home/me'),
    ]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane } });
    await tick();

    // Collapsed at rest → header present, rows hidden.
    expect(getByTestId('sidebar-terminals-group')).toBeInTheDocument();
    expect(queryByTestId('stub-thread-row')).toBeNull();
    expect(getByTestId('sidebar-terminals-chevron').getAttribute('aria-expanded')).toBe('false');

    // Matching search auto-expands → the matching row is revealed and the
    // chevron reflects the temporary expansion.
    setThreadFilterQuery('notes');
    await tick();
    expect(getByTestId('stub-thread-row').getAttribute('data-thread-id')).toBe('term-notes');
    expect(getByTestId('sidebar-terminals-chevron').getAttribute('aria-expanded')).toBe('true');

    // Clearing the query rolls our expansion back — collapsed again, rows hidden.
    setThreadFilterQuery('');
    await tick();
    expect(queryByTestId('stub-thread-row')).toBeNull();
    expect(isTerminalsGroupExpanded()).toBe(false);
  });

  it('leaves a user-expanded Terminals group expanded after a search clears (ownership guard)', async () => {
    // The group is expanded going in (the default — i.e. NOT collapsed by us).
    // The auto-expand effect must not claim ownership it doesn't have: after a
    // matching search and a clear, the group stays expanded. This is the only
    // test that pins the `terminalsAutoExpanded` tracking — a naive "expand on
    // match / collapse on clear" rewrite would wrongly collapse it here.
    expect(isTerminalsGroupExpanded()).toBe(true);
    setBindingMock('ListThreads', async () => [
      standaloneTerminal('term-notes', 'notes', '/home/me'),
    ]);
    await refreshThreads();

    const pane = createThreadPane();
    const { getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane } });
    await tick();
    expect(queryByTestId('stub-thread-row')).not.toBeNull();

    setThreadFilterQuery('notes');
    await tick();
    expect(getByTestId('stub-thread-row').getAttribute('data-thread-id')).toBe('term-notes');

    setThreadFilterQuery('');
    await tick();
    // Still expanded, rows still visible — we never owned the expansion, so we
    // must not have collapsed it.
    expect(isTerminalsGroupExpanded()).toBe(true);
    expect(getByTestId('stub-thread-row').getAttribute('data-thread-id')).toBe('term-notes');
  });
});
