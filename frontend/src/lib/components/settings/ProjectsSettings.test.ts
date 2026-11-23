import { readSidebarCatalogs } from '../../../test/helpers/sidebarCatalogs';
import { beforeEach, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ProjectsSettings from './ProjectsSettings.svelte';
import { stageBackend } from '../../../test/helpers/backends';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal, updateProjectLocal } from '../../stores/projects.svelte';
import type { Project } from '../../types/models';

beforeEach(async () => {
  setBindingMock('ListProjects', async () => []);
  await readSidebarCatalogs();
  setBindingMock('GetProjectWorktreeSetup', async () => ({ copy: [], run: [], timeout: '' }));
});

it('labels only the selected computer’s projects and updates after a rename', async () => {
  stageBackend({ id: 'remote', name: 'Remote' });
  const local: Project = { id: 'local', name: 'app', path: '/Users/randy/repos/app', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false };
  noteProject(local.id, '');
  addProjectLocal(local);
  noteProject('remote', 'remote');
  addProjectLocal({ ...local, id: 'remote', path: '/home/randy/repos/app' });
  const view = render(ProjectsSettings);
  const options = () => [...view.getByTestId('settings-projects-select').querySelectorAll('option')].map(el => el.textContent).sort();
  await waitFor(() => expect(options()).toEqual(['app']));
  const other = { ...local, id: 'other', path: '/work/repos/app' };
  noteProject(other.id, '');
  addProjectLocal(other);
  await waitFor(() => expect(options()).toEqual(['/work/repos/app', 'randy/repos/app']));
  updateProjectLocal({ ...other, name: 'other' });
  await waitFor(() => expect(options()).toEqual(['app', 'other']));
});

// The page reads no catalog of its own: the tree's one round
// (stores/sidebarCatalogs.ts) owns the project rows, so a read here would
// supersede that round's and leave its tree held.
it('reads no catalog when it mounts', async () => {
  const reads = (['ListProjects', 'ListThreads', 'ListThreadGroups'] as const)
    .map((name) => setBindingMock(name, async () => []));
  noteProject('local', '');
  addProjectLocal({ id: 'local', name: 'app', path: '/repos/app', createdAt: 1, updatedAt: 1, sortPosition: 0, archived: false });
  const view = render(ProjectsSettings);
  await waitFor(() => expect(view.getByTestId('settings-projects-select')).toBeInTheDocument());
  expect(reads.map((mock) => mock.mock.calls.length)).toEqual([0, 0, 0]);
  expect([...view.getByTestId('settings-projects-select').querySelectorAll('option')].map((el) => el.textContent))
    .toEqual(['app']);
});
