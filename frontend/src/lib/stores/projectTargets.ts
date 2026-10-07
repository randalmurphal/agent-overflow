// Last explicit target per frontend and repository. Persist computer UUIDs,
// never the empty HOME slot or mutable nicknames. Offline targets remain chosen.
import type { Project } from '../types/models';
import { getAttachedBackends } from './attachedBackends.svelte';
import { projectMembers, projectRepoKey } from './projects.svelte';
import { projectBackend } from '../transport/entityIndex';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { readFrontendValue, writeFrontendValue } from './frontendStorage';

const STORAGE_KEY = 'project-targets';
const MAX_REMEMBERED_PROJECTS = 512;

function readTargets(): Map<string, string> {
  const raw = readFrontendValue(STORAGE_KEY);
  if (!Array.isArray(raw)) return new Map();
  const rows = raw.slice(-MAX_REMEMBERED_PROJECTS).filter(
    (row): row is [string, string] => Array.isArray(row) && row.length === 2
      && row.every((value) => typeof value === 'string' && value.length > 0 && value.length <= 4096)
      && (row[0].startsWith('forge:') || row[0].startsWith('project:')),
  );
  if (rows.length !== raw.length) writeFrontendValue(STORAGE_KEY, rows);
  return new Map(rows);
}

function keyFor(project: Project): string {
  return projectRepoKey(project.id) || `project:${project.id}`;
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
  const targets = readTargets();
  if (targets.size === 0) return project;
  const members = projectMembers(project.id);
  const key = keyFor(project);
  let rememberedKey = targets.has(key) ? key : undefined;
  if (!rememberedKey) {
    rememberedKey = members.map(row => `project:${row.project.id}`).find(candidate => targets.has(candidate));
  }
  if (!rememberedKey) return project;
  const remembered = targets.get(rememberedKey)!;
  const computer = getAttachedBackends().find((entry) => entry.backendId === remembered);
  if (!computer) {
    // Forgetting a computer retires all its defaults; outages keep its entry.
    for (const [candidate, target] of targets) if (target === remembered) targets.delete(candidate);
    writeFrontendValue(STORAGE_KEY, [...targets]);
    return project;
  }
  const member = members.find((row) =>
    (projectBackend(row.project.id) ?? HOME_BACKEND) === computer.id);
  if (!member) {
    if (rememberedKey !== key) return project;
    throw new Error('The preferred computer has no available checkout for this project. Choose another computer.');
  }
  if (rememberedKey !== key) {
    targets.delete(rememberedKey);
    targets.set(key, remembered);
    writeFrontendValue(STORAGE_KEY, [...targets]);
  }
  return member.project;
}
