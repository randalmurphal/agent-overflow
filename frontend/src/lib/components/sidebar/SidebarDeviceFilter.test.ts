import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor, within } from '@testing-library/svelte';
import { takePinnedBackend } from '../../transport/backends';
import { TransportError, type TransportStatusSnapshot } from '../../transport/wsClient';
import { resetCatalogLoadForTest } from '../../stores/catalogLoad.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { normalizedTextContent } from '../../../test/helpers/textContent';
import { tick } from 'svelte';
import ProjectsSection from './ProjectsSection.svelte';
import SidebarDeviceFilter from './SidebarDeviceFilter.svelte';
import { stageNoRemoteBuild } from '../../../test/helpers/buildVariant';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { setBackendIdentityFromBootstrap } from '../../transport/backendIdentity';
import { noteProject, noteThread } from '../../transport/entityIndex';
import { addProjectLocal, updateProjectLocal, resetProjectsForTest, touchProjectActivity } from '../../stores/projects.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { listCatalogTreesForTest, loadSidebarCatalogs, resetSidebarCatalogsForTest } from '../../stores/sidebarCatalogs';
import { upsertThreadGroup, resetThreadGroupsForTest } from '../../stores/threadGroups.svelte';
import { showAllSidebarDevices, setSidebarDeviceVisible, sidebarDevices } from '../../stores/sidebarDevices.svelte';
import { setProjectSortMode } from '../../stores/sidebar.svelte';
import { setThreadFilterQuery } from '../../stores/threadFilter.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { selectedBackend, setSelectedBackend } from '../../stores/selectedBackend.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { Thread } from '../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function thread(id: string, projectId: string, groupId?: string): Thread {
  return { id, projectId, groupId, title: id, provider: 'claude', model: 'claude-sonnet-4-6', workspacePath: '/repo', projectPath: '/repo', createdAt: 1, updatedAt: 1, archived: false, pinnedAt: groupId ? undefined : 1 };
}
beforeEach(() => {
  resetStagedBackends(); resetProjectsForTest(); resetThreadGroupsForTest(); replaceAllThreads([]);
  showAllSidebarDevices(); setThreadFilterQuery('');
  setBackendIdentityFromBootstrap('local-id', 'gen', 'Mac');
});
afterEach(() => { setCompactLayoutForTest(false); showAllSidebarDevices(); resetStagedBackends(); });

for (const compact of [false, true]) it(`filters merged projects and search without changing the open thread or execution target (${compact ? 'phone' : 'desktop'})`, async () => {
  setCompactLayoutForTest(compact);
  const remote = stageBackend({ id: 'remote', backendId: 'remote-id', name: 'GPU' });
  listCatalogTreesForTest();
  for (const [id, backend] of [['local-project', ''], ['remote-project', 'remote']] as const) {
    noteProject(id, backend);
    addProjectLocal({ id, name: 'Shared project', repositoryID: 'github:github.com:1', path: `/repo/${id}`, createdAt: 0, updatedAt: 0, sortPosition: 0, archived: false });
  }
  const local = thread('Local pinned thread', 'local-project');
  const elsewhere = thread('Remote worktree thread', 'remote-project', 'remote-group');
  elsewhere.worktreePath = '/repo/worktrees/gpu';
  elsewhere.branch = 'gpu';
  noteThread(local.id, ''); noteThread(elsewhere.id, 'remote');
  replaceAllThreads([local, elsewhere]);
  upsertThreadGroup({ id: 'remote-group', projectId: 'remote-project', name: 'GPU work', createdAt: 0, updatedAt: 0 });
  const pane = createThreadPane(); pane.replaceThread(elsewhere);
  setSelectedBackend('remote');
  const view = render(ProjectsSection, { props: { pane } });
  await tick();
  expect(view.container.querySelectorAll('[data-testid="project-item"]')).toHaveLength(1);
  await fireEvent.click(view.getByRole('button', { name: 'Filter projects by computer' }));
  await fireEvent.keyDown(view.getByRole('menuitem', { name: 'All computers' }), { key: 'ArrowDown' });
  expect(document.activeElement).toBe(view.getByRole('menuitemcheckbox', { name: /^Mac/ }));
  const checkbox = view.getByRole('menuitemcheckbox', { name: /^GPU/ });
  expect(checkbox).toHaveAttribute('aria-checked', 'true');
  await fireEvent.click(checkbox); await tick();
  expect(checkbox).toHaveAttribute('aria-checked', 'false');
  expect(view.queryByText('Remote worktree thread')).not.toBeInTheDocument();
  expect(view.queryByText('GPU work')).not.toBeInTheDocument();
  expect(view.getByText('Local pinned thread')).toBeInTheDocument();
  expect(pane.thread?.id).toBe(elsewhere.id);
  expect(selectedBackend()).toBe('remote');
  expect(remote.reconnect).not.toHaveBeenCalled();
  setThreadFilterQuery('GPU work'); await tick();
  expect(view.queryByText('GPU work')).not.toBeInTheDocument();
  setThreadFilterQuery('');
  await fireEvent.click(view.getByRole('menuitemcheckbox', { name: /^Mac/ })); await tick();
  expect(view.getByText('No projects on the selected computers.')).toBeInTheDocument();
  expect(pane.thread?.id).toBe(elsewhere.id);
  await fireEvent.click(view.getByRole('menuitem', { name: 'All computers' })); await tick();
  expect(view.getByText('GPU work')).toBeInTheDocument();

  // Showing only the GPU's checkout does not move what the row acts on: an
  // action on it still executes on this computer's project.
  resetBindingMocks();
  const archive = setBindingMock('ArchiveProject', vi.fn(async () => undefined));
  if (view.queryByRole('menuitemcheckbox', { name: /^Mac/ }) === null) {
    await fireEvent.click(view.getByRole('button', { name: 'Filter projects by computer' }));
  }
  await fireEvent.click(view.getByRole('menuitemcheckbox', { name: /^Mac/ })); await tick();
  await fireEvent.keyDown(document.activeElement ?? document.body, { key: 'Escape' });
  expect(view.queryByText('Local pinned thread')).not.toBeInTheDocument();
  expect(view.getByTestId('project-item')).toHaveAttribute('data-project-id', 'local-project');
  await fireEvent.contextMenu(view.getByTestId('project-item'));
  await fireEvent.click(view.getByRole('menuitem', { name: 'Archive Project' }));
  expect(view.getByRole('dialog')).toHaveTextContent('Archive "Shared project" on Mac.');
  await fireEvent.click(view.getByRole('button', { name: 'Archive' }));
  await waitFor(() => expect(archive).toHaveBeenCalledWith('local-project'));
});

// The menu is where the sidebar says how each computer is: plain text in
// each row, and Retry on a load that failed for a reason other than reaching
// the computer. Nothing in the open menu moves.
for (const compact of [false, true]) it(`names each computer's state in its menu row, with Retry on a failed load (${compact ? 'phone' : 'desktop'})`, async () => {
  setCompactLayoutForTest(compact);
  vi.spyOn(console, 'error').mockImplementation(() => {});
  resetCatalogLoadForTest(); resetSidebarCatalogsForTest();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  const gpu = stageBackend({ id: 'remote', backendId: 'remote-id', name: 'GPU' });
  let gpuFails = true;
  setBindingMock('ListProjects', async () => {
    if (takePinnedBackend() === 'remote' && gpuFails) throw new TransportError('method_error', 'list projects: database disk image is malformed');
    return [];
  });
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListThreadGroups', async () => []);
  try {
    await loadSidebarCatalogs();
    const view = render(ProjectsSection, { props: { pane: null } });
    await tick();
    expect(view.queryByTestId('sidebar-catalog-status')).toBeNull();
    await fireEvent.click(view.getByRole('button', { name: 'Filter projects by computer' }));
    const mac = view.getByTestId('sidebar-device-local-id');
    // The row's element changes with its Retry, so each check finds it anew.
    const row = () => view.getByTestId('sidebar-device-remote-id');
    // Name, status, then the visible check.
    expect(normalizedTextContent(mac)).toBe('Mac Connected ✓');
    expect(normalizedTextContent(row())).toBe('GPU Database disk image is malformed. ✓ Retry');
    expect(row()).toHaveAttribute('title', 'Database disk image is malformed.');
    expect(within(mac).queryByRole('button')).toBeNull();

    gpuFails = false;
    await fireEvent.click(within(row()).getByRole('button', { name: 'Retry loading GPU' }));
    await waitFor(() => expect(normalizedTextContent(row())).toBe('GPU Connected ✓'));
    expect(within(row()).queryByRole('button')).toBeNull();

    const said: [TransportStatusSnapshot['status'], string][] = [
      ['reconnecting', 'GPU Offline ✓'], ['disconnected', 'GPU Connecting… ✓'], ['unauthorized', 'GPU Not connected ✓'], ['connected', 'GPU Connected ✓'],
    ];
    for (const [status, text] of said) {
      gpu.setStatus(status); await tick();
      expect(normalizedTextContent(row())).toBe(text);
      expect(within(row()).queryByRole('button')).toBeNull();
      expect(view.queryByTestId('sidebar-catalog-status')).toBeNull();
      expect(document.querySelector('[role="menu"] [class*="animate-"], [role="menu"] [aria-busy="true"]')).toBeNull();
    }
  } finally {
    resetCatalogLoadForTest(); resetSidebarCatalogsForTest(); resetBindingMocks(); vi.restoreAllMocks();
  }
});

for (const remote of [true, false]) it(`renders the Projects heading as ${remote ? 'a computer filter' : 'plain text in a build without remote access'}`, () => {
  if (!remote) stageNoRemoteBuild();
  const view = render(SidebarDeviceFilter);
  expect(view.queryByRole('button', { name: 'Filter projects by computer' }) !== null).toBe(remote);
  expect(view.getByText('Projects')).toBeInTheDocument();
});

for (const compact of [false, true]) it(`labels only visible projects across filter and archive transitions (${compact ? 'phone' : 'desktop'})`, async () => {
  setCompactLayoutForTest(compact);
  stageBackend({ id: 'remote', backendId: 'remote-id', name: 'GPU' });
  listCatalogTreesForTest();
  const local = { id: 'local', name: 'app', path: '/Users/randy/repos/app', repositoryID: 'github:github.com:1', createdAt: 0, updatedAt: 0, sortPosition: 0, archived: false };
  const cached = { ...local, id: 'cached', path: '/home/randy/repos/app', repositoryID: '', identityError: 'Repository identity has not been verified yet.' };
  noteProject(local.id, ''); noteProject(cached.id, 'remote');
  // The persisted filter is already active when an older offline catalog arrives.
  setSidebarDeviceVisible('remote-id', false);
  addProjectLocal(local); addProjectLocal(cached);
  const view = render(ProjectsSection, { pane: null });
  const labels = () => [...view.container.querySelectorAll('[data-testid="project-item-label"]')].map(el => el.textContent?.replace(/\s+/g, ''));
  await waitFor(() => expect(labels()).toEqual(['app']));
  await fireEvent.contextMenu(view.getByTestId('project-item'));
  await fireEvent.click(view.getByRole('menuitem', { name: 'Archive Project' }));
  expect(view.getByRole('dialog')).toHaveTextContent('Hide "app" from the sidebar.');
  await fireEvent.click(view.getByRole('button', { name: 'Cancel' }));
  for (let i = 0; i < 2; i++) {
    showAllSidebarDevices(); await tick();
    expect(labels().sort()).toEqual(['/Users/randy/repos/app', '/home/randy/repos/app'].sort());
    setSidebarDeviceVisible('remote-id', false); await tick();
    await waitFor(() => expect(labels()).toEqual(['app']));
  }
  showAllSidebarDevices();
  updateProjectLocal({ ...cached, archived: true }); await tick();
  await waitFor(() => expect(labels()).toEqual(['app']));
  updateProjectLocal(cached); await tick();
  expect(labels()).toHaveLength(2);
  setThreadFilterQuery('local-only');
  const kept = thread('local-only', local.id); noteThread(kept.id, ''); replaceAllThreads([kept]);
  await tick(); await waitFor(() => expect(labels()).toEqual(['app']));
  setThreadFilterQuery(''); await tick(); expect(labels()).toHaveLength(2);
  // The returning peer verifies its ID: two entries become one without a reload.
  updateProjectLocal({ ...cached, repositoryID: local.repositoryID, identityError: '' });
  await tick(); await waitFor(() => expect(labels()).toEqual(['app']));
  updateProjectLocal(cached);
  await tick(); expect(labels()).toHaveLength(2);
});

// A date-ordered sidebar ranks an entry by the members it SHOWS. News about a
// checkout the filter hides is not news about the row: a rename on it, or a
// streaming beat under it, must not move a row the user can see.
it('ranks an entry by the members it shows', async () => {
  stageBackend({ id: 'remote', backendId: 'remote-id', name: 'GPU' });
  listCatalogTreesForTest();
  const shown = { id: 'shown', name: 'app', path: '/repo/app', repositoryID: 'github:github.com:1', createdAt: 0, updatedAt: 100, sortPosition: 0, archived: false };
  const hidden = { ...shown, id: 'hidden', path: '/gpu/app' };
  const other = { id: 'other', name: 'tools', path: '/repo/tools', repositoryID: 'github:github.com:2', createdAt: 0, updatedAt: 9000, sortPosition: 0, archived: false };
  noteProject(shown.id, ''); noteProject(hidden.id, 'remote'); noteProject(other.id, '');
  addProjectLocal(shown); addProjectLocal(hidden); addProjectLocal(other);
  setSidebarDeviceVisible('remote-id', false);
  const view = render(ProjectsSection, { pane: null });
  const ids = () => [...view.container.querySelectorAll('[data-testid="project-item"]')]
    .map(el => el.getAttribute('data-project-id'));
  await waitFor(() => expect(ids()).toEqual(['other', 'shown']));

  // Renamed on the computer the filter hides: the row keeps its place.
  updateProjectLocal({ ...hidden, updatedAt: 20_000 });
  await tick();
  expect(ids()).toEqual(['other', 'shown']);
  // Streaming under it, likewise.
  touchProjectActivity(hidden.id, 30_000);
  await tick();
  expect(ids()).toEqual(['other', 'shown']);

  // The shown checkout's own rename does move it.
  updateProjectLocal({ ...shown, updatedAt: 20_000 });
  await tick();
  expect(ids()).toEqual(['shown', 'other']);
  // And a beat on a shown member outranks a date.
  touchProjectActivity(other.id, 50_000);
  await tick();
  expect(ids()).toEqual(['other', 'shown']);
});

// New projects all sit at position 0, so the manual order breaks ties by
// creation date. Both come from the row's target, so toggling the filter
// cannot reorder rows the user has not moved.
it('keeps tied rows of the manual order where they are through the filter, both ways', async () => {
  stageBackend({ id: 'remote', backendId: 'remote-id', name: 'GPU' });
  listCatalogTreesForTest();
  setProjectSortMode('manual');
  const project = (id: string, repo: string, createdAt: number) => ({ id, name: id, path: `/repo/${id}`, repositoryID: `github:github.com:${repo}`, createdAt, updatedAt: 0, sortPosition: 0, archived: false });
  for (const [row, backend] of [[project('a', '1', 30), ''], [project('a-gpu', '1', 10), 'remote'], [project('b', '2', 20), ''], [project('b-gpu', '2', 20), 'remote']] as const) {
    noteProject(row.id, backend);
    addProjectLocal(row);
  }
  const view = render(ProjectsSection, { pane: null });
  const ids = () => [...view.container.querySelectorAll('[data-testid="project-item"]')].map(el => el.getAttribute('data-project-id'));
  await waitFor(() => expect(ids()).toEqual(['b', 'a']));
  const mac = sidebarDevices().find((device) => device.name === 'Mac')!.id;
  for (let i = 0; i < 2; i++) {
    setSidebarDeviceVisible(mac, false); await tick();
    expect(ids()).toEqual(['b', 'a']);
    showAllSidebarDevices(); await tick();
    expect(ids()).toEqual(['b', 'a']);
  }
  setProjectSortMode('lastActivity');
});
