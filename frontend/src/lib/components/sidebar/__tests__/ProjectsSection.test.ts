import { readSidebarCatalogs } from '../../../../test/helpers/sidebarCatalogs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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
  resetProjectsForTest,
  touchProjectActivity,
} from '../../../stores/projects.svelte';
import { catalogTreeListed, loadSidebarCatalogs, resetSidebarCatalogsForTest } from '../../../stores/sidebarCatalogs';
import {
  ensureMainPane,
  getAllPanes,
  getFocusedPaneId,
  resetPanesForTest,
  syncThread,
} from '../../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../../stores/paneLayout.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { normalizedTextContent } from '../../../../test/helpers/textContent';
import { makeThread } from '../../../../test/helpers/chat';
import { HOME_BACKEND } from '../../../transport/backendKey';
import { takePinnedBackend } from '../../../transport/backends';
import type { Project, ProjectWithCounts, Thread, ThreadGroup } from '../../../types/models';
import { setThreadFilterQuery } from '../../../stores/threadFilter.svelte';
import {
  consumePendingGroupRename,
  resetThreadGroupsForTest,
} from '../../../stores/threadGroups.svelte';
import { createThreadGroupAction } from '../threadGroupActions';
import { resetCatalogLoadForTest } from '../../../stores/catalogLoad.svelte';
import { applyProjectUpdated } from '../../../stores/eventsProjectRows';
import { __setTransportStatusForTest } from '../../../stores/transportStatus.svelte';
import { DisconnectedError, TransportError } from '../../../transport/wsClient';
import { observeBackendGeneration, setBackendIdentityFromBootstrap } from '../../../transport/backendIdentity';

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

async function settleTicks(): Promise<void> {
  for (let i = 0; i < 4; i++) await tick();
}

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (error: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function seedProjects(items: ProjectWithCounts[]): Promise<void> {
  setBindingMock('ListProjects', async () => items);
  await readSidebarCatalogs();
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
    setBindingMock('ListThreadGroups', async () => []);
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
    // into the next. The reads also settle HOME's catalogs as loaded and
    // its thread and group reads as finished, so the list renders instead
    // of its loading row.
    await loadSidebarCatalogs();
    await readSidebarCatalogs();
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
      resetSidebarCatalogsForTest();
    });

    it('shows a loading row naming each catalog of one round, and never the empty hint until the tree loads', async () => {
      const threads = deferred<Thread[]>();
      const groups = deferred<ThreadGroup[]>();
      setBindingMock('ListThreads', () => threads.promise);
      setBindingMock('ListThreadGroups', () => groups.promise);
      const { getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane: null } });
      await tick();
      expect(getByTestId('sidebar-catalog-loading-label')).toHaveTextContent('Loading projects…');
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      // One round reads the three. Its project answer commits for the rest
      // of the app, and the row names the next list it waits for.
      void loadSidebarCatalogs().catch(() => {});
      await vi.waitFor(() => expect(getByTestId('sidebar-catalog-loading-label')).toHaveTextContent('Loading threads…'));
      expect(getByTestId('sidebar-catalog-loading')).toBeInTheDocument();
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      threads.resolve([]);
      await vi.waitFor(() => expect(getByTestId('sidebar-catalog-loading-label')).toHaveTextContent('Loading groups…'));
      expect(queryByTestId('sidebar-projects-empty')).toBeNull();

      groups.resolve([]);
      await vi.waitFor(() => expect(queryByTestId('sidebar-catalog-status')).toBeNull());
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
      expect(normalizedTextContent(getByTestId('sidebar-catalog-loading-meta'))).toMatch(/^Step 3 of 7$/);
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
      await readSidebarCatalogs();
      await loadSidebarCatalogs();
      const { getByTestId, queryByTestId, findByTestId } = render(ProjectsSection, { props: { pane: null } });
      await tick();
      expect(normalizedTextContent(getByTestId('sidebar-catalog-failed'))).toMatch(/database disk image is malformed/i);
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

  function itemIds(container: HTMLElement): string[] {
    return Array.from(container.querySelectorAll('[data-testid="project-item"]'))
      .map((el) => el.getAttribute('data-project-id') ?? '');
  }

  // Showing the list costs what the list is long, not its square. The gate
  // this replaced asked, per entry, which computers held that entry's
  // members, and the question reached a scan of every row; each rendered row
  // asks the same question for its identity errors. Doubling the list
  // doubles the row reads while the cost is linear, and quadruples them if
  // any of those scans returns.
  async function rowReadsFor(count: number): Promise<number> {
    let reads = 0;
    const rows: ProjectWithCounts[] = Array.from({ length: count }, (_, i) => {
      const id = `p-${String(i).padStart(3, '0')}`;
      return {
        project: { ...mkProject(id), get id() { reads++; return id; } },
        threadCount: 0,
        lastActive: 0,
      };
    });
    await seedProjects(rows);
    const { container, unmount } = render(ProjectsSection, { props: { pane: null } });
    await tick();
    expect(itemIds(container)).toHaveLength(count);
    unmount();
    return reads;
  }

  it('reads the project rows a number of times linear in the list length', async () => {
    const small = await rowReadsFor(50);
    resetProjectsForTest();
    resetSidebarForTest();
    const large = await rowReadsFor(100);
    expect(large / small).toBeLessThan(2.6);
  });

  // A refused reorder is undone from the rows it replaced. It cannot wait for
  // the read that follows: the projects are read only beside the threads
  // under them, so a round whose thread half fails withholds the project
  // answer and would leave the rejected order on screen.
  it('puts the previous order back when the reorder write is refused and the read after it fails', async () => {
    await seedProjects([
      { project: mkProject('p-a', { name: 'A', sortPosition: 0 }), threadCount: 0, lastActive: 0 },
      { project: mkProject('p-b', { name: 'B', sortPosition: 1 }), threadCount: 0, lastActive: 0 },
    ]);
    setProjectSortMode('manual');
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await tick();
    expect(itemIds(container)).toEqual(['p-a', 'p-b']);

    const write = vi.fn(async () => { throw new Error('refused'); });
    setBindingMock('UpdateProjectSortPositions', write);
    // The computer would answer the new order if the round ever listed it.
    setBindingMock('ListProjects', async () => [
      { project: mkProject('p-b', { name: 'B', sortPosition: 0 }), threadCount: 0, lastActive: 0 },
      { project: mkProject('p-a', { name: 'A', sortPosition: 1 }), threadCount: 0, lastActive: 0 },
    ]);
    setBindingMock('ListThreads', async () => { throw new Error('offline'); });
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      const items = container.querySelectorAll('[data-testid="project-item"]');
      const dataTransfer = { effectAllowed: '', dropEffect: '', setData: vi.fn(), getData: () => '' };
      // jsdom has no DragEvent, so each event carries its own dataTransfer.
      const drag = async (type: string, target: Element, props: Record<string, unknown> = {}): Promise<void> => {
        const event = new Event(type, { bubbles: true, cancelable: true });
        Object.defineProperty(event, 'dataTransfer', { value: dataTransfer });
        for (const [key, value] of Object.entries(props)) Object.defineProperty(event, key, { value });
        await fireEvent(target, event);
      };
      // The drag starts on the header, the drop lands on the whole row.
      // An empty rect puts the cursor past the row's midpoint: dropped after it.
      const header = items[0].querySelector('[aria-expanded]');
      expect(header).not.toBeNull();
      await drag('dragstart', header!);
      await drag('dragover', items[1], { clientY: 1 });
      await drag('drop', items[1]);
      // The write went out with the dropped order, so the optimistic reorder
      // this undoes really happened.
      await vi.waitFor(() => expect(write).toHaveBeenCalledWith(['p-b', 'p-a']));
      await settleTicks();
      expect(itemIds(container)).toEqual(['p-a', 'p-b']);
    } finally {
      errors.mockRestore();
    }
  });

  // The dropped order is an overlay the list sorts by, dropped when the
  // write settles. What makes that safe is the order on the wire: the
  // computer broadcasts one project:updated frame per row it moved before
  // it returns (internal/app/app_projects.go#UpdateProjectSortPositions),
  // the connection gets every event queued before a reply ahead of that
  // reply (internal/transport/conn.go#writeReply, pinned by
  // TestReplyFollowsEventsEmittedBeforeIt), and this client applies a
  // frame as it arrives (transport/wsClient.ts#handleFrame). So the rows
  // carry the order by the time the overlay goes, and the list never shows
  // the order the user dragged away from.
  it('never shows the order dropped away from, from the drop to the computer\'s frames', async () => {
    await seedProjects([
      { project: mkProject('p-a', { name: 'A', sortPosition: 0 }), threadCount: 0, lastActive: 0 },
      { project: mkProject('p-b', { name: 'B', sortPosition: 1 }), threadCount: 0, lastActive: 0 },
    ]);
    setProjectSortMode('manual');
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await tick();
    expect(itemIds(container)).toEqual(['p-a', 'p-b']);

    const settlement = deferred<Project[]>();
    const write = vi.fn(() => settlement.promise);
    setBindingMock('UpdateProjectSortPositions', write);
    const items = container.querySelectorAll('[data-testid="project-item"]');
    const dataTransfer = { effectAllowed: '', dropEffect: '', setData: vi.fn(), getData: () => '' };
    const drag = async (type: string, target: Element, props: Record<string, unknown> = {}): Promise<void> => {
      const event = new Event(type, { bubbles: true, cancelable: true });
      Object.defineProperty(event, 'dataTransfer', { value: dataTransfer });
      for (const [key, value] of Object.entries(props)) Object.defineProperty(event, key, { value });
      await fireEvent(target, event);
    };
    const sampled: string[][] = [];
    const sample = async (times: number): Promise<void> => {
      for (let i = 0; i < times; i += 1) {
        sampled.push(itemIds(container));
        await tick();
      }
    };

    // B dropped above A.
    await drag('dragstart', items[1].querySelector('[aria-expanded]')!);
    await drag('dragover', items[0], { clientY: -1 });
    await drag('drop', items[0]);
    await sample(4);
    await vi.waitFor(() => expect(write).toHaveBeenCalledWith(['p-b', 'p-a']));
    await sample(2);

    // The frames, then the reply: the order the transport guarantees.
    applyProjectUpdated({ action: 'full', project: mkProject('p-b', { name: 'B', sortPosition: 0 }) });
    applyProjectUpdated({ action: 'full', project: mkProject('p-a', { name: 'A', sortPosition: 1 }) });
    await sample(2);
    settlement.resolve([]);
    await settleTicks();
    await sample(3);

    expect(sampled).toEqual(sampled.map(() => ['p-b', 'p-a']));
    expect(itemIds(container)).toEqual(['p-b', 'p-a']);
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

// A computer's thread list is two catalogs, threads and groups. Shown one
// ahead of the other, its rows would appear in one arrangement and move to
// another when the second commits, under whoever is clicking.
describe('<ProjectsSection> threads and their groups', () => {
  const group = (id: string): ThreadGroup => ({ id, projectId: 'p1', name: id, createdAt: 0, updatedAt: 0 });
  const row = (id: string, updatedAt: number, groupId?: string): Thread =>
    makeThread({ id, projectId: 'p1', groupId, updatedAt });
  // Before: three threads, no group. After: a group made while the
  // computer was away holds one of them and a new one.
  const before = [row('a', 10), row('b', 20), row('outside', 30)];
  const after = [row('a', 10, 'g2'), row('b', 20), row('outside', 30), row('c', 40, 'g2')];
  const BEFORE = ['outside', 'b', 'a'];
  const AFTER = ['g2', 'c', 'a', 'outside', 'b'];

  function arrangement(container: HTMLElement): string[] {
    return Array.from(container.querySelectorAll('[data-sidebar-group-id], [data-sidebar-thread-id]'))
      .map((el) => el.getAttribute('data-sidebar-group-id') ?? el.getAttribute('data-sidebar-thread-id') ?? '');
  }

  async function settle(): Promise<void> {
    for (let i = 0; i < 4; i++) await tick();
  }

  beforeEach(async () => {
    resetSidebarForTest();
    resetProjectsForTest();
    resetPanesForTest();
    resetThreadGroupsForTest();
    resetCatalogLoadForTest();
    setThreadFilterQuery('');
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListThreadGroups', async () => []);
    // The project rows this suite's threads hang from, then back to a
    // computer whose tree has not listed: each case is its first listing.
    await seedProjects([{ project: mkProject('p1', { name: 'Project One' }), threadCount: 3, lastActive: 0 }]);
    resetCatalogLoadForTest();
    resetSidebarCatalogsForTest();
  });
  afterEach(() => { vi.useRealTimers(); });

  for (const first of ['threads', 'groups'] as const) {
    it(`first lists a computer's rows when both answers are in, when the ${first} answer first`, async () => {
      const threads = deferred<Thread[]>();
      const groups = deferred<ThreadGroup[]>();
      setBindingMock('ListThreads', () => threads.promise);
      setBindingMock('ListThreadGroups', () => groups.promise);
      const { container, getByTestId } = render(ProjectsSection, { props: { pane: null } });
      const round = loadSidebarCatalogs();
      if (first === 'threads') threads.resolve(after); else groups.resolve([group('g2')]);
      await settle();
      expect(arrangement(container)).toEqual([]);
      expect(getByTestId('sidebar-catalog-loading-label')).toHaveTextContent(first === 'threads' ? 'Loading groups…' : 'Loading threads…');

      if (first === 'threads') groups.resolve([group('g2')]); else threads.resolve(after);
      await round;
      await settle();
      expect(arrangement(container)).toEqual(AFTER);
    });
  }

  it('holds a computer past the startup deadline until its late group answer, while the boot gets its thread rows', async () => {
    vi.useFakeTimers();
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => after);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const { container } = render(ProjectsSection, { props: { pane: null } });
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    expect((await round).map((thread) => thread.id).sort()).toEqual(['a', 'b', 'c', 'outside']);
    await settle();
    expect(arrangement(container)).toEqual([]);

    groups.resolve([group('g2')]);
    await vi.advanceTimersByTimeAsync(0);
    await settle();
    expect(arrangement(container)).toEqual(AFTER);
  });

  // The project headers are held by the same gate as the rows under them,
  // so a computer's whole tree appears in one step.
  function headers(container: HTMLElement): string[] {
    return Array.from(container.querySelectorAll('[data-project-id]'))
      .map((el) => el.getAttribute('data-project-id') ?? '');
  }

  for (const held of ['projects', 'threads'] as const) {
    it(`shows no project header until the whole tree lists, when the ${held} answer last`, async () => {
      const projects = deferred<ProjectWithCounts[]>();
      const threads = deferred<Thread[]>();
      setBindingMock('ListProjects', () => projects.promise);
      setBindingMock('ListThreads', () => threads.promise);
      setBindingMock('ListThreadGroups', async () => [group('g2')]);
      const { container, getByTestId } = render(ProjectsSection, { props: { pane: null } });
      const round = loadSidebarCatalogs();

      // The catalog that answers first is in the store and still shows
      // nothing: no header with an empty "New Thread" button under it.
      if (held === 'projects') threads.resolve(after);
      else projects.resolve([{ project: mkProject('p1', { name: 'Project One' }), threadCount: 3, lastActive: 0 }]);
      await settle();
      expect(headers(container)).toEqual([]);
      expect(arrangement(container)).toEqual([]);
      expect(getByTestId('sidebar-catalog-loading')).toBeInTheDocument();
      expect(container.querySelector('[data-testid="project-thread-list-empty"]')).toBeNull();

      if (held === 'projects') projects.resolve([{ project: mkProject('p1', { name: 'Project One' }), threadCount: 3, lastActive: 0 }]);
      else threads.resolve(after);
      await round;
      await settle();
      expect(headers(container)).toEqual(['p1']);
      expect(arrangement(container)).toEqual(AFTER);
    });
  }

  // Each computer's tree is held on its own: one held computer must not
  // take another's headers off the list.
  it("lists a listed computer's projects while another computer's tree is held", async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { takePinnedBackend } = await import('../../../transport/backends');
    stageBackend({ id: 'gpu' });
    try {
      const gpuThreads = deferred<Thread[]>();
      setBindingMock('ListProjects', async () => (takePinnedBackend() === 'gpu'
        ? [{ project: mkProject('p2', { name: 'GPU Project', path: '/repo/p2' }), threadCount: 0, lastActive: 0 }]
        : [{ project: mkProject('p1', { name: 'Project One' }), threadCount: 3, lastActive: 0 }]));
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuThreads.promise : Promise.resolve(after)));
      setBindingMock('ListThreadGroups', async () => (takePinnedBackend() === 'gpu' ? [] : [group('g2')]));
      const { container } = render(ProjectsSection, { props: { pane: null } });
      const round = loadSidebarCatalogs();
      await round;
      await settle();
      expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
      expect(catalogTreeListed('gpu')).toBe(false);
      expect(headers(container)).toEqual(['p1']);
      expect(arrangement(container)).toEqual(AFTER);

      gpuThreads.resolve([]);
      await vi.waitFor(() => expect(catalogTreeListed('gpu')).toBe(true));
      await settle();
      expect(headers(container).sort()).toEqual(['p1', 'p2']);
    } finally {
      resetStagedBackends();
    }
  });

  // A held computer must not feed the grouping either: its member would
  // raise the entry's activity and re-sort the list before its rows exist,
  // and move the list again when they arrive.
  const repoProject = (id: string, repo: string, path: string) =>
    mkProject(id, { name: `Repo ${repo}`, path, repositoryID: `github:github.com:${repo}` });

  it("leaves a held computer out of a merged entry's activity, so the order changes once", async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { takePinnedBackend } = await import('../../../transport/backends');
    stageBackend({ id: 'gpu' });
    try {
      const gpuThreads = deferred<Thread[]>();
      setBindingMock('ListProjects', async () => (takePinnedBackend() === 'gpu'
        // The GPU checkout of repository A carries the newest activity in
        // the entry it merges into.
        ? [{ project: repoProject('a-gpu', 'A', '/gpu/a'), threadCount: 1, lastActive: 300 }]
        : [
          { project: repoProject('a-home', 'A', '/repo/a'), threadCount: 1, lastActive: 100 },
          { project: repoProject('b-home', 'B', '/repo/b'), threadCount: 1, lastActive: 200 },
        ]));
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu'
        ? gpuThreads.promise
        : Promise.resolve([makeThread({ id: 'ta', projectId: 'a-home' }), makeThread({ id: 'tb', projectId: 'b-home' })])));
      setBindingMock('ListThreadGroups', async () => []);
      const { container } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      expect(catalogTreeListed('gpu')).toBe(false);
      // B is still the newer entry: the held member's 300 is not folded in.
      expect(headers(container)).toEqual(['b-home', 'a-home']);

      gpuThreads.resolve([]);
      await vi.waitFor(() => expect(catalogTreeListed('gpu')).toBe(true));
      await settle();
      // One change, and it is the whole change: the member joins its entry
      // and the entry takes the top in the same step.
      expect(headers(container)).toEqual(['a-home', 'b-home']);
    } finally {
      resetStagedBackends();
    }
  });

  // One member has to pass both the listing test and the device filter for
  // its entry to show, or a filter that hides the only listed computer
  // leaves an empty header behind.
  it('shows no header when the device filter hides the only computer whose tree lists', async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { takePinnedBackend } = await import('../../../transport/backends');
    const { setSidebarDeviceVisible, showAllSidebarDevices } = await import('../../../stores/sidebarDevices.svelte');
    stageBackend({ id: 'mac', backendId: 'mac-uuid' });
    stageBackend({ id: 'gpu', backendId: 'gpu-uuid' });
    try {
      const gpuThreads = deferred<Thread[]>();
      setBindingMock('ListProjects', async () => {
        const computer = takePinnedBackend();
        if (computer === 'mac') return [{ project: repoProject('a-mac', 'A', '/mac/a'), threadCount: 0, lastActive: 100 }];
        if (computer === 'gpu') return [{ project: repoProject('a-gpu', 'A', '/gpu/a'), threadCount: 0, lastActive: 0 }];
        return [];
      });
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuThreads.promise : Promise.resolve([])));
      setBindingMock('ListThreadGroups', async () => []);
      const { container } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      expect(headers(container)).toEqual(['a-mac']);

      // Hide the Mac: the entry's only remaining member is the GPU one,
      // whose tree is held, so the entry has nothing to show.
      setSidebarDeviceVisible('mac-uuid', false);
      await settle();
      expect(headers(container)).toEqual([]);

      gpuThreads.resolve([]);
      await vi.waitFor(() => expect(catalogTreeListed('gpu')).toBe(true));
      await settle();
      // The entry it comes back as is the one it left: identity is the
      // complete merge's representative, not the member being shown.
      expect(headers(container)).toEqual(['a-mac']);
    } finally {
      showAllSidebarDevices();
      resetStagedBackends();
    }
  });

  // An entry's identity is the complete merge's representative, so what the
  // sidebar shows of it cannot move the state persisted under it. The each
  // key, the collapse set and the show-more limit all key on it, and a
  // reorder sends it to the computer that holds it.
  const mergedRepoMocks = (homeRows: ProjectWithCounts[], gpuRows: ProjectWithCounts[]) => {
    setBindingMock('ListProjects', async () => (takePinnedBackend() === 'gpu' ? gpuRows : homeRows));
  };
  const chevron = (container: HTMLElement, id: string): HTMLElement => {
    const el = container.querySelector(`[data-project-id="${id}"] [data-testid="project-item-chevron"]`);
    if (!(el instanceof HTMLElement)) throw new Error(`no chevron for ${id}`);
    return el;
  };
  const expandedOf = (container: HTMLElement, id: string): string | null =>
    container.querySelector(`[data-project-id="${id}"] [aria-expanded]`)?.getAttribute('aria-expanded') ?? null;

  it("keeps a collapsed entry's identity, and its show-more limit, when the filter hides its home member", async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { setSidebarDeviceVisible, showAllSidebarDevices } = await import('../../../stores/sidebarDevices.svelte');
    const { getThreadListVisibleLimit, setThreadListVisibleLimit } = await import('../../../stores/sidebar.svelte');
    const { projectDisclosureKey } = await import('../../../stores/projectDisclosure');
    stageBackend({ id: 'gpu', backendId: 'gpu-id' });
    setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
    try {
      mergedRepoMocks(
        [{ project: repoProject('a-home', 'A', '/home/a'), threadCount: 1, lastActive: 1 }],
        [{ project: repoProject('a-gpu', 'A', '/gpu/a'), threadCount: 12, lastActive: 2 }],
      );
      setBindingMock('ListThreads', async () => (takePinnedBackend() === 'gpu'
        ? Array.from({ length: 12 }, (_, i) => makeThread({ id: `g${i}`, projectId: 'a-gpu' }))
        : [makeThread({ id: 'h0', projectId: 'a-home' })]));
      setBindingMock('ListThreadGroups', async () => []);
      const { container } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      expect(headers(container)).toEqual(['a-home']);

      await fireEvent.click(chevron(container, 'a-home'));
      setThreadListVisibleLimit(projectDisclosureKey('a-home'), 10);
      await settle();
      expect(expandedOf(container, 'a-home')).toBe('false');

      setSidebarDeviceVisible('home-id', false);
      await settle();
      // Same entry, still collapsed, and the limit still the one the user
      // revealed to: a rekey to the GPU member would show it expanded at
      // the default preview.
      expect(headers(container)).toEqual(['a-home']);
      expect(expandedOf(container, 'a-home')).toBe('false');
      expect(getThreadListVisibleLimit(projectDisclosureKey('a-home'))).toBe(10);

      await fireEvent.click(chevron(container, 'a-home'));
      await settle();
      expect(arrangement(container)).toHaveLength(10);
    } finally {
      showAllSidebarDevices();
      setBackendIdentityFromBootstrap('', '', '', HOME_BACKEND);
      resetStagedBackends();
    }
  });

  it("keeps the row it rendered, and its disclosure, through a held member's commit and its listing", async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const {
      collapseProject,
      getThreadListVisibleLimit,
      setThreadListVisibleLimit,
    } = await import('../../../stores/sidebar.svelte');
    const { projectDisclosureKey } = await import('../../../stores/projectDisclosure');
    stageBackend({ id: 'gpu', backendId: 'gpu-id' });
    try {
      // The user collapsed the entry and revealed more of it while GPU was
      // the only computer showing it, under the entry's own key: repository
      // A's, which is the key whatever checkout represents it.
      const keyOfA = 'forge:github:github.com:A';
      collapseProject(keyOfA);
      setThreadListVisibleLimit(keyOfA, 10);
      const homeProjects = deferred<ProjectWithCounts[]>();
      const homeThreads = deferred<Thread[]>();
      setBindingMock('ListProjects', () => (takePinnedBackend() === 'gpu'
        ? Promise.resolve([{ project: repoProject('a-gpu', 'A', '/gpu/a'), threadCount: 1, lastActive: 2 }])
        : homeProjects.promise));
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu'
        ? Promise.resolve([makeThread({ id: 'g0', projectId: 'a-gpu' })])
        : homeThreads.promise));
      setBindingMock('ListThreadGroups', async () => []);
      vi.useFakeTimers();
      const { container } = render(ProjectsSection, { props: { pane: null } });
      void loadSidebarCatalogs();
      // GPU's answers wait on the round's windows, which the startup
      // deadline closes while Home's reads are still out.
      await vi.advanceTimersByTimeAsync(2500);
      await settle();
      expect(catalogTreeListed('gpu')).toBe(true);
      expect(headers(container)).toEqual(['a-gpu']);
      expect(expandedOf(container, 'a-gpu')).toBe('false');
      expect(projectDisclosureKey('a-gpu')).toBe(keyOfA);
      expect(getThreadListVisibleLimit(keyOfA)).toBe(10);
      const row = container.querySelector('[data-testid="project-item"]');

      // Home's project row commits at its own window while Home's threads
      // are still out. Nothing of the entry on screen may move for it.
      homeProjects.resolve([{ project: repoProject('a-home', 'A', '/home/a'), threadCount: 1, lastActive: 1 }]);
      await vi.advanceTimersByTimeAsync(0);
      await settle();
      expect(catalogTreeListed(HOME_BACKEND)).toBe(false);
      expect(headers(container)).toEqual(['a-gpu']);
      expect(expandedOf(container, 'a-gpu')).toBe('false');
      expect(getThreadListVisibleLimit(projectDisclosureKey('a-gpu'))).toBe(10);
      expect(container.querySelector('[data-testid="project-item"]')).toBe(row);

      // At Home's listing the entry changes hands, by design. Its key does
      // not, so the row is the same row and keeps its disclosure.
      homeThreads.resolve([makeThread({ id: 'h0', projectId: 'a-home' })]);
      await vi.advanceTimersByTimeAsync(0);
      await settle();
      expect(catalogTreeListed(HOME_BACKEND)).toBe(true);
      expect(headers(container)).toEqual(['a-home']);
      expect(container.querySelector('[data-testid="project-item"]')).toBe(row);
      expect(expandedOf(container, 'a-home')).toBe('false');
      expect(getThreadListVisibleLimit(projectDisclosureKey('a-home'))).toBe(10);
    } finally {
      resetStagedBackends();
    }
  });

  it('gives a second checkout on one computer its own row, and keeps the first one when a computer attaches', async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { collapseProject } = await import('../../../stores/sidebar.svelte');
    const { projectDisclosureKey } = await import('../../../stores/projectDisclosure');
    const homeRows = [
      { project: { ...repoProject('a1', 'A', '/home/a1'), createdAt: 1 }, threadCount: 0, lastActive: 200 },
      { project: { ...repoProject('a2', 'A', '/home/a2'), createdAt: 2 }, threadCount: 0, lastActive: 100 },
    ];
    setBindingMock('ListProjects', async () => (takePinnedBackend() === 'gpu'
      ? [{ project: { ...repoProject('a-gpu', 'A', '/gpu/a'), createdAt: 3 }, threadCount: 0, lastActive: 50 }]
      : homeRows));
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListThreadGroups', async () => []);
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    await settle();
    // One repository, two checkouts on one computer: two entries, so two
    // keys. The repository key belongs to the computer's chosen checkout.
    expect(headers(container)).toEqual(['a1', 'a2']);
    collapseProject(projectDisclosureKey('a1'));
    await settle();
    expect(expandedOf(container, 'a1')).toBe('false');
    expect(expandedOf(container, 'a2')).toBe('true');
    const first = container.querySelector('[data-project-id="a1"]');

    // Two rows of one repository re-sort past each other and keep their
    // own row and their own disclosure, which two blocks under one key
    // cannot.
    touchProjectActivity('a2', 500);
    await settle();
    expect(headers(container)).toEqual(['a2', 'a1']);
    expect(expandedOf(container, 'a1')).toBe('false');
    expect(expandedOf(container, 'a2')).toBe('true');
    expect(container.querySelector('[data-project-id="a1"]')).toBe(first);

    try {
      stageBackend({ id: 'gpu', backendId: 'gpu-id' });
      await loadSidebarCatalogs();
      await settle();
      // The GPU checkout merges into the chosen one, whose key was already
      // the repository's, so its row is the same row.
      expect(headers(container)).toEqual(['a2', 'a1']);
      expect(container.querySelector('[data-project-id="a1"]')).toBe(first);
      expect(expandedOf(container, 'a1')).toBe('false');
      expect(expandedOf(container, 'a2')).toBe('true');
    } finally {
      resetStagedBackends();
    }
  });

  it('dates a merged entry by the members it shows, not by the one that represents it', async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { setSidebarDeviceVisible, showAllSidebarDevices } = await import('../../../stores/sidebarDevices.svelte');
    stageBackend({ id: 'gpu', backendId: 'gpu-id' });
    setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
    try {
      setBindingMock('ListProjects', async () => (takePinnedBackend() === 'gpu'
        ? [
          { project: repoProject('a-gpu', 'A', '/gpu/a'), threadCount: 0, lastActive: 100 },
          { project: repoProject('b-gpu', 'B', '/gpu/b'), threadCount: 0, lastActive: 200 },
        ]
        : [{ project: repoProject('a-home', 'A', '/home/a'), threadCount: 0, lastActive: 100 }]));
      setBindingMock('ListThreads', async () => []);
      setBindingMock('ListThreadGroups', async () => []);
      const { container } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      setSidebarDeviceVisible('home-id', false);
      await settle();
      expect(headers(container)).toEqual(['b-gpu', 'a-home']);

      // The representative's own box, on a computer the filter hides: not
      // this entry's activity.
      touchProjectActivity('a-home', 400);
      await settle();
      expect(headers(container)).toEqual(['b-gpu', 'a-home']);

      // The member it shows: its beat is the entry's.
      touchProjectActivity('a-gpu', 300);
      await settle();
      expect(headers(container)).toEqual(['a-home', 'b-gpu']);
    } finally {
      showAllSidebarDevices();
      setBackendIdentityFromBootstrap('', '', '', HOME_BACKEND);
      resetStagedBackends();
    }
  });

  it('reorders by the ids of the computer that holds them while the filter hides it', async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    const { setSidebarDeviceVisible, showAllSidebarDevices } = await import('../../../stores/sidebarDevices.svelte');
    stageBackend({ id: 'gpu', backendId: 'gpu-id' });
    setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
    try {
      mergedRepoMocks(
        [
          { project: { ...repoProject('a-home', 'A', '/home/a'), sortPosition: 0 }, threadCount: 0, lastActive: 0 },
          { project: { ...repoProject('b-home', 'B', '/home/b'), sortPosition: 1 }, threadCount: 0, lastActive: 0 },
        ],
        [
          { project: { ...repoProject('a-gpu', 'A', '/gpu/a'), sortPosition: 0 }, threadCount: 0, lastActive: 0 },
          { project: { ...repoProject('b-gpu', 'B', '/gpu/b'), sortPosition: 1 }, threadCount: 0, lastActive: 0 },
        ],
      );
      setBindingMock('ListThreads', async () => []);
      setBindingMock('ListThreadGroups', async () => []);
      setProjectSortMode('manual');
      const write = vi.fn(async () => []);
      setBindingMock('UpdateProjectSortPositions', write);
      const { container } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      setSidebarDeviceVisible('home-id', false);
      await settle();
      expect(headers(container)).toEqual(['a-home', 'b-home']);

      const items = container.querySelectorAll('[data-testid="project-item"]');
      const dataTransfer = { effectAllowed: '', dropEffect: '', setData: vi.fn(), getData: () => '' };
      const drag = async (type: string, target: Element, props: Record<string, unknown> = {}): Promise<void> => {
        const event = new Event(type, { bubbles: true, cancelable: true });
        Object.defineProperty(event, 'dataTransfer', { value: dataTransfer });
        for (const [key, value] of Object.entries(props)) Object.defineProperty(event, key, { value });
        await fireEvent(target, event);
      };
      await drag('dragstart', items[1].querySelector('[aria-expanded]')!);
      await drag('dragover', items[0], { clientY: -1 });
      await drag('drop', items[0]);
      // Home's ids, which Home holds and can save, though Home is hidden.
      await vi.waitFor(() => expect(write).toHaveBeenCalledWith(['b-home', 'a-home']));
    } finally {
      showAllSidebarDevices();
      setBackendIdentityFromBootstrap('', '', '', HOME_BACKEND);
      resetStagedBackends();
    }
  });

  // What a header says about its members comes from the same projection
  // that decided the header: a member with no row yet is not one of them.
  it('leaves a held member out of the header\'s identity warning and out of its recheck', async () => {
    const { stageBackend, resetStagedBackends } = await import('../../../../test/helpers/backends');
    stageBackend({ id: 'gpu', backendId: 'gpu-id' });
    try {
      const gpuThreads = deferred<Thread[]>();
      mergedRepoMocks(
        [{ project: { ...repoProject('a-home', 'A', '/home/a'), identityError: 'home reason' }, threadCount: 0, lastActive: 0 }],
        [{ project: { ...repoProject('a-gpu', 'A', '/gpu/a'), identityError: 'gpu reason' }, threadCount: 0, lastActive: 0 }],
      );
      setBindingMock('ListThreads', () => (takePinnedBackend() === 'gpu' ? gpuThreads.promise : Promise.resolve([])));
      setBindingMock('ListThreadGroups', async () => []);
      const refresh = vi.fn(async (id: string) => ({ ...repoProject(id, 'A', '/home/a'), identityError: '' }));
      setBindingMock('RefreshProjectIdentity', refresh);
      const { container, getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      expect(headers(container)).toEqual(['a-home']);
      expect(getByTestId('project-item-identity-error').getAttribute('title'))
        .toBe('Could not fully verify /home/a: home reason');

      await fireEvent.click(getByTestId('project-item-identity-error'));
      // Only the member with a row: the held one is never asked.
      await vi.waitFor(() => expect(refresh).toHaveBeenCalledWith('a-home'));
      expect(refresh).toHaveBeenCalledTimes(1);
      await vi.waitFor(() => expect(queryByTestId('project-item-identity-error')).toBeNull());

      // The held member joins the header's account of itself when its own
      // row exists, and not before.
      gpuThreads.resolve([]);
      await vi.waitFor(() => expect(catalogTreeListed('gpu')).toBe(true));
      await settle();
      expect(getByTestId('project-item-identity-error').getAttribute('title'))
        .toBe('Could not fully verify /gpu/a: gpu reason');
    } finally {
      resetStagedBackends();
    }
  });

  it('holds a computer that was offline with nothing cached until both answers of its first connection', async () => {
    setBindingMock('ListThreads', async () => { throw new DisconnectedError('Computer is offline.'); });
    setBindingMock('ListThreadGroups', async () => { throw new DisconnectedError('Computer is offline.'); });
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await expect(loadSidebarCatalogs()).rejects.toBeInstanceOf(DisconnectedError);
    expect(catalogTreeListed(HOME_BACKEND)).toBe(false);

    // The first hello reads both again; the threads answer first.
    const threads = deferred<Thread[]>();
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const hello = loadSidebarCatalogs();
    threads.resolve(after);
    await settle();
    expect(arrangement(container)).toEqual([]);
    groups.resolve([group('g2')]);
    await hello;
    await settle();
    expect(arrangement(container)).toEqual(AFTER);
  });

  for (const first of ['groups', 'threads'] as const) {
    it(`moves a listed computer's rows in one step after a reconnect, when the ${first} answer first`, async () => {
      setBindingMock('ListThreads', async () => before);
      setBindingMock('ListThreadGroups', async () => []);
      const { container } = render(ProjectsSection, { props: { pane: null } });
      await loadSidebarCatalogs();
      await settle();
      expect(arrangement(container)).toEqual(BEFORE);

      const threads = deferred<Thread[]>();
      const groups = deferred<ThreadGroup[]>();
      setBindingMock('ListThreads', () => threads.promise);
      setBindingMock('ListThreadGroups', () => groups.promise);
      const reconnect = loadSidebarCatalogs();
      if (first === 'threads') threads.resolve(after); else groups.resolve([group('g2')]);
      await settle();
      expect(arrangement(container)).toEqual(BEFORE);

      if (first === 'threads') groups.resolve([group('g2')]); else threads.resolve(after);
      await reconnect;
      await settle();
      expect(arrangement(container)).toEqual(AFTER);
    });
  }

  // The thread answer is held with its partner, not shown alone: the rows
  // stay as they were until the late group answer lands, then move once.
  // The round still resolves with the held thread rows for the restore.
  it("keeps a listed computer's rows in place while its group read is past the deadline, then moves them once", async () => {
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    vi.useFakeTimers();
    const groups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', async () => after);
    setBindingMock('ListThreadGroups', () => groups.promise);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    expect((await round).map((thread) => thread.id).sort()).toEqual(['a', 'b', 'c', 'outside']);
    await settle();
    expect(arrangement(container)).toEqual(BEFORE);

    groups.resolve([group('g2')]);
    await vi.advanceTimersByTimeAsync(0);
    await settle();
    expect(arrangement(container)).toEqual(AFTER);
  });

  it("keeps a listed computer's rows in place while its thread read is past the deadline, then moves them once", async () => {
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    vi.useFakeTimers();
    const threads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    const round = loadSidebarCatalogs();
    await vi.advanceTimersByTimeAsync(2500);
    await round;
    await settle();
    expect(arrangement(container)).toEqual(BEFORE);

    threads.resolve(after);
    await vi.advanceTimersByTimeAsync(0);
    await settle();
    expect(arrangement(container)).toEqual(AFTER);
  });

  // A loaded catalog can fail on a refresh or go back to loading on a history
  // generation change. Either way its rows stay listed, beside the status
  // row, and are never presented as empty.
  it("keeps a listed computer's rows beside the failed row when a refresh fails", async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container, getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    await settle();
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    await loadSidebarCatalogs();
    await settle();
    expect(normalizedTextContent(getByTestId('sidebar-catalog-failed'))).toMatch(/disk I\/O error/i);
    expect(arrangement(container)).toEqual(BEFORE);
    expect(queryByTestId('project-thread-list-empty')).toBeNull();
  });

  // G1: the group answer does not show alone while the failed thread read
  // retries; the retry's pair moves the rows in one step.
  it("keeps a listed computer's arrangement when a refresh's thread read fails beside a new group, until the retry moves it once", async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container, getByTestId } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    await settle();
    expect(arrangement(container)).toEqual(BEFORE);

    vi.useFakeTimers();
    const shown: string[][] = [arrangement(container)];
    const observer = new MutationObserver(() => {
      const next = arrangement(container);
      if (next.join() !== shown[shown.length - 1]!.join()) shown.push(next);
    });
    observer.observe(container, { childList: true, subtree: true, attributes: true });
    setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    await loadSidebarCatalogs().catch(() => {});
    await vi.advanceTimersByTimeAsync(0);
    await settle();
    expect(normalizedTextContent(getByTestId('sidebar-catalog-failed'))).toMatch(/disk I\/O error/i);
    expect(arrangement(container)).toEqual(BEFORE);

    setBindingMock('ListThreads', async () => after);
    await vi.advanceTimersByTimeAsync(1000);
    await settle();
    observer.disconnect();
    expect(arrangement(container)).toEqual(AFTER);
    expect(shown).toEqual([BEFORE, AFTER]);
  });

  // H2: no attempt publishes half the pair, the retries included.
  it("keeps a listed computer's arrangement through a refresh's thread failure and its retry's, then changes it once at the success", async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container, getByTestId } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    await settle();
    expect(arrangement(container)).toEqual(BEFORE);

    vi.useFakeTimers();
    const shown: string[][] = [arrangement(container)];
    const observer = new MutationObserver(() => {
      const next = arrangement(container);
      if (next.join() !== shown[shown.length - 1]!.join()) shown.push(next);
    });
    observer.observe(container, { childList: true, subtree: true, attributes: true });
    const threads = setBindingMock('ListThreads', async () => { throw new TransportError('method_error', 'list threads: disk I/O error'); });
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    await loadSidebarCatalogs().catch(() => {});
    await vi.advanceTimersByTimeAsync(0);
    await settle();
    expect(normalizedTextContent(getByTestId('sidebar-catalog-failed'))).toMatch(/disk I\/O error/i);
    expect(arrangement(container)).toEqual(BEFORE);

    // The retry's thread read fails again.
    await vi.advanceTimersByTimeAsync(1000);
    await settle();
    expect(threads).toHaveBeenCalledTimes(2);
    expect(getByTestId('sidebar-catalog-failed')).toBeInTheDocument();
    expect(arrangement(container)).toEqual(BEFORE);

    threads.mockImplementation(async () => after);
    await vi.advanceTimersByTimeAsync(2000);
    await settle();
    observer.disconnect();
    expect(arrangement(container)).toEqual(AFTER);
    expect(shown).toEqual([BEFORE, AFTER]);
  });

  it("keeps the old history's rows beside a loading row after a generation change, then replaces them in one step", async () => {
    setBackendIdentityFromBootstrap('home-id', 'gen-1', 'Mac', HOME_BACKEND);
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container, getByTestId, queryByTestId } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    await settle();
    const threads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => threads.promise);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    observeBackendGeneration('gen-2', HOME_BACKEND);
    await settle();
    expect(getByTestId('sidebar-catalog-loading')).toBeInTheDocument();
    expect(arrangement(container)).toEqual(BEFORE);
    expect(queryByTestId('project-thread-list-empty')).toBeNull();
    threads.resolve(after);
    await vi.waitFor(() => expect(arrangement(container)).toEqual(AFTER));
    await vi.waitFor(() => expect(queryByTestId('sidebar-catalog-status')).toBeNull());
  });

  // A newer round supersedes both reads of the computer at once: the older
  // round's group answer, in and waiting for its threads, never shows.
  it('shows only the newer of two overlapping rounds, in one step', async () => {
    setBindingMock('ListThreads', async () => before);
    setBindingMock('ListThreadGroups', async () => []);
    const { container } = render(ProjectsSection, { props: { pane: null } });
    await loadSidebarCatalogs();
    await settle();

    const olderThreads = deferred<Thread[]>();
    setBindingMock('ListThreads', () => olderThreads.promise);
    setBindingMock('ListThreadGroups', async () => [group('g2')]);
    const older = loadSidebarCatalogs();
    await settle();
    const newerThreads = deferred<Thread[]>();
    const newerGroups = deferred<ThreadGroup[]>();
    setBindingMock('ListThreads', () => newerThreads.promise);
    setBindingMock('ListThreadGroups', () => newerGroups.promise);
    const newer = loadSidebarCatalogs();
    await settle();
    expect(arrangement(container)).toEqual(BEFORE);
    olderThreads.resolve(after);
    await settle();
    expect(arrangement(container)).toEqual(BEFORE);

    newerThreads.resolve(after);
    newerGroups.resolve([group('g2')]);
    await Promise.all([older, newer]);
    await settle();
    expect(arrangement(container)).toEqual(AFTER);
  });
});
