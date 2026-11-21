// Sidebar UI state: which project rows are expanded, and which direction
// the projects list is sorted.
//
// Persistence follows the kind of state:
//   - Provider icons persist locally through frontendStorage.
//   - View state (collapsed projects, collapsed groups, thread-list
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
// collapseProject) keeps "expanded" semantics — only the
// underlying storage flips.
//
// We deliberately keep this separate from projects.svelte.ts — that store
// is about data, this one is about view state. They live together on
// screen but have different lifecycles (data refreshes; view state
// persists across refreshes).

import type { ProjectSortMode } from '../types/settings';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';
import {
  appStorageAdoptLegacyKey,
  appStorageDelete,
  appStorageGet,
  appStorageSet,
  isAppStorageHydrated,
} from './appStorage';
import { getSettings, updateSettingsPatch } from './settings.svelte';
import { readFrontendValue, writeFrontendValue, onFrontendValueChanged } from './frontendStorage';

export type { ProjectSortMode };

// appStorage (per-client) keys.
//
// A project row's disclosure, its fold and the size of its own list, is
// keyed by the sidebar ENTRY (`stores/projectDisclosure.ts` chooses the
// key) and lives in records of its own, which nothing else writes.
//
// They are new records because a project's disclosure used to be keyed by
// its own id, before a repository checked out on several computers became
// one entry, and that cannot be migrated: an id cannot be turned into an
// entry key without every computer's project rows, which the sidebar has
// not read when this module initializes. Separate records are what makes
// the old state unreachable rather than merely unread. An entry's key moves
// with its repository identity, which Recheck can restore and a lost remote
// can take away, so a shared record would answer a project's id again the
// moment its identity went: it would be the same fold the upgrade dropped,
// coming back. Every project therefore opens once, whatever its key is.
const COLLAPSED_PROJECTS_KEY = 'sidebar:collapsedProjectEntries';
const PROJECT_LIST_LIMITS_KEY = 'sidebar:projectEntryListLimits';
// Read by nothing: the id-keyed fold from before the entry key. Dropped
// after hydration rather than at init, because the first write of a boot
// marks this client migrated (`persist` in ./appStorage.ts) and a boot whose
// host read then fails must still be able to read the host bucket.
const RETIRED_COLLAPSED_PROJECTS_KEY = 'sidebar:collapsedProjects';
// A group's members and the Assistants section's rows, by group id and by
// ASSISTANTS_LIST_KEY. Untouched by the entry key: neither ever changed key,
// so this record and its legacy adoption stay as they were. Limits a project
// id saved here before the entry key are read by nothing.
const THREAD_LIST_VISIBLE_LIMITS_KEY = 'sidebar:threadListVisibleLimits';
// Groups default to EXPANDED, so the persisted set holds the COLLAPSED
// ids — the same inversion collapsedProjects uses, for the same reason:
// a group the user has never touched must show its members. No legacy
// key: the feature never existed before appStorage.
const COLLAPSED_GROUPS_KEY = 'sidebar:collapsedGroups';
// The Assistants and Projects sections, each folded whole from its header.
const COLLAPSED_SECTIONS_KEY = 'sidebar:collapsedSections';

// Pre-appStorage localStorage keys. The two project ones are removed, not
// adopted: both were keyed by a project id. Removing a localStorage key
// writes nothing to appStorage.
const LEGACY_COLLAPSED_STORAGE_KEY = 'agent-overflow:sidebar:collapsedProjects';
const LEGACY_EXPANDED_STORAGE_KEY = 'agent-overflow:sidebar:expandedProjects';
const LEGACY_EXPANDED_THREAD_LISTS_KEY = 'agent-overflow:sidebar:expandedThreadLists';
const LEGACY_THREAD_LIST_LIMITS_KEY = 'agent-overflow:sidebar:threadListVisibleLimits';

// projectSortMode cache key (Go settings remain the durable copy).
const SORT_MODE_KEY = 'agent-overflow:sidebar:projectSortMode';

const DEFAULT_PROJECT_SORT_MODE: ProjectSortMode = 'lastActivity';

export const PROJECT_SORT_OPTIONS = [
  { value: 'lastActivity', label: 'Latest Activity' },
  { value: 'createdAt', label: 'Created' },
  { value: 'manual', label: 'Manual' },
] as const satisfies ReadonlyArray<{ value: ProjectSortMode; label: string }>;
const PROJECT_SORT_MODES: readonly ProjectSortMode[] = PROJECT_SORT_OPTIONS.map((option) => option.value);

const PROVIDER_ICONS_KEY = 'sidebar:showProviderIcons';
let showProviderIcons = $state(readFrontendValue(PROVIDER_ICONS_KEY) === true);

export function getShowProviderIcons(): boolean {
  return showProviderIcons;
}

export function setShowProviderIcons(value: boolean): boolean {
  if (typeof value !== 'boolean') throw new Error('Show provider icons must be a boolean');
  showProviderIcons = value;
  return writeFrontendValue(PROVIDER_ICONS_KEY, value);
}

onFrontendValueChanged(PROVIDER_ICONS_KEY, () => {
  showProviderIcons = readFrontendValue(PROVIDER_ICONS_KEY) === true;
});

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

function readCollapsedProjects(): Set<string> {
  removeLocalStorageKey(LEGACY_COLLAPSED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_STORAGE_KEY);
  return stringSetFromStorage(COLLAPSED_PROJECTS_KEY);
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

function parseListLimits(raw: string): Record<string, number> | null {
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

function readListLimits(key: string): Record<string, number> {
  const raw = appStorageGet(key);
  if (raw === null) return {};
  return parseListLimits(raw) ?? {};
}

function writeListLimits(key: string, limits: Record<string, number>): void {
  if (Object.keys(limits).length === 0) {
    appStorageDelete(key);
    return;
  }
  appStorageSet(key, JSON.stringify(limits));
}

function readThreadListVisibleLimits(): Record<string, number> {
  removeLocalStorageKey(LEGACY_EXPANDED_THREAD_LISTS_KEY);
  const raw =
    appStorageGet(THREAD_LIST_VISIBLE_LIMITS_KEY) ??
    appStorageAdoptLegacyKey(THREAD_LIST_VISIBLE_LIMITS_KEY, LEGACY_THREAD_LIST_LIMITS_KEY, (legacy) =>
      parseListLimits(legacy) === null ? null : legacy,
    );
  if (raw === null) return {};
  return parseListLimits(raw) ?? {};
}

function writeThreadListVisibleLimits(limits: Record<string, number>): void {
  writeListLimits(THREAD_LIST_VISIBLE_LIMITS_KEY, limits);
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

let collapsedProjects: Set<string> = $state(readCollapsedProjects());
// How many rows a project's own list shows, by entry key.
let projectListLimits: Record<string, number> = $state(readListLimits(PROJECT_LIST_LIMITS_KEY));
let collapsedGroups: Set<string> = $state(stringSetFromStorage(COLLAPSED_GROUPS_KEY));
let collapsedSections: Set<string> = $state(stringSetFromStorage(COLLAPSED_SECTIONS_KEY));
// How many rows a group's members and the Assistants section's rows show,
// by group id and by ASSISTANTS_LIST_KEY.
let threadListVisibleLimits: Record<string, number> = $state(readThreadListVisibleLimits());
let projectSortMode: ProjectSortMode = $state(readProjectSortMode());

// Each record is persisted by the key its caller gives, and this module does
// not know what that key is about. A project's is its sidebar entry's key
// (`stores/projectDisclosure.ts` chooses it); a group's is the group id, the
// Assistants section's is ASSISTANTS_LIST_KEY.

function writeCollapsed(next: Set<string>): void {
  collapsedProjects = next;
  writeStringSet(COLLAPSED_PROJECTS_KEY, next);
}

export function isProjectExpanded(key: string): boolean {
  return !collapsedProjects.has(key);
}

export function expandProject(key: string): void {
  if (!collapsedProjects.has(key)) return;
  const next = new Set(collapsedProjects);
  next.delete(key);
  writeCollapsed(next);
}

export function collapseProject(key: string): void {
  if (collapsedProjects.has(key)) return;
  writeCollapsed(new Set(collapsedProjects).add(key));
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
  // The id-keyed fold is read by nothing; this is the first point in a boot
  // where dropping it cannot be mistaken for a migration this client has
  // already done.
  if (isAppStorageHydrated() && appStorageGet(RETIRED_COLLAPSED_PROJECTS_KEY) !== null) {
    appStorageDelete(RETIRED_COLLAPSED_PROJECTS_KEY);
  }
  const collapsed = new Set(parseStringArray(appStorageGet(COLLAPSED_PROJECTS_KEY) ?? '[]') ?? []);
  if (!setsEqual(collapsed, collapsedProjects)) {
    collapsedProjects = collapsed;
  }
  projectListLimits = readListLimits(PROJECT_LIST_LIMITS_KEY);
  const groups = new Set(
    parseStringArray(appStorageGet(COLLAPSED_GROUPS_KEY) ?? '[]') ?? [],
  );
  if (!setsEqual(groups, collapsedGroups)) {
    collapsedGroups = groups;
  }
  const sections = new Set(
    parseStringArray(appStorageGet(COLLAPSED_SECTIONS_KEY) ?? '[]') ?? [],
  );
  if (!setsEqual(sections, collapsedSections)) {
    collapsedSections = sections;
  }
  const rawLimits = appStorageGet(THREAD_LIST_VISIBLE_LIMITS_KEY);
  threadListVisibleLimits = rawLimits === null ? {} : (parseListLimits(rawLimits) ?? {});
}

function setsEqual(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  if (a.size !== b.size) return false;
  for (const id of a) {
    if (!b.has(id)) return false;
  }
  return true;
}

/**
 * Thread-group collapse state. A group the user has never touched is
 * EXPANDED, so only explicit collapses persist and a newly created group
 * shows its members. Global across projects: a group id is unique, and a
 * per-project map would carry the same information with a project id
 * nobody reads.
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

export type SidebarSectionId = 'assistants' | 'projects';

/** Whether a whole sidebar section is unfolded; unfolded until the user folds it. */
export function isSidebarSectionExpanded(section: SidebarSectionId): boolean {
  return !collapsedSections.has(section);
}

/**
 * Whether a section shows its rows: while it is unfolded, and during a
 * search, which shows its matches in a folded section too. Its header's
 * chevron and `aria-expanded` follow this.
 */
export function isSidebarSectionShown(section: SidebarSectionId, searching: boolean): boolean {
  return searching || isSidebarSectionExpanded(section);
}

export function setSidebarSectionExpanded(section: SidebarSectionId, expanded: boolean): void {
  if (collapsedSections.has(section) === !expanded) return;
  const next = new Set(collapsedSections);
  if (expanded) next.delete(section);
  else next.add(section);
  collapsedSections = next;
  writeStringSet(COLLAPSED_SECTIONS_KEY, next);
}

export function toggleSidebarSection(section: SidebarSectionId): void {
  setSidebarSectionExpanded(section, !isSidebarSectionExpanded(section));
}

/**
 * The key the Assistants section's rows page under in the per-list
 * preview sizes below. A project's list pages under its entry id, a UUID,
 * so the two never meet.
 */
export const ASSISTANTS_LIST_KEY = 'assistants';

/**
 * Thread-list preview size, keyed by the list's owner: a project entry's key
 * for its top level, a group id for the group's members (both random entity
 * ids, so they share one map), and ASSISTANTS_LIST_KEY for the Assistants
 * section's rows. A list starts at THREAD_PREVIEW_LIMIT; each "Show more"
 * click reveals up to THREAD_REVEAL_INCREMENT hidden rows, and "Show less"
 * drops it back to the default preview.
 */
/**
 * What a sidebar list's top level pages under. Its two kinds page in two
 * records, so the caller names which it is instead of a key alone saying it:
 * a project's record is keyed by its entry and holds nothing else.
 */
export type SidebarTopLevelList =
  | { readonly kind: 'project'; readonly entryKey: string }
  | { readonly kind: 'assistants' };

/**
 * How many rows a project's own list shows, by its sidebar entry's key. Its
 * own record: nothing a project saved under its project id before the entry
 * key can be read here, whatever its key is today.
 */
export function getProjectListLimit(entryKey: string): number {
  return Math.max(THREAD_PREVIEW_LIMIT, projectListLimits[entryKey] ?? THREAD_PREVIEW_LIMIT);
}

export function setProjectListLimit(entryKey: string, limit: number): void {
  const roundedLimit = Math.floor(limit);
  if (!Number.isFinite(roundedLimit) || roundedLimit <= THREAD_PREVIEW_LIMIT) {
    collapseProjectList(entryKey);
    return;
  }
  if (projectListLimits[entryKey] === roundedLimit) return;
  const next = { ...projectListLimits, [entryKey]: roundedLimit };
  projectListLimits = next;
  writeListLimits(PROJECT_LIST_LIMITS_KEY, next);
}

export function collapseProjectList(entryKey: string): void {
  if (!(entryKey in projectListLimits)) return;
  const next = { ...projectListLimits };
  delete next[entryKey];
  projectListLimits = next;
  writeListLimits(PROJECT_LIST_LIMITS_KEY, next);
}

export function getThreadListVisibleLimit(key: string): number {
  return Math.max(THREAD_PREVIEW_LIMIT, threadListVisibleLimits[key] ?? THREAD_PREVIEW_LIMIT);
}

export function setThreadListVisibleLimit(key: string, limit: number): void {
  const roundedLimit = Math.floor(limit);
  if (!Number.isFinite(roundedLimit) || roundedLimit <= THREAD_PREVIEW_LIMIT) {
    collapseThreadList(key);
    return;
  }
  if (threadListVisibleLimits[key] === roundedLimit) return;
  const next = { ...threadListVisibleLimits, [key]: roundedLimit };
  threadListVisibleLimits = next;
  writeThreadListVisibleLimits(next);
}

export function collapseThreadList(key: string): void {
  if (!(key in threadListVisibleLimits)) return;
  const next = { ...threadListVisibleLimits };
  delete next[key];
  threadListVisibleLimits = next;
  writeThreadListVisibleLimits(next);
}

/** Test helper: clears in-memory state and the sort-mode cache. View
 *  state lives in appStorage — reset that via resetAppStorageForTest. */
export function resetSidebarForTest(): void {
  showProviderIcons = false;
  collapsedProjects = new Set();
  projectListLimits = {};
  collapsedGroups = new Set();
  collapsedSections = new Set();
  threadListVisibleLimits = {};
  projectSortMode = DEFAULT_PROJECT_SORT_MODE;
  removeLocalStorageKey(SORT_MODE_KEY);
  removeLocalStorageKey(LEGACY_COLLAPSED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_THREAD_LISTS_KEY);
  removeLocalStorageKey(LEGACY_THREAD_LIST_LIMITS_KEY);
}
