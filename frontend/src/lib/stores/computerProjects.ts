// Path-based project operations capture their computer before any await.
// Two hosts may have exactly the same path; neither browse nor duplicate
// detection may infer ownership from that path or the currently focused pane.
import { BrowseDirectory, CreateProject } from './bindings';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { noteProject, projectBackend } from '../transport/entityIndex';
import { addProjectLocal, getProjects } from './projects.svelte';

export function browseComputerDirectory(backend: BackendKey, path: string) {
  return withBackendTarget(backend, () => BrowseDirectory(path));
}

export async function addComputerProject(backend: BackendKey, path: string) {
  const project = await withBackendTarget(backend, () => CreateProject(path));
  noteProject(project.id, backend);
  addProjectLocal(project);
  return project;
}

export function projectAtComputerPath(backend: BackendKey, path: string) {
  return getProjects().find((row) => row.project.path === path
    && (projectBackend(row.project.id) ?? HOME_BACKEND) === backend)?.project;
}
