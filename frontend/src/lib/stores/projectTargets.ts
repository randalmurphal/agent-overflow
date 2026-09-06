// Last explicit target per frontend and repository. Persist computer UUIDs,
// never the empty HOME slot or mutable nicknames. Offline targets remain chosen.
import type { Project } from '../types/models';
import { repoKey } from '../utils/repoKey';
import { getAttachedBackends } from './attachedBackends.svelte';
import { projectMembers } from './projects.svelte';
import { projectBackend } from '../transport/entityIndex';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';

const STORAGE_KEY = 'project-targets';
const MAX_REMEMBERED_PROJECTS = 512;

function readTargets(): Map<string, string> {
  const raw = readFrontendValue(STORAGE_KEY);
  if (!Array.isArray(raw)) return new Map();
  return new Map(raw.slice(-MAX_REMEMBERED_PROJECTS).filter(
    (row): row is [string, string] => Array.isArray(row) && row.length === 2
      && row.every((value) => typeof value === 'string' && value.length > 0 && value.length <= 4096),
  ));
}

function keyFor(project: Project): string {
  return repoKey(project) || `project:${project.id}`;
}

export function rememberProjectTarget(project: Project, backend: BackendKey): void {
  const computer = getAttachedBackends().find((entry) => entry.id === backend);
  if (!computer?.backendId) return;
  const targets = readTargets();
  const key = keyFor(project);
  targets.delete(key);
  targets.set(key, computer.backendId);
  while (targets.size > MAX_REMEMBERED_PROJECTS) targets.delete(targets.keys().next().value!);
  writeFrontendValue(STORAGE_KEY, [...targets]);
}

export function preferredProjectTarget(project: Project): Project {
  const remembered = readTargets().get(keyFor(project));
  if (!remembered) return project;
  const computer = getAttachedBackends().find((entry) => entry.backendId === remembered);
  if (!computer) {
    // Forgetting a computer also retires its default. Temporary outages keep
    // the registry entry, so they never reach this branch or change a target.
    const targets = readTargets();
    targets.delete(keyFor(project));
    writeFrontendValue(STORAGE_KEY, [...targets]);
    return project;
  }
  const member = projectMembers(project.id).find((row) =>
    (projectBackend(row.project.id) ?? HOME_BACKEND) === computer.id);
  if (!member) throw new Error('The preferred computer has no available checkout for this project. Choose another computer.');
  return member.project;
}
