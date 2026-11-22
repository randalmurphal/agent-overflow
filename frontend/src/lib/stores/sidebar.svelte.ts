// Sidebar UI state: which groups and sections are folded, how far a group's
// or the Assistants section's list is paged, and which direction the projects
// list is sorted. A project row's fold and list size belong to its sidebar
// entry (stores/sidebarEntryContinuity.svelte.ts), which keeps them in records
// built by the factories here.
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
// Folds use an inverted set: we persist explicit *collapses* rather than
// expansions, so an unseen row defaults to expanded (t3-code parity). The
// public API keeps "expanded" semantics; only the underlying storage flips.
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
// ids — the same inversion a project's fold uses, for the same reason:
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

/**
 * How records follow when the keys they are persisted under move. Applied as
 * one step against the record as it was, so a key that gives its value away
 * and takes another's in the same change takes the one it was given.
 */
export interface RecordKeyChanges {
  /** Each key, and the key whose value it takes. */
  readonly copies: ReadonlyMap<string, string>;
  /** Keys whose value goes, unless a copy writes them. */
  readonly clears: ReadonlySet<string>;
}

/** A persisted set of keys: the folds of one kind of row. */
export interface KeySetRecord {
  has(key: string): boolean;
  values(): ReadonlySet<string>;
  set(key: string, present: boolean): void;
  apply(changes: RecordKeyChanges): void;
  /** Re-read the record after hydration lands the durable bucket. */
  reload(): void;
  /** Test helper: forget the in-memory copy. */
  resetForTest(): void;
}

/** The set of keys persisted under `storageKey`, adopting `legacyKey` once. */
export function keySetRecord(storageKey: string, legacyKey?: string): KeySetRecord {
  let keys: ReadonlySet<string> = $state.raw(stringSetFromStorage(storageKey, legacyKey));

  function write(next: ReadonlySet<string>): void {
    keys = next;
    writeStringSet(storageKey, next);
  }

  return {
    has: (key) => keys.has(key),
    values: () => keys,
    set(key, present) {
      if (keys.has(key) === present) return;
      const next = new Set(keys);
      if (present) next.add(key);
      else next.delete(key);
      write(next);
    },
    apply(changes) {
      const next = new Set(keys);
      for (const key of changes.clears) next.delete(key);
      for (const key of changes.copies.keys()) next.delete(key);
      for (const [to, from] of changes.copies) if (keys.has(from)) next.add(to);
      if (!setsEqual(next, keys)) write(next);
    },
    reload() {
      const next = new Set(parseStringArray(appStorageGet(storageKey) ?? '[]') ?? []);
      if (!setsEqual(next, keys)) keys = next;
    },
    resetForTest() {
      keys = new Set();
    },
  };
}

export interface ListLimitRecord {
  limitOf(key: string): number;
  setLimit(key: string, limit: number): void;
  clear(key: string): void;
  apply(changes: RecordKeyChanges): void;
  /** Re-read the record after hydration lands the durable bucket. */
  reload(): void;
  /** Test helper: forget the in-memory copy. */
  resetForTest(): void;
}

/**
 * One record of how many rows each list under `storageKey` shows, by
 * whatever key its caller gives it.
 *
 * Every list starts at THREAD_PREVIEW_LIMIT and the default is stored as
 * nothing at all, so a record holds only the lists the user has revealed
 * more of, and a limit at or below the preview removes its entry. There are
 * two records, one for project entries and one for everything else, and this
 * is the paging rule both keep: the records differ in what may be written to
 * them, never in how.
 */
export function listLimitRecord(storageKey: string, legacyKey?: string): ListLimitRecord {
  function fromStorage(): Record<string, number> {
    const raw = appStorageGet(storageKey);
    return raw === null ? {} : (parseListLimits(raw) ?? {});
  }

  // Adoption is init-only: a legacy key is read once and removed with it.
  const adopted =
    appStorageGet(storageKey) ??
    (legacyKey === undefined
      ? null
      : appStorageAdoptLegacyKey(storageKey, legacyKey, (legacy) =>
          parseListLimits(legacy) === null ? null : legacy,
        ));
  let limits: Record<string, number> = $state(
    adopted === null ? {} : (parseListLimits(adopted) ?? {}),
  );

  function commit(next: Record<string, number>): void {
    limits = next;
    if (Object.keys(next).length === 0) appStorageDelete(storageKey);
    else appStorageSet(storageKey, JSON.stringify(next));
  }

  function clear(key: string): void {
    if (!(key in limits)) return;
    const next = { ...limits };
    delete next[key];
    commit(next);
  }

  return {
    clear,
    apply(changes: RecordKeyChanges): void {
      const next = { ...limits };
      for (const key of changes.clears) delete next[key];
      for (const key of changes.copies.keys()) delete next[key];
      for (const [to, from] of changes.copies) {
        const limit = limits[from];
        if (limit !== undefined) next[to] = limit;
      }
      const keys = Object.keys(next);
      if (keys.length === Object.keys(limits).length && keys.every((key) => limits[key] === next[key])) return;
      commit(next);
    },
    limitOf(key: string): number {
      return Math.max(THREAD_PREVIEW_LIMIT, limits[key] ?? THREAD_PREVIEW_LIMIT);
    },
    setLimit(key: string, limit: number): void {
      const roundedLimit = Math.floor(limit);
      if (!Number.isFinite(roundedLimit) || roundedLimit <= THREAD_PREVIEW_LIMIT) {
        clear(key);
        return;
      }
      if (limits[key] === roundedLimit) return;
      commit({ ...limits, [key]: roundedLimit });
    },
    reload(): void {
      limits = fromStorage();
    },
    resetForTest(): void {
      limits = {};
    },
  };
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

removeLocalStorageKey(LEGACY_COLLAPSED_STORAGE_KEY);
removeLocalStorageKey(LEGACY_EXPANDED_STORAGE_KEY);
const collapsedGroups = keySetRecord(COLLAPSED_GROUPS_KEY);
const collapsedSections = keySetRecord(COLLAPSED_SECTIONS_KEY);
// Dropped rather than adopted: the pre-inversion key listed the thread lists
// the user had EXPANDED, and the inverse of that needs every list there is.
removeLocalStorageKey(LEGACY_EXPANDED_THREAD_LISTS_KEY);
// How many rows a group's members and the Assistants section's rows show,
// by group id and by ASSISTANTS_LIST_KEY.
const threadListLimits = listLimitRecord(
  THREAD_LIST_VISIBLE_LIMITS_KEY,
  LEGACY_THREAD_LIST_LIMITS_KEY,
);
let projectSortMode: ProjectSortMode = $state(readProjectSortMode());

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
  collapsedGroups.reload();
  collapsedSections.reload();
  threadListLimits.reload();
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
  return collapsedGroups.values();
}

export function toggleGroup(id: string): void {
  collapsedGroups.set(id, !collapsedGroups.has(id));
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
  collapsedSections.set(section, !expanded);
}

export function toggleSidebarSection(section: SidebarSectionId): void {
  setSidebarSectionExpanded(section, !isSidebarSectionExpanded(section));
}

/**
 * The key the Assistants section's rows page under in the thread-list
 * preview sizes below, beside the group ids. A project's list pages in a
 * record of its own.
 */
export const ASSISTANTS_LIST_KEY = 'assistants';

/**
 * What a sidebar list's top level pages under. Its two kinds page in two
 * records, so the caller names which it is instead of a key alone saying it:
 * a project's record is keyed by its entry and holds nothing else. A
 * project's list carries the limit its entry read, since the entry knows
 * where its record lives while its key moves.
 *
 * A list starts at THREAD_PREVIEW_LIMIT; each "Show more" click reveals up
 * to THREAD_REVEAL_INCREMENT hidden rows, and "Show less" drops it back to
 * the default preview.
 */
export type SidebarTopLevelList =
  | { readonly kind: 'project'; readonly entryKey: string; readonly limit: number }
  | { readonly kind: 'assistants' };

export function getThreadListVisibleLimit(key: string): number {
  return threadListLimits.limitOf(key);
}

export function setThreadListVisibleLimit(key: string, limit: number): void {
  threadListLimits.setLimit(key, limit);
}

export function collapseThreadList(key: string): void {
  threadListLimits.clear(key);
}

/** Test helper: clears in-memory state and the sort-mode cache. View
 *  state lives in appStorage — reset that via resetAppStorageForTest. */
export function resetSidebarForTest(): void {
  showProviderIcons = false;
  collapsedGroups.resetForTest();
  collapsedSections.resetForTest();
  threadListLimits.resetForTest();
  projectSortMode = DEFAULT_PROJECT_SORT_MODE;
  removeLocalStorageKey(SORT_MODE_KEY);
  removeLocalStorageKey(LEGACY_COLLAPSED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_STORAGE_KEY);
  removeLocalStorageKey(LEGACY_EXPANDED_THREAD_LISTS_KEY);
  removeLocalStorageKey(LEGACY_THREAD_LIST_LIMITS_KEY);
}
