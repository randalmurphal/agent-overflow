// A thread change rebuilds the sidebar tree of its own project only.
import { beforeEach, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';

import ProjectsSection from '../ProjectsSection.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import { resetSidebarForTest } from '../../../stores/sidebar.svelte';
import { refreshProjects, resetProjectsForTest } from '../../../stores/projects.svelte';
import { refreshThreads, updateThreadTitle } from '../../../stores/threads.svelte';
import { resetPanesForTest } from '../../../stores/panes.svelte';
import { resetPaneLayoutForTest } from '../../../stores/paneLayout.svelte';
import { resetThreadGroupsForTest } from '../../../stores/threadGroups.svelte';
import { setThreadFilterQuery } from '../../../stores/threadFilter.svelte';
import { buildSidebarThreadTree } from '../../../utils/sidebarTree';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import type { Project, Thread } from '../../../types/models';

vi.mock('../../../utils/sidebarTree', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../utils/sidebarTree')>();
  return { ...actual, buildSidebarThreadTree: vi.fn(actual.buildSidebarThreadTree) };
});

const project = (id: string): Project => ({
  id, path: `/tmp/${id}`, name: id, sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false,
});
const thread = (id: string, projectId: string): Thread => ({
  id, projectId, title: id, provider: 'claude', workspacePath: `/tmp/${projectId}`, projectPath: `/tmp/${projectId}`,
  mode: 'chat', model: 'm', createdAt: 1, updatedAt: 1, archived: false,
});

beforeEach(async () => {
  resetSidebarForTest();
  resetProjectsForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetThreadGroupsForTest();
  setThreadFilterQuery('');
  setBindingMock('ListProjects', async () => [
    { project: project('alpha'), threadCount: 2 },
    { project: project('beta'), threadCount: 2 },
  ]);
  setBindingMock('ListThreads', async () => [
    thread('a1', 'alpha'), thread('a2', 'alpha'), thread('b1', 'beta'), thread('b2', 'beta'),
  ]);
  await refreshThreads();
  await refreshProjects();
});

it('rebuilds the tree of the project whose thread changed and of no other', async () => {
  const view = render(ProjectsSection, { props: { pane: createThreadPane() } });
  await tick();
  const built = () => vi.mocked(buildSidebarThreadTree).mock.calls.map(([input]) => input.threads.map((row) => row.id).join(','));
  expect(built()).toEqual(expect.arrayContaining(['a1,a2', 'b1,b2']));
  vi.mocked(buildSidebarThreadTree).mockClear();

  updateThreadTitle('a1', 'Renamed');
  await tick();
  expect(view.getByText('Renamed')).toBeInTheDocument();
  expect(built()).toContain('a1,a2');
  expect(built()).not.toContain('b1,b2');
});
