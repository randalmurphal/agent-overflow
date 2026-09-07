import { beforeEach, afterEach, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import ProjectsSection from './ProjectsSection.svelte';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { setBackendIdentityFromBootstrap } from '../../transport/backendIdentity';
import { noteProject, noteThread } from '../../transport/entityIndex';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { replaceAllThreads } from '../../stores/threads.svelte';
import { upsertThreadGroup, resetThreadGroupsForTest } from '../../stores/threadGroups.svelte';
import { showAllSidebarDevices } from '../../stores/sidebarDevices.svelte';
import { setThreadFilterQuery } from '../../stores/threadFilter.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { selectedBackend, setSelectedBackend } from '../../stores/selectedBackend.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { Thread } from '../../types/models';

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
  for (const [id, backend] of [['local-project', ''], ['remote-project', 'remote']] as const) {
    noteProject(id, backend);
    addProjectLocal({ id, name: 'Shared project', remoteURL: 'https://github.com/example/shared.git', path: `/repo/${id}`, createdAt: 0, updatedAt: 0, sortPosition: 0, archived: false });
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
  await fireEvent.click(view.getByRole('button', { name: 'Filter projects by device' }));
  await fireEvent.keyDown(view.getByRole('menuitem', { name: 'All devices' }), { key: 'ArrowDown' });
  expect(document.activeElement).toBe(view.getByRole('menuitemcheckbox', { name: 'Mac' }));
  const checkbox = view.getByRole('menuitemcheckbox', { name: 'GPU' });
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
  await fireEvent.click(view.getByRole('menuitemcheckbox', { name: 'Mac' })); await tick();
  expect(view.getByText('No projects on the selected devices.')).toBeInTheDocument();
  expect(pane.thread?.id).toBe(elsewhere.id);
  await fireEvent.click(view.getByRole('menuitem', { name: 'All devices' })); await tick();
  expect(view.getByText('GPU work')).toBeInTheDocument();
});
