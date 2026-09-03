// Sidebar UI state: which project rows are expanded, and which direction
// the projects list is sorted.
//
// Two persistence layers, split by what kind of state each is:
//   - View state (collapsed projects, expanded discussions, thread-list
//     limits) persists per client through appStorage
//     (ui_state table) — two machines looking at the same backend keep
//     independent view state. appStorage serves module init from its
//     same-session cache; syncSidebarFromAppStorage() re-reads after
//     hydration lands the durable bucket.
//   - projectSortMode is a user preference, not view state — it stays
//     in Go settings (with localStorage as the pre-load cache) so it
//     follows the user, and syncSidebarFromSettings() reconciles it
//     after loadSettings(). It is a USER-tier settings key
//     (internal/settings/tier.go), so every screen this person opens
//     reads the same order; a per-screen answer is what the device tier
//     is for, and this is not one.
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
import {
  appStorageAdoptLegacyKey,
  appStorageDelete,
  appStorageGet,
  appStorageSet,
} from './appStorage';
import { getSettings, updateSettingsPatch } from './settings.svelte';

export type { ProjectSortMode };

// appStorage (per-client) keys.
const COLLAPSED_KEY = 'sidebar:collapsedProjects';
const EXPANDED_DISCUSSIONS_KEY = 'sidebar:expandedDiscussions';
const THREAD_LIST_VISIBLE_LIMITS_KEY = 'sidebar:threadListVisibleLimits';
// Groups default to EXPANDED, so the persisted set holds the COLLAPSED
// ids — the same inversion collapsedProjects uses, for the same reason:
// a group the user has never touched must show its members. No legacy
// key: the feature never existed before appStorage.
const COLLAPSED_GROUPS_KEY = 'sidebar:collapsedGroups';

// Pre-appStorage localStorage keys, adopted once at module init.
const LEGACY_COLLAPSED_STORAGE_KEY = 'agent-overflow:sidebar:collapsedProjects';
const LEGACY_EXPANDED_STORAGE_KEY = 'agent-overflow:sidebar:expandedProjects';
const LEGACY_EXPANDED_DISCUSSIONS_KEY = 'agent-overflow:sidebar:expandedDiscussions';
const LEGACY_EXPANDED_THREAD_LISTS_KEY = 'agent-overflow:sidebar:expandedThreadLists';
const LEGACY_THREAD_LIST_LIMITS_KEY = 'agent-overflow:sidebar:threadListVisibleLimits';

// projectSortMode cache key (Go settings remain the durable copy).
const SORT_MODE_KEY = 'agent-overflow:sidebar:projectSortMode';

const DEFAULT_PROJECT_SORT_MODE: ProjectSortMode = 'lastActivity';

const PROJECT_SORT_MODES: readonly ProjectSortMode[] = [
  'lastActivity',
  'createdAt',
  'manual',
];

/** Parses a persisted JSON string[] value; null on any malformed shape. */
function parseStringArray(raw: string): string[] | null {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return null;
    return parsed.filter((entry): entry is string => typeof entry === 'string');
  } catch {
    return null;
  }
}

function stringSetFromStorage(key: string, legacyKey?: string): Set<string> {
  const raw =
    appStorageGet(key) ??
    (legacyKey === undefined
      ? null
      : appStorageAdoptLegacyKey(key, legacyKey, (legacy) =>
          parseStringArray(legacy) === null ? null : legacy,
        ));
  if (raw === null) return new Set();
  return new Set(parseStringArray(raw) ?? []);
}

function writeStringSet(key: string, set: ReadonlySet<string>): void {
  appStorageSet(key, JSON.stringify([...set]));
}

function readCollapsed(): Set<string> {
  // Drop the pre-inversion legacy "expanded" key so old data doesn't
  // linger forever. Its contents can't be migrated meaningfully: the
  // old set listed user-expanded ids, and computing the inverse needs
  // the full project list, which isn't loaded here.
  removeLocalStorageKey(LEGACY_EXPANDED_STORAGE_KEY);
  return stringSetFromStorage(COLLAPSED_KEY, LEGACY_COLLAPSED_STORAGE_KEY);
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

function parseThreadListVisibleLimits(raw: string): Record<string, number> | null {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed == null || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
    const out: Record<string, number> = {};
    for (const [projectId, value] of Object.entries(parsed)) {
      if (typeof value !== 'number' || !Number.isFinite(value)) continue;
      if (value <= THREAD_PREVIEW_LIMIT) continue;
      out[projectId] = Math.floor(value);
    }
    return out;
  } catch {
    return null;
  }
}

function readThreadListVisibleLimits(): Record<string, number> {
  removeLocalStorageKey(LEGACY_EXPANDED_THREAD_LISTS_KEY);
  const raw =
    appStorageGet(THREAD_LIST_VISIBLE_LIMITS_KEY) ??
    appStorageAdoptLegacyKey(THREAD_LIST_VISIBLE_LIMITS_KEY, LEGACY_THREAD_LIST_LIMITS_KEY, (legacy) =>
      parseThreadListVisibleLimits(legacy) === null ? null : legacy,
    );
  if (raw === null) return {};
  return parseThreadListVisibleLimits(raw) ?? {};
}

function writeThreadListVisibleLimits(limits: Record<string, number>): void {
  if (Object.keys(limits).length === 0) {
    appStorageDelete(THREAD_LIST_VISIBLE_LIMITS_KEY);
    return;
  }
  appStorageSet(THREAD_LIST_VISIBLE_LIMITS_KEY, JSON.stringify(limits));
}

function writeProjectSortMode(mode: ProjectSortMode): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(SORT_MODE_KEY, mode);
  } catch {
    // ignore
  }
}

function removeLocalStorageKey(key: string): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(key);
  } catch {
    // ignore
  }
}

let collapsedProjects: Set<string> = $state(readCollapsed());
let expandedDiscussions: Set<string> = $state(
  stringSetFromStorage(EXPANDED_DISCUSSIONS_KEY, LEGACY_EXPANDED_DISCUSSIONS_KEY),
);
let collapsedGroups: Set<string> = $state(stringSetFromStorage(COLLAPSED_GROUPS_KEY));
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
  writeStringSet(COLLAPSED_KEY, next);
}

export function expandProject(id: string): void {
  if (!collapsedProjects.has(id)) return;
  const next = new Set(collapsedProjects);
  next.delete(id);
  collapsedProjects = next;
  writeStringSet(COLLAPSED_KEY, next);
}

export function collapseProject(id: string): void {
  if (collapsedProjects.has(id)) return;
  const next = new Set(collapsedProjects).add(id);
  collapsedProjects = next;
  writeStringSet(COLLAPSED_KEY, next);
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
 * Reconcile projectSortMode with Go settings after loadSettings()
 * completes. Go settings are the durable source of truth, but on the
 * first run after upgrade localStorage may hold the user's real
 * preference while Go still has the factory default — in that case
 * push localStorage → Go (one-time migration) instead of overwriting
 * the user's state with the default.
 *
 * View state (collapsed projects etc.) is NOT handled here — it lives
 * in appStorage; see syncSidebarFromAppStorage().
 */
export function syncSidebarFromSettings(): void {
  const s = getSettings();
  const goMode = PROJECT_SORT_MODES.includes(s.projectSortMode)
    ? s.projectSortMode
    : DEFAULT_PROJECT_SORT_MODE;
  if (goMode === DEFAULT_PROJECT_SORT_MODE && projectSortMode !== DEFAULT_PROJECT_SORT_MODE) {
    void updateSettingsPatch({ projectSortMode });
  } else if (projectSortMode !== goMode) {
    projectSortMode = goMode;
    writeProjectSortMode(goMode);
  }
}

/**
 * Re-read the appStorage-backed view state after hydration lands the
 * durable per-client bucket. appStorage itself reconciles cache vs
 * server (pending local writes win; cache-only keys push up), so this
 * just adopts whatever the bucket now holds.
 */
export function syncSidebarFromAppStorage(): void {
  const collapsed = new Set(parseStringArray(appStorageGet(COLLAPSED_KEY) ?? '[]') ?? []);
  if (!setsEqual(collapsed, collapsedProjects)) {
    collapsedProjects = collapsed;
  }
  const discussions = new Set(
    parseStringArray(appStorageGet(EXPANDED_DISCUSSIONS_KEY) ?? '[]') ?? [],
  );
  if (!setsEqual(discussions, expandedDiscussions)) {
    expandedDiscussions = discussions;
  }
  const groups = new Set(
    parseStringArray(appStorageGet(COLLAPSED_GROUPS_KEY) ?? '[]') ?? [],
  );
  if (!setsEqual(groups, collapsedGroups)) {
    collapsedGroups = groups;
  }
  const rawLimits = appStorageGet(THREAD_LIST_VISIBLE_LIMITS_KEY);
  threadListVisibleLimits = rawLimits === null ? {} : (parseThreadListVisibleLimits(rawLimits) ?? {});
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
 * Thread-group collapse state — the inverse of the discussion set above.
 * A group the user has never touched is EXPANDED, so only explicit
 * collapses persist and a newly created group shows its members. Global
 * across projects: a group id is unique, and a per-project map would
 * carry the same information with a project id nobody reads.
 */
export function isGroupExpanded(id: string): boolean {
  return !collapsedGroups.has(id);
}

export function getCollapsedGroups(): ReadonlySet<string> {
  return collapsedGroups;
}

export function toggleGroup(id: string): void {
  const next = new Set(collapsedGroups);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  collapsedGroups = next;
  writeStringSet(COLLAPSED_GROUPS_KEY, next);
}

/**
 * Replace the whole collapsed-group set. Used by the auto-expand effect
 * that un-collapses the group containing the active thread, the same way
 * setExpandedDiscussions is used for discussion ancestors.
 */
export function setCollapsedGroups(next: ReadonlySet<string>): void {
  if (setsEqual(next, collapsedGroups)) return;
  const copy = new Set(next);
  collapsedGroups = copy;
  writeStringSet(COLLAPSED_GROUPS_KEY, copy);
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

/** Test helper: clears in-memory state and the sort-mode cache. View
 *  state lives in appStorage — reset that via resetAppStorageForTest. */
export function resetSidebarForTest(): void {
  collapsedProjects = new Set();
  expandedDiscussions = new Set();
  collapsedGroups = new Set();
  threadListVisibleLimits = {};
  projectSortMode = DEFAULT_PROJECT_SORT_MODE;
  removeLocalStorageKey(SORT_MODE_KEY);
  removeLocalStorageKey(LEGACY_COLLAPSED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_DISCUSSIONS_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_THREAD_LISTS_KEY);
  removeLocalStorageKey(LEGACY_THREAD_LIST_LIMITS_KEY);
}
