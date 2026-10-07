// Path-based project operations capture their computer before any await.
// Two hosts may have exactly the same path; neither browse nor duplicate
// detection may infer ownership from that path or the currently focused pane.
import { BrowseDirectory, CreateProject, InspectProjectFolder } from './bindings';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { noteProject, projectBackend } from '../transport/entityIndex';
import { addProjectLocal, checkoutMatchesProject, getProject, getProjects } from './projects.svelte';
import { hasRepoIdentity, normalizeRemoteURL } from '../utils/repoKey';
import type { Project } from '../types/models';

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

/**
 * Why the folder at `path` on `backend` cannot stand in for `projectId`
 * there, or null when it can. A project with a repository identity accepts
 * only a checkout of that repository, judged by the rule sidebar entries
 * merge on; a project with none (a plain directory) accepts any folder,
 * which becomes its own entry. A git failure reading the folder refuses it
 * with git's reason rather than treating it as a plain folder.
 */
export async function checkoutRefusal(projectId: string, backend: BackendKey, path: string): Promise<string | null> {
  const project = getProject(projectId)?.project;
  if (!project || !hasRepoIdentity(project)) return null;
  let folder;
  try {
    folder = await withBackendTarget(backend, () => InspectProjectFolder(path));
  } catch (err) {
    return `Git couldn't read that folder: ${err instanceof Error ? err.message : String(err)}`;
  }
  const expected = repositoryName(project);
  if (!folder.repository) return `That folder isn't a git repository, so it can't be ${expected}.`;
  if (checkoutMatchesProject(projectId, backend, folder)) return null;
  const actual = normalizeRemoteURL(folder.remoteURL ?? '');
  return actual === ''
    ? `That folder isn't a checkout of ${expected}.`
    : `That folder is a checkout of ${actual}, not ${expected}.`;
}

function repositoryName(project: Project): string {
  return normalizeRemoteURL(project.remoteURL ?? '') || project.name;
}
