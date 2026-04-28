// Sidebar UI state: which project rows are expanded, and which direction
// the projects list is sorted. Both persist to localStorage so reopening
// the app doesn't lose the user's reading context.
//
// Project expansion uses an inverted set: we persist explicit *collapses*
// rather than expansions, so an unseen project defaults to expanded
// (t3-code parity). The public API (isProjectExpanded / expandProject /
// collapseProject / toggleProject) keeps "expanded" semantics — only the
// underlying storage flips.
//
// We deliberately keep this separate from projects.svelte.ts — that store
// is about data, this one is about view state. They live together on
// screen but have different lifecycles (data refreshes; view state
// persists across refreshes).

const COLLAPSED_STORAGE_KEY = 'agent-overflow:sidebar:collapsedProjects';
const LEGACY_EXPANDED_STORAGE_KEY = 'agent-overflow:sidebar:expandedProjects';
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

function readCollapsed(): Set<string> {
  // Drop the legacy "expanded" key on first read so old data doesn't
  // linger forever. Old persisted state can't be migrated meaningfully:
  // the old set listed user-expanded ids, but we'd need the full project
  // list to compute the inverse — and that list isn't loaded here.
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(LEGACY_EXPANDED_STORAGE_KEY);
    } catch {
      // ignore
    }
  }
  return readStringSet(COLLAPSED_STORAGE_KEY);
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

function writeCollapsed(set: Set<string>): void {
  writeStringSet(COLLAPSED_STORAGE_KEY, set);
}

function writeProjectSortMode(mode: ProjectSortMode): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(SORT_MODE_KEY, mode);
  } catch {
    // ignore
  }
}

let collapsedProjects: Set<string> = $state(readCollapsed());
let expandedDiscussions: Set<string> = $state(readStringSet(EXPANDED_DISCUSSIONS_KEY));
let expandedThreadLists: Set<string> = $state(readStringSet(EXPANDED_THREAD_LISTS_KEY));
let projectSortMode: ProjectSortMode = $state(readProjectSortMode());

export function isProjectExpanded(id: string): boolean {
  return !collapsedProjects.has(id);
}

export function toggleProject(id: string): void {
  const next = new Set(collapsedProjects);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  collapsedProjects = next;
  writeCollapsed(next);
}

export function expandProject(id: string): void {
  if (!collapsedProjects.has(id)) return;
  const next = new Set(collapsedProjects);
  next.delete(id);
  collapsedProjects = next;
  writeCollapsed(next);
}

export function collapseProject(id: string): void {
  if (collapsedProjects.has(id)) return;
  const next = new Set(collapsedProjects).add(id);
  collapsedProjects = next;
  writeCollapsed(next);
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
 * user's reading context. Distinct from project expansion because a
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
  collapsedProjects = new Set();
  expandedDiscussions = new Set();
  expandedThreadLists = new Set();
  projectSortMode = DEFAULT_PROJECT_SORT_MODE;
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(COLLAPSED_STORAGE_KEY);
      localStorage.removeItem(LEGACY_EXPANDED_STORAGE_KEY);
      localStorage.removeItem(EXPANDED_DISCUSSIONS_KEY);
      localStorage.removeItem(EXPANDED_THREAD_LISTS_KEY);
      localStorage.removeItem(SORT_MODE_KEY);
    } catch {
      // ignore
    }
  }
}
