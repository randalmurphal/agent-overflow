import { afterEach, beforeEach, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import ProjectsSettings from './ProjectsSettings.svelte';
import { stageBackend, resetStagedBackends } from '../../../test/helpers/backends';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { noteProject } from '../../transport/entityIndex';
import { addProjectLocal, refreshProjects, resetProjectsForTest, updateProjectLocal } from '../../stores/projects.svelte';
import type { Project } from '../../types/models';

beforeEach(async () => {
  resetStagedBackends();
  resetProjectsForTest();
  setBindingMock('ListProjects', async () => []);
  await refreshProjects();
  setBindingMock('GetProjectWorktreeSetup', async () => ({ copy: [], run: [], timeout: '' }));
});

afterEach(() => { resetStagedBackends(); resetProjectsForTest(); });

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
