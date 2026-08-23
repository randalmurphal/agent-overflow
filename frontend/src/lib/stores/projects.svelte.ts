// Sidebar-facing projects store. Mirrors the pattern of threads.svelte.ts:
// a single reactive $state array driven by an explicit refresh, with
// optimistic local mutations so the sidebar can reflect a create/rename/
// delete without round-tripping the server.
//
// Callers (Sidebar components, AddProjectModal) refresh once on mount
// and call the addProjectLocal / updateProjectLocal / removeProjectLocal
// helpers after a successful RPC so the list stays in sync with the
// backend. `refreshProjects` is still safe to call any time to resync.

import type { Project, ProjectWithCounts } from '../types/models';
import { ListProjects } from './bindings';
import { addToast } from './toast.svelte';
import {
  disambiguatedProjectLabels,
  formatProjectLabel,
  type ProjectLabel,
} from '../utils/pathDisplay';

let projects: ProjectWithCounts[] = $state([]);
let loaded = $state(false);

/** Read-only view of the current project list for consumers. */
export function getProjects(): readonly ProjectWithCounts[] {
  return projects;
}

/** Lookup helper. Returns undefined when the id isn't in the store. */
export function getProject(id: string): ProjectWithCounts | undefined {
  return projects.find((p) => p.project.id === id);
}

// Duplicate project names are legal (only paths are unique), so every
// name-rendering surface reads its label here: unique names label as-is,
// duplicates gain the minimal parent-dir prefix that tells them apart.
// One shared $derived so the map is computed once per list change.
const projectLabels = $derived(
  disambiguatedProjectLabels(projects.map((p) => p.project)),
);

/** Structured display label for a project (prefix + name). Undefined when
 *  the id isn't in the store. */
export function getProjectLabel(id: string): ProjectLabel | undefined {
  return projectLabels.get(id);
}

/** Flat display-label string (`prefix/name` when disambiguated). Falls
 *  back to the empty string for unknown ids — callers with a "(deleted)"
 *  style placeholder should check getProjectLabel themselves. */
export function getProjectLabelText(id: string): string {
  const label = projectLabels.get(id);
  return label ? formatProjectLabel(label) : '';
}

/** True once refreshProjects has completed at least one successful fetch. */
export function isLoaded(): boolean {
  return loaded;
}

/**
 * Pull the authoritative list from the backend. Resolves once the store
 * has been populated (or left unchanged on failure — previous data is
 * preserved so the sidebar doesn't blank out on a transient error).
 */
export async function refreshProjects(): Promise<void> {
  try {
    const result = (await ListProjects()) as ProjectWithCounts[] | null;
    projects = Array.isArray(result) ? result : [];
    loaded = true;
  } catch (err) {
    console.error('Failed to load projects:', err);
    addToast('error', 'Failed to load projects');
  }
}

/**
 * Insert a freshly-created project at the head of the list. Accepts a
 * bare Project (the CreateProject binding returns one without counts) and
 * wraps it as ProjectWithCounts with zero counts — the next refresh will
 * reconcile actual totals.
 */
export function addProjectLocal(p: Project): void {
  // Prevent duplicate inserts if the caller fires this and a refresh
  // races. Refresh wins because it carries thread counts.
  if (projects.some((existing) => existing.project.id === p.id)) return;
  const wrapped: ProjectWithCounts = {
    project: p,
    threadCount: 0,
    lastActive: 0,
  };
  projects = [wrapped, ...projects];
}

/**
 * Replace a project's row after rename / archive / unarchive. Preserves
 * the existing threadCount + lastActive so the sidebar doesn't flicker
 * back to 0 threads until the next refresh.
 */
export function updateProjectLocal(p: Project): void {
  projects = projects.map((existing) =>
    existing.project.id === p.id
      ? { ...existing, project: p }
      : existing,
  );
}

/**
 * Bump a project's activity projection when one of its threads receives
 * newer live activity. Mirrors the backend's ListProjects lastActive value
 * without refetching the whole projects list for every streamed item.
 */
export function touchProjectActivity(projectId: string | undefined, updatedAt: number): void {
  if (!projectId || !Number.isFinite(updatedAt)) return;
  const index = projects.findIndex((p) => p.project.id === projectId);
  if (index === -1) return;

  const existing = projects[index];
  if ((existing.lastActive ?? 0) >= updatedAt) return;

  const next = [...projects];
  next[index] = { ...existing, lastActive: updatedAt };
  projects = next;
}

/** Drop a project row and any related thread counts. */
export function removeProjectLocal(id: string): void {
  projects = projects.filter((p) => p.project.id !== id);
}

/** Test helper — clears state between tests. */
export function resetProjectsForTest(): void {
  projects = [];
  loaded = false;
}
