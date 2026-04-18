// Sidebar UI state: which project rows are expanded, and which direction
// the projects list is sorted. Both persist to localStorage so reopening
// the app doesn't lose the user's reading context.
//
// We deliberately keep this separate from projects.svelte.ts — that store
// is about data, this one is about view state. They live together on
// screen but have different lifecycles (data refreshes; view state
// persists across refreshes).

import { getThreads } from './threads.svelte';
import { getProjects } from './projects.svelte';

const EXPANDED_STORAGE_KEY = 'agent-overflow:sidebar:expandedProjects';
const SORT_STORAGE_KEY = 'agent-overflow:sidebar:sortDirection';

export type SortDirection = 'asc' | 'desc';

function readExpanded(): Set<string> {
  if (typeof localStorage === 'undefined') return new Set();
  try {
    const raw = localStorage.getItem(EXPANDED_STORAGE_KEY);
    if (!raw) return new Set();
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return new Set();
    const out = new Set<string>();
    for (const entry of parsed) {
      if (typeof entry === 'string') out.add(entry);
    }
    return out;
  } catch {
    // Corrupt JSON — treat as empty and let the next write overwrite it.
    return new Set();
  }
}

function readSortDirection(): SortDirection {
  if (typeof localStorage === 'undefined') return 'desc';
  try {
    const raw = localStorage.getItem(SORT_STORAGE_KEY);
    return raw === 'asc' ? 'asc' : 'desc';
  } catch {
    return 'desc';
  }
}

function writeExpanded(set: Set<string>): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(EXPANDED_STORAGE_KEY, JSON.stringify([...set]));
  } catch {
    // Ignore quota / access errors — in-memory state stays consistent.
  }
}

function writeSortDirection(direction: SortDirection): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(SORT_STORAGE_KEY, direction);
  } catch {
    // Same rationale as writeExpanded: best-effort persistence.
  }
}

let expandedProjects: Set<string> = $state(readExpanded());
let sortDirection: SortDirection = $state(readSortDirection());

export function isProjectExpanded(id: string): boolean {
  return expandedProjects.has(id);
}

export function toggleProject(id: string): void {
  const next = new Set(expandedProjects);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  expandedProjects = next;
  writeExpanded(next);
}

export function expandProject(id: string): void {
  if (expandedProjects.has(id)) return;
  const next = new Set(expandedProjects).add(id);
  expandedProjects = next;
  writeExpanded(next);
}

export function collapseProject(id: string): void {
  if (!expandedProjects.has(id)) return;
  const next = new Set(expandedProjects);
  next.delete(id);
  expandedProjects = next;
  writeExpanded(next);
}

/**
 * Ensure the project containing the active thread is expanded. Additive
 * only — we never auto-collapse on thread change. Called from the sidebar
 * whenever the active thread changes so the user always sees their
 * current thread in context.
 */
export function expandProjectsForActiveThread(threadId: string | null): void {
  if (!threadId) return;
  const thread = getThreads().find((t) => t.id === threadId);
  // The Thread shape carries a projectId once Wave 3 backend changes land.
  const projectId = (thread as { projectId?: string } | undefined)?.projectId;
  if (!projectId) return;
  if (getProjects().some((p) => p.project.id === projectId)) {
    expandProject(projectId);
  }
}

export function getSortDirection(): SortDirection {
  return sortDirection;
}

export function toggleSortDirection(): void {
  sortDirection = sortDirection === 'asc' ? 'desc' : 'asc';
  writeSortDirection(sortDirection);
}

/** Test helper: clears in-memory + storage between tests. */
export function resetSidebarForTest(): void {
  expandedProjects = new Set();
  sortDirection = 'desc';
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(EXPANDED_STORAGE_KEY);
      localStorage.removeItem(SORT_STORAGE_KEY);
    } catch {
      // ignore
    }
  }
}
