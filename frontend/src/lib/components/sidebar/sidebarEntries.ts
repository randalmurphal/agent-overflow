// One value per sidebar entry.
//
// A repository checked out on several computers is ONE row in the sidebar,
// and almost nothing that row shows is a fact about one checkout: which DOM
// row it is, what it is called, what it counts, what it sorts by, what its
// header says about its members. A component handed a representative
// `ProjectWithCounts` as its `project` has that row and whatever is keyed by
// its id as the nearest carrier for any of those, and a row is the wrong
// carrier for all of them: which member it is changes.
//
// An entry is one value instead, built over the listed entries that
// `stores/sidebarEntryContinuity.svelte.ts` groups. That store decides, with
// the device filter ignored, which rows form an entry and which earlier row
// it continues; this module adds only what the filter decides. The ids an
// entry separates:
//   - `key` is the entry's persisted identity: what its disclosure is written
//     under, the repository key of a computer's chosen checkout, else the
//     project id;
//   - `rowKey` is its DOM identity, which survives its key moving under a
//     row that is still the same row, whatever the filter shows;
//   - `id` is the representative of every listed member, the filter
//     ignored: the project an action started on this row targets, and the
//     one the row's name, path, creation date and manual position are read
//     from, because filtering never changes where an action executes
//     (docs/specs/connected-computers.md);
//   - the members are what the filter shows, and what the entry counts,
//     dates and warns about.
//
// Under components/sidebar only this module may import from the projects
// store and the continuity store beyond the writers
// `lib/architecture.test.ts` lists; the test resolves re-exports.
import type { ProjectWithCounts } from '../../types/models';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import { projectBackend } from '../../transport/entityIndex';
import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
import {
  getProject,
  getProjectLiveActivityAt,
  projectOrderPosition,
  projectSpansBackends,
  representativeOf,
} from '../../stores/projects.svelte';
import {
  entryExpanded,
  entryListLimit,
  sidebarEntryGroups,
  sidebarEntryRowOf,
} from '../../stores/sidebarEntryContinuity.svelte';
import { getProjectSortMode, type ProjectSortMode } from '../../stores/sidebar.svelte';
import { sidebarBackendVisible } from '../../stores/sidebarDevices.svelte';

/** A member whose identity its own computer could not verify. */
export interface SidebarEntryWarning {
  /** The member to recheck, which is never a member with no row yet. */
  readonly id: string;
  readonly path: string;
  readonly reason: string;
}

export interface SidebarEntry {
  /** The key the entry's disclosure is persisted by. */
  readonly key: string;
  /**
   * DOM identity: the key of the `{#each}` that renders the rows, and of the
   * thread buckets under them.
   *
   * Not `key`, which moves while the row does not. A keyed each takes an item
   * whose key changed for a different item: it mounts the new row and plays
   * the old row's exit with it out of flow, so for the length of that exit
   * the same project, and every thread under it, is on screen twice.
   */
  readonly rowKey: string;
  /** The project an action started on this row acts on, whatever the filter shows. */
  readonly id: string;
  readonly backend: BackendKey;
  readonly name: string;
  readonly path: string;
  readonly createdAt: number;
  readonly threadCount: number;
  /** Manual order of `id`: the order still out while a reorder has not settled. */
  readonly position: number;
  readonly expanded: boolean;
  /** How many rows the entry's own list shows. */
  readonly listLimit: number;
  /** The members the sidebar shows of this entry, in list order. */
  readonly members: readonly ProjectWithCounts[];
  readonly warnings: readonly SidebarEntryWarning[];
}

/** What an action on a row affects: the whole entry, or one of its checkouts. */
export type ProjectTargetScope =
  | { readonly kind: 'whole' }
  | { readonly kind: 'oneOfSeveral'; readonly computerName: string };

/**
 * What an action carries: a project, frozen at the gesture that started it.
 * The label is the row's as the user read it, which the list disambiguates
 * over what it renders, so it is passed in rather than read off the entry.
 */
export interface ProjectTarget {
  readonly id: string;
  readonly name: string;
  readonly path: string;
  readonly backend: BackendKey;
  /** The row's label as the user read it, for the lines an action writes. */
  readonly label: string;
  /** The target project's own threads when the gesture started. */
  readonly threadCount: number;
  readonly scope: ProjectTargetScope;
}

export interface SidebarEntriesInput {
  /** Whether the device filter shows a computer. */
  readonly visible: (backend: BackendKey) => boolean;
  /** The last result, so an entry nothing changed keeps its identity. */
  readonly previous: readonly SidebarEntry[];
}

const NO_WARNINGS: readonly SidebarEntryWarning[] = [];

function backendOf(row: ProjectWithCounts): BackendKey {
  return projectBackend(row.project.id) ?? HOME_BACKEND;
}

/**
 * The entries the sidebar renders. An entry with no member the filter shows,
 * or whose listed members are all archived, renders nothing; its members'
 * threads still bucket under its rowKey (`sidebarBucketOf`).
 */
export function buildSidebarEntries(input: SidebarEntriesInput): readonly SidebarEntry[] {
  const previous = new Map(input.previous.map((entry) => [entry.rowKey, entry]));
  const entries: SidebarEntry[] = [];
  for (const group of sidebarEntryGroups()) {
    const shown = group.members.filter((member) => input.visible(backendOf(member)));
    if (shown.length === 0) continue;
    const rep = representativeOf(group.members);
    if (rep.project.archived) continue;
    const backend = backendOf(rep);
    let threadCount = 0;
    const warnings: SidebarEntryWarning[] = [];
    for (const member of shown) {
      threadCount += member.threadCount;
      const reason = member.project.identityError;
      if (reason) warnings.push({ id: member.project.id, path: member.project.path, reason });
    }
    const next: SidebarEntry = {
      key: group.key,
      rowKey: group.rowKey,
      id: rep.project.id,
      backend,
      name: rep.project.name,
      path: rep.project.path,
      createdAt: rep.project.createdAt,
      threadCount,
      position: projectOrderPosition(rep),
      expanded: entryExpanded(group),
      listLimit: entryListLimit(group),
      members: shown,
      warnings: warnings.length === 0 ? NO_WARNINGS : warnings,
    };
    const before = previous.get(group.rowKey);
    entries.push(before !== undefined && sameEntry(before, next) ? before : next);
  }
  return entries;
}

/**
 * The bucket a project's threads and groups sit in: its entry's rowKey, the
 * filter ignored, so an entry the filter hides still has one; '' for a
 * project in no listed entry, whose threads have no row.
 */
export function sidebarBucketOf(projectId: string): string {
  return sidebarEntryRowOf(projectId);
}

/**
 * What the list sorts a date-ordered sidebar by: the newest live activity
 * over the members the entry shows, and when no thread has touched any of
 * them, the newest the rows themselves were changed, so a freshly renamed or
 * added project still surfaces. Both halves read the shown members only: the
 * representative's own row answered for a checkout the filter hides.
 *
 * A function and not a field, because a streaming beat moves it: a field
 * would allocate a new entry on every beat and defeat the identity cutoff
 * the entries exist to keep.
 */
export function rankOf(entry: SidebarEntry): number {
  let activity = 0;
  let updated = 0;
  for (const member of entry.members) {
    activity = Math.max(activity, getProjectLiveActivityAt(member));
    updated = Math.max(updated, member.project.updatedAt);
  }
  return activity > 0 ? activity : updated;
}

/**
 * The entries in the sidebar's order `mode`. Last activity ranks each entry
 * once per sort: a rank scans the members and reads their live activity,
 * which the comparator would otherwise repeat for every pair it compares.
 */
export function sortSidebarEntries(entries: readonly SidebarEntry[], mode: ProjectSortMode): SidebarEntry[] {
  switch (mode) {
    case 'manual':
      return [...entries].sort((a, b) => (a.position - b.position) || (a.createdAt - b.createdAt));
    case 'createdAt':
      return [...entries].sort((a, b) => b.createdAt - a.createdAt);
    case 'lastActivity': {
      const rank = new Map(entries.map((entry) => [entry, rankOf(entry)]));
      return [...entries].sort((a, b) => (rank.get(b)! - rank.get(a)!) || a.name.localeCompare(b.name));
    }
  }
}

/**
 * Each project's position in the sidebar as it shows now, every checkout of
 * an entry at the entry's position, with no search applied; a project the
 * sidebar does not show has none. Builds and sorts the entries on each
 * call, so a caller reads it once per decision, outside a reactive read.
 */
export function sidebarProjectOrder(): ReadonlyMap<string, number> {
  const entries = sortSidebarEntries(buildSidebarEntries({ visible: sidebarBackendVisible, previous: [] }), getProjectSortMode());
  const out = new Map<string, number>();
  entries.forEach((entry, i) => {
    for (const member of entry.members) out.set(member.project.id, i);
  });
  return out;
}

/**
 * Freeze what an action needs about its project, at the gesture that started
 * it. An action holding this cannot observe a representative change, so no
 * await, confirmation or preview can retarget it.
 *
 * The count is the target project's own threads, and the scope is decided
 * over every checkout the client holds, a computer whose tree is still held
 * included: a checkout the sidebar does not list yet is one the action does
 * not affect, and its row comes back when that computer lists.
 */
export function projectTarget(entry: SidebarEntry, label: string): ProjectTarget {
  const own = getProject(entry.id);
  if (own === undefined) throw new Error(`Sidebar entry ${entry.rowKey} targets project ${entry.id}, which the client does not hold`);
  return Object.freeze({
    id: entry.id,
    name: entry.name,
    path: entry.path,
    backend: entry.backend,
    label,
    threadCount: own.threadCount,
    scope: projectSpansBackends(entry.id)
      ? Object.freeze({ kind: 'oneOfSeveral', computerName: computerName(entry.backend) })
      : WHOLE,
  });
}

const WHOLE: ProjectTargetScope = Object.freeze({ kind: 'whole' });

function computerName(backend: BackendKey): string {
  const attached = attachedBackendEntry(backend);
  if (attached === undefined) throw new Error(`Computer ${backend} of a listed project is not attached`);
  return backendDisplayName(attached);
}

function sameEntry(a: SidebarEntry, b: SidebarEntry): boolean {
  return a.key === b.key
    && a.id === b.id
    && a.backend === b.backend
    && a.name === b.name
    && a.path === b.path
    && a.createdAt === b.createdAt
    && a.threadCount === b.threadCount
    && a.position === b.position
    && a.expanded === b.expanded
    && a.listLimit === b.listLimit
    // The warnings are a projection of the members, so identical members are
    // identical warnings.
    && a.members.length === b.members.length
    && a.members.every((member, index) => member === b.members[index]);
}
