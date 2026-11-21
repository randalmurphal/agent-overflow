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
// An entry is one value instead: the components take it and can reach none
// of this another way (`lib/architecture.test.ts` fences the alternatives).
// The ids it separates:
//   - `key` is the entry's persisted identity, stable across every change of
//     which member represents it: what its disclosure is written under;
//   - `rowKey` is its DOM identity, which also survives its KEY moving under
//     a row that is still the same row;
//   - `id` is the project an action started on this row targets, the listed
//     members' representative;
//   - the members are what the entry counts, dates and speaks for, which the
//     device filter decides.
//
// Only this module and ProjectsSection.svelte may read the projects store's
// entry-level functions; `lib/architecture.test.ts` enforces that.
import type { ProjectWithCounts } from '../../types/models';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import { projectBackend } from '../../transport/entityIndex';
import {
  entryIdFor,
  getProjectLiveActivityAt,
  getProjects,
  projectEntryKey,
  projectOrderPosition,
  representativeOf,
} from '../../stores/projects.svelte';


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
   * DOM identity: the key of the `{#each}` that renders the rows.
   *
   * Not `key`, which moves while the row does not. A computer verifies a
   * checkout's repository identity on its own schedule, so a project listed
   * without one is keyed by its project id until the identity lands and by
   * the repository afterwards. A keyed each takes an item whose key changed
   * for a different item: it mounts the new row and plays the old row's exit
   * with it out of flow, so for the length of that exit the same project,
   * and every thread under it, is on screen twice. Carried from the previous
   * build through a member the row kept, so neither an identity arriving nor
   * one going away remounts a row.
   */
  readonly rowKey: string;
  /** The project an action started on this row acts on. */
  readonly id: string;
  readonly backend: BackendKey;
  readonly name: string;
  readonly path: string;
  readonly createdAt: number;
  readonly threadCount: number;
  /** Manual order: the order still out while a reorder has not settled. */
  readonly position: number;
  readonly expanded: boolean;
  /** The members the sidebar shows of this entry, in list order. */
  readonly members: readonly ProjectWithCounts[];
  readonly warnings: readonly SidebarEntryWarning[];
}

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
  /** What the row counted when the gesture started, for what a
   *  confirmation says it is about to affect. */
  readonly threadCount: number;
}

export interface SidebarEntries {
  readonly entries: readonly SidebarEntry[];
  /** Any member's project id → the id of the row rendered for it. */
  readonly entryOf: ReadonlyMap<string, string>;
}

export interface SidebarEntriesInput {
  /** Whether a computer's whole tree lists; a held member is not an entry's. */
  readonly listed: (backend: BackendKey) => boolean;
  /** Whether the device filter shows a computer. */
  readonly visible: (backend: BackendKey) => boolean;
  /** Whether an entry is unfolded. Takes the entry so the caller chooses
   *  what the state is keyed by. */
  readonly expanded: (entry: { readonly key: string; readonly id: string }) => boolean;
  /** The last result, so an entry nothing changed keeps its identity. */
  readonly previous: readonly SidebarEntry[];
}

const NO_WARNINGS: readonly SidebarEntryWarning[] = [];

/**
 * The entries the sidebar renders, and the row each member renders under.
 *
 * A member whose computer holds its tree is out of this entirely, so a
 * computer that answers its projects before its threads changes nothing on
 * screen. Of the members that remain, the device filter decides which ones an
 * entry counts, dates and speaks for; the representative is elected over the
 * listed members with the filter ignored, so the label, the action target and
 * the manual position change only when a computer's tree lists.
 */
export function buildSidebarEntries(input: SidebarEntriesInput): SidebarEntries {
  const groups = new Map<string, { listed: ProjectWithCounts[]; shown: ProjectWithCounts[] }>();
  for (const row of getProjects()) {
    const backend = projectBackend(row.project.id) ?? HOME_BACKEND;
    if (!input.listed(backend)) continue;
    // The store's entry id groups the members; which of them wears the entry
    // is decided below, over the listed ones.
    const token = entryIdFor(row.project.id);
    let group = groups.get(token);
    if (group === undefined) groups.set(token, group = { listed: [], shown: [] });
    group.listed.push(row);
    if (input.visible(backend)) group.shown.push(row);
  }

  const entryOf = new Map<string, string>();
  const rendered: { rep: ProjectWithCounts; shown: readonly ProjectWithCounts[] }[] = [];
  for (const group of groups.values()) {
    const rep = representativeOf(group.listed);
    // Every member buckets under the row rendered for it, whether or not the
    // entry has one: a thread of an archived project has no row either.
    for (const member of group.listed) entryOf.set(member.project.id, rep.project.id);
    if (group.shown.length === 0 || rep.project.archived) continue;
    rendered.push({ rep, shown: group.shown });
  }

  const previous = new Map(input.previous.map((entry) => [entry.key, entry]));
  // Which row each member was rendered under last time, so a row whose key
  // moved keeps its DOM identity. One row may claim a previous row's
  // identity: two rows with one `rowKey` would be a duplicate each key.
  const rowKeyOf = new Map<string, string>();
  for (const before of input.previous) {
    for (const member of before.members) rowKeyOf.set(member.project.id, before.rowKey);
  }
  const claimed = new Set<string>();
  const entries = rendered.map(({ rep, shown }) => {
    const id = rep.project.id;
    const key = projectEntryKey(id);
    const rowKey = claimRowKey(shown, key, id, rowKeyOf, claimed);
    let threadCount = 0;
    const warnings: SidebarEntryWarning[] = [];
    for (const member of shown) {
      threadCount += member.threadCount;
      const reason = member.project.identityError;
      if (reason) warnings.push({ id: member.project.id, path: member.project.path, reason });
    }
    const next: SidebarEntry = {
      key,
      rowKey,
      id,
      backend: projectBackend(id) ?? HOME_BACKEND,
      name: rep.project.name,
      path: rep.project.path,
      createdAt: rep.project.createdAt,
      threadCount,
      position: projectOrderPosition(rep),
      expanded: input.expanded({ key, id }),
      members: shown,
      warnings: warnings.length === 0 ? NO_WARNINGS : warnings,
    };
    const before = previous.get(key);
    return before !== undefined && sameEntry(before, next) ? before : next;
  });
  return { entries, entryOf };
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
 * Freeze what an action needs about its project, at the gesture that started
 * it. An action holding this cannot observe a representative change, so no
 * await, confirmation or preview can retarget it.
 */
export function projectTarget(entry: SidebarEntry, label: string): ProjectTarget {
  return Object.freeze({
    id: entry.id,
    name: entry.name,
    path: entry.path,
    backend: entry.backend,
    label,
    threadCount: entry.threadCount,
  });
}

/**
 * The row identity this entry continues, claimed so no two rows take the
 * same one: the identity of a previous row that still holds one of these
 * members, else this entry's own key, else a value of its own (its key is
 * another row's identity only when that row carried it here).
 */
function claimRowKey(
  shown: readonly ProjectWithCounts[],
  key: string,
  id: string,
  rowKeyOf: ReadonlyMap<string, string>,
  claimed: Set<string>,
): string {
  let rowKey: string | undefined;
  for (const member of shown) {
    const carried = rowKeyOf.get(member.project.id);
    if (carried !== undefined && !claimed.has(carried)) {
      rowKey = carried;
      break;
    }
  }
  rowKey ??= claimed.has(key) ? `${key}\u0000${id}` : key;
  claimed.add(rowKey);
  return rowKey;
}

function sameEntry(a: SidebarEntry, b: SidebarEntry): boolean {
  return a.id === b.id
    && a.rowKey === b.rowKey
    && a.backend === b.backend
    && a.name === b.name
    && a.path === b.path
    && a.createdAt === b.createdAt
    && a.threadCount === b.threadCount
    && a.position === b.position
    && a.expanded === b.expanded
    // The warnings are a projection of the members, so identical members are
    // identical warnings.
    && a.members.length === b.members.length
    && a.members.every((member, index) => member === b.members[index]);
}
