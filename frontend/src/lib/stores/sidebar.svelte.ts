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
const SORT_MODE_KEY = 'agent-overflow:sidebar:projectSortMode';
const EXPANDED_DISCUSSIONS_KEY = 'agent-overflow:sidebar:expandedDiscussions';
const EXPANDED_THREAD_LISTS_KEY = 'agent-overflow:sidebar:expandedThreadLists';

/**
 * Project sort mode. Three discrete strategies with no per-mode tuning
 * (forge / t3-code parity). Mode persists across sessions; manual order
 * lives in `Project.sortPosition` and is set by the DnD reorder handler.
 */
export type ProjectSortMode = 'lastActivity' | 'createdAt' | 'manual';

const DEFAULT_PROJECT_SORT_MODE: ProjectSortMode = 'lastActivity';

const PROJECT_SORT_MODES: readonly ProjectSortMode[] = [
  'lastActivity',
  'createdAt',
  'manual',
];

function readStringSet(key: string): Set<string> {
  if (typeof localStorage === 'undefined') return new Set();
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return new Set();
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return new Set();
    const out = new Set<string>();
    for (const entry of parsed) {
      if (typeof entry === 'string') out.add(entry);
    }
    return out;
  } catch {
    return new Set();
  }
}

function readExpanded(): Set<string> {
  return readStringSet(EXPANDED_STORAGE_KEY);
}

function readProjectSortMode(): ProjectSortMode {
  if (typeof localStorage === 'undefined') return DEFAULT_PROJECT_SORT_MODE;
  try {
    const raw = localStorage.getItem(SORT_MODE_KEY);
    if (raw && PROJECT_SORT_MODES.includes(raw as ProjectSortMode)) {
      return raw as ProjectSortMode;
    }
    return DEFAULT_PROJECT_SORT_MODE;
  } catch {
    return DEFAULT_PROJECT_SORT_MODE;
  }
}

function writeStringSet(key: string, set: Set<string>): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(key, JSON.stringify([...set]));
  } catch {
    // Ignore quota / access errors — in-memory state stays consistent.
  }
}

function writeExpanded(set: Set<string>): void {
  writeStringSet(EXPANDED_STORAGE_KEY, set);
}

function writeProjectSortMode(mode: ProjectSortMode): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(SORT_MODE_KEY, mode);
  } catch {
    // ignore
  }
}

let expandedProjects: Set<string> = $state(readExpanded());
let expandedDiscussions: Set<string> = $state(readStringSet(EXPANDED_DISCUSSIONS_KEY));
let expandedThreadLists: Set<string> = $state(readStringSet(EXPANDED_THREAD_LISTS_KEY));
let projectSortMode: ProjectSortMode = $state(readProjectSortMode());

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

export function getProjectSortMode(): ProjectSortMode {
  return projectSortMode;
}

export function setProjectSortMode(mode: ProjectSortMode): void {
  if (projectSortMode === mode) return;
  projectSortMode = mode;
  writeProjectSortMode(mode);
}

/**
 * Discussion-tree expansion state — a flat set of thread ids whose
 * children should be visible. Persisted so reopening the app keeps the
 * user's reading context. Distinct from `expandedProjects` because a
 * collapsed project hides its discussions wholesale; this controls
 * which discussion *roots* show their participants when their parent
 * project is open.
 */
export function isDiscussionExpanded(id: string): boolean {
  return expandedDiscussions.has(id);
}

export function getExpandedDiscussions(): ReadonlySet<string> {
  return expandedDiscussions;
}

export function toggleDiscussion(id: string): void {
  const next = new Set(expandedDiscussions);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  expandedDiscussions = next;
  writeStringSet(EXPANDED_DISCUSSIONS_KEY, next);
}

/**
 * Replace the entire expanded-discussions set. Used by the auto-expand
 * effect that keeps an active thread's ancestors visible — we compute
 * the next set from the tree and swap it in atomically.
 */
export function setExpandedDiscussions(next: ReadonlySet<string>): void {
  // Cheap equality check so we don't write storage on every keystroke.
  if (next.size === expandedDiscussions.size) {
    let same = true;
    for (const id of next) {
      if (!expandedDiscussions.has(id)) {
        same = false;
        break;
      }
    }
    if (same) return;
  }
  const copy = new Set(next);
  expandedDiscussions = copy;
  writeStringSet(EXPANDED_DISCUSSIONS_KEY, copy);
}

/**
 * Per-project "show all threads" state. When a project's id is in this
 * set, ProjectThreadList renders the full sorted list; otherwise it
 * truncates at THREAD_PREVIEW_LIMIT (with the active thread always
 * pinned in). Persisted so reopening keeps the user's chosen view.
 */
export function isThreadListExpanded(id: string): boolean {
  return expandedThreadLists.has(id);
}

export function expandThreadList(id: string): void {
  if (expandedThreadLists.has(id)) return;
  const next = new Set(expandedThreadLists).add(id);
  expandedThreadLists = next;
  writeStringSet(EXPANDED_THREAD_LISTS_KEY, next);
}

export function collapseThreadList(id: string): void {
  if (!expandedThreadLists.has(id)) return;
  const next = new Set(expandedThreadLists);
  next.delete(id);
  expandedThreadLists = next;
  writeStringSet(EXPANDED_THREAD_LISTS_KEY, next);
}

/** Test helper: clears in-memory + storage between tests. */
export function resetSidebarForTest(): void {
  expandedProjects = new Set();
  expandedDiscussions = new Set();
  expandedThreadLists = new Set();
  projectSortMode = DEFAULT_PROJECT_SORT_MODE;
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(EXPANDED_STORAGE_KEY);
      localStorage.removeItem(EXPANDED_DISCUSSIONS_KEY);
      localStorage.removeItem(EXPANDED_THREAD_LISTS_KEY);
      localStorage.removeItem(SORT_MODE_KEY);
    } catch {
      // ignore
    }
  }
}
