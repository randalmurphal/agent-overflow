// Sidebar UI state: which project rows are expanded, and which direction
// the projects list is sorted.
//
// Dual-write persistence: localStorage is the synchronous fast-path so
// module init has the correct value before the async Go settings load
// completes. Go settings (settings.json on disk) is the durable source
// of truth — localStorage is ephemeral on some webview platforms
// (WebKit2GTK / WSL2). On loadSettings(), syncSidebarFromSettings()
// overwrites the in-memory state from Go; on mutation, both layers are
// written through.
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

import type { ProjectSortMode } from '../types/settings';
import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from '../utils/sidebarThreadLimits';
import { getSettings, updateSettingsPatch } from './settings.svelte';

export type { ProjectSortMode };

const COLLAPSED_STORAGE_KEY = 'agent-overflow:sidebar:collapsedProjects';
const LEGACY_EXPANDED_STORAGE_KEY = 'agent-overflow:sidebar:expandedProjects';
const SORT_MODE_KEY = 'agent-overflow:sidebar:projectSortMode';
const EXPANDED_DISCUSSIONS_KEY = 'agent-overflow:sidebar:expandedDiscussions';
const LEGACY_EXPANDED_THREAD_LISTS_KEY = 'agent-overflow:sidebar:expandedThreadLists';
const THREAD_LIST_VISIBLE_LIMITS_KEY = 'agent-overflow:sidebar:threadListVisibleLimits';

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

function readThreadListVisibleLimits(): Record<string, number> {
  if (typeof localStorage === 'undefined') return {};
  try {
    localStorage.removeItem(LEGACY_EXPANDED_THREAD_LISTS_KEY);
    const raw = localStorage.getItem(THREAD_LIST_VISIBLE_LIMITS_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (parsed == null || typeof parsed !== 'object' || Array.isArray(parsed)) return {};

    const out: Record<string, number> = {};
    for (const [projectId, value] of Object.entries(parsed)) {
      if (typeof value !== 'number' || !Number.isFinite(value)) continue;
      if (value <= THREAD_PREVIEW_LIMIT) continue;
      out[projectId] = Math.floor(value);
    }
    return out;
  } catch {
    return {};
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

function writeThreadListVisibleLimits(limits: Record<string, number>): void {
  if (typeof localStorage === 'undefined') return;
  try {
    if (Object.keys(limits).length === 0) {
      localStorage.removeItem(THREAD_LIST_VISIBLE_LIMITS_KEY);
      return;
    }
    localStorage.setItem(THREAD_LIST_VISIBLE_LIMITS_KEY, JSON.stringify(limits));
  } catch {
    // Ignore quota / access errors — in-memory state stays consistent.
  }
}

let collapsedProjects: Set<string> = $state(readCollapsed());
let expandedDiscussions: Set<string> = $state(readStringSet(EXPANDED_DISCUSSIONS_KEY));
let threadListVisibleLimits: Record<string, number> = $state(readThreadListVisibleLimits());
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
  void updateSettingsPatch({ collapsedProjects: [...next] });
}

export function expandProject(id: string): void {
  if (!collapsedProjects.has(id)) return;
  const next = new Set(collapsedProjects);
  next.delete(id);
  collapsedProjects = next;
  writeCollapsed(next);
  void updateSettingsPatch({ collapsedProjects: [...next] });
}

export function collapseProject(id: string): void {
  if (collapsedProjects.has(id)) return;
  const next = new Set(collapsedProjects).add(id);
  collapsedProjects = next;
  writeCollapsed(next);
  void updateSettingsPatch({ collapsedProjects: [...next] });
}

export function getProjectSortMode(): ProjectSortMode {
  return projectSortMode;
}

export function setProjectSortMode(mode: ProjectSortMode): void {
  if (projectSortMode === mode) return;
  projectSortMode = mode;
  writeProjectSortMode(mode);
  void updateSettingsPatch({ projectSortMode: mode });
}

/**
 * Reconcile in-memory sidebar state with Go settings. Called after
 * loadSettings() completes. Go settings are the durable source of
 * truth, but on the first run after upgrade localStorage may hold
 * the user's real preferences while Go still has factory defaults.
 * In that case we push localStorage → Go (one-time migration)
 * instead of overwriting the user's state with defaults.
 *
 * Steady state (post-migration): every mutation writes through to
 * both layers, so they stay in sync and Go always wins here.
 */
export function syncSidebarFromSettings(): void {
  const s = getSettings();
  const migrationPatch: Partial<{ projectSortMode: ProjectSortMode; collapsedProjects: string[] }> =
    {};

  const goMode = PROJECT_SORT_MODES.includes(s.projectSortMode)
    ? s.projectSortMode
    : DEFAULT_PROJECT_SORT_MODE;
  if (goMode === DEFAULT_PROJECT_SORT_MODE && projectSortMode !== DEFAULT_PROJECT_SORT_MODE) {
    migrationPatch.projectSortMode = projectSortMode;
  } else if (projectSortMode !== goMode) {
    projectSortMode = goMode;
    writeProjectSortMode(goMode);
  }

  const goIds = s.collapsedProjects ?? [];
  const goSet = new Set(goIds.filter((id) => typeof id === 'string' && id !== ''));
  if (goSet.size === 0 && collapsedProjects.size > 0) {
    migrationPatch.collapsedProjects = [...collapsedProjects];
  } else if (!setsEqual(collapsedProjects, goSet)) {
    collapsedProjects = goSet;
    writeCollapsed(goSet);
  }

  if (Object.keys(migrationPatch).length > 0) {
    void updateSettingsPatch(migrationPatch);
  }
}

function setsEqual(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  if (a.size !== b.size) return false;
  for (const id of a) {
    if (!b.has(id)) return false;
  }
  return true;
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
  if (setsEqual(next, expandedDiscussions)) return;
  const copy = new Set(next);
  expandedDiscussions = copy;
  writeStringSet(EXPANDED_DISCUSSIONS_KEY, copy);
}

/**
 * Per-project thread preview size. Projects start at THREAD_PREVIEW_LIMIT;
 * each "Show more" click adds THREAD_REVEAL_INCREMENT, and "Show less"
 * drops the project back to the default preview.
 */
export function isThreadListExpanded(id: string): boolean {
  return getThreadListVisibleLimit(id) > THREAD_PREVIEW_LIMIT;
}

export function getThreadListVisibleLimit(id: string): number {
  return threadListVisibleLimits[id] ?? THREAD_PREVIEW_LIMIT;
}

export function revealMoreThreadList(id: string): void {
  const current = getThreadListVisibleLimit(id);
  const next = visibleLimitsWith(id, current + THREAD_REVEAL_INCREMENT);
  threadListVisibleLimits = next;
  writeThreadListVisibleLimits(next);
}

export function setThreadListVisibleLimit(id: string, limit: number): void {
  const roundedLimit = Math.floor(limit);
  if (!Number.isFinite(roundedLimit) || roundedLimit <= THREAD_PREVIEW_LIMIT) {
    collapseThreadList(id);
    return;
  }
  if (getThreadListVisibleLimit(id) === roundedLimit) return;
  const next = visibleLimitsWith(id, roundedLimit);
  threadListVisibleLimits = next;
  writeThreadListVisibleLimits(next);
}

export function collapseThreadList(id: string): void {
  if (!(id in threadListVisibleLimits)) return;
  const next = { ...threadListVisibleLimits };
  delete next[id];
  threadListVisibleLimits = next;
  writeThreadListVisibleLimits(next);
}

function visibleLimitsWith(id: string, limit: number): Record<string, number> {
  return {
    ...threadListVisibleLimits,
    [id]: limit,
  };
}

/** Test helper: clears in-memory + storage between tests. */
export function resetSidebarForTest(): void {
  collapsedProjects = new Set();
  expandedDiscussions = new Set();
  threadListVisibleLimits = {};
  projectSortMode = DEFAULT_PROJECT_SORT_MODE;
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(COLLAPSED_STORAGE_KEY);
      localStorage.removeItem(LEGACY_EXPANDED_STORAGE_KEY);
      localStorage.removeItem(EXPANDED_DISCUSSIONS_KEY);
      localStorage.removeItem(LEGACY_EXPANDED_THREAD_LISTS_KEY);
      localStorage.removeItem(THREAD_LIST_VISIBLE_LIMITS_KEY);
      localStorage.removeItem(SORT_MODE_KEY);
    } catch {
      // ignore
    }
  }
}
