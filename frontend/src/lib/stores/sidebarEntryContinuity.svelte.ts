// Which listed project rows form each sidebar entry, which earlier row each
// entry continues, and where the disclosure that follows it is written.
//
// A repository checked out on several computers is one sidebar entry. Its
// `key` is what its fold and show-more limit are persisted under: the
// repository key of a computer's chosen checkout, else the project's own id
// (`projectEntryKey`). That key moves while the row stays: a computer verifies
// a checkout's identity after listing it or loses it, and archiving or
// deleting a chosen checkout promotes another one on that computer. So one
// decision, "this entry continues that earlier row", answers both what the
// row's DOM identity is (`rowKey`) and where its record goes when its key
// moves. This module is that decision's one owner, and the only place listed
// rows are grouped into entries; the sidebar's projection
// (components/sidebar/sidebarEntries.ts) adds only what the device filter
// decides.
//
// Continuity is decided over every listed member with the device filter
// ignored, in three passes: a row that keeps its key and shares a member,
// then a row that shares a member (a key that moved, a merge, a split), then
// a new row under its own key unless a continuing row carries that identity.
// What follows for the records, applied as one change:
//   - an entry whose key moved takes the record of the row it continues;
//   - a merge keeps the continuing row's record and clears the other row's,
//     which would otherwise come back when that member splits off again;
//   - a row that splits off takes a copy of the record it split from, so
//     whichever half is on screen keeps the fold;
//   - a row whose members are all gone (deleted, detached) keeps its record.
//
// The derivation reads a record where it lives now (`recordKey`): under the
// entry's key once committed, under the key of the row it continues until
// then. So nothing computes a wrong fold while a change waits. The change is
// committed by one effect, which runs whether or not the sidebar is mounted,
// and by every disclosure writer here before it writes, so no write can land
// on a key whose record is still on its way.
//
// The records are this module's alone: it creates them and exports nothing
// that reads or writes one by key, so no other module can read a record that
// is on its way or write one under a key that is about to move. They are new
// records because a project's disclosure used to be keyed by its own id,
// before a repository checked out on several computers became one entry, and
// that cannot be migrated: an id cannot be turned into an entry key without
// every computer's project rows, which the sidebar has not read when this
// module initializes. Separate records are what makes the old state
// unreachable rather than merely unread; every project opens once.
import { untrack } from 'svelte';
import type { ProjectWithCounts } from '../types/models';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { projectBackend } from '../transport/entityIndex';
import { entryIdFor, getProjects, projectEntryKey } from './projects.svelte';
import { catalogTreeListed } from './sidebarCatalogs';
import { keySetRecord, listLimitRecord, type RecordKeyChanges } from './sidebar.svelte';

// An entry's fold, inverted: the keys of the entries the user collapsed.
const collapsed = keySetRecord('sidebar:collapsedProjectEntries');
// How many rows an entry's own list shows, by entry key.
const listLimits = listLimitRecord('sidebar:projectEntryListLimits');

/** One entry of the sidebar's listed rows, the device filter ignored. */
export interface SidebarEntryGroup {
  /** What the entry's disclosure is persisted under. */
  readonly key: string;
  /** DOM identity: carried from the row this entry continues. */
  readonly rowKey: string;
  /** Every listed member, in store order. */
  readonly members: readonly ProjectWithCounts[];
  /**
   * Where the entry's disclosure record lives now: `key`, or until a change
   * is committed, the key of the row whose record it takes.
   */
  readonly recordKey: string;
}

/** A row as remembered for the next decision. */
interface RememberedRow {
  readonly rowKey: string;
  readonly key: string;
  readonly memberIds: ReadonlySet<string>;
}

interface Continuity {
  readonly groups: readonly SidebarEntryGroup[];
  /** Member project id → the rowKey of its entry. */
  readonly rowOf: ReadonlyMap<string, string>;
  /** What to remember: the previous array itself when nothing changed. */
  readonly rows: readonly RememberedRow[];
  readonly changes: RecordKeyChanges | null;
}

interface ListedGroup {
  readonly key: string;
  readonly members: readonly ProjectWithCounts[];
}

// Written only by `commit`.
let remembered: readonly RememberedRow[] = $state.raw([]);

function listedGroups(): ListedGroup[] {
  const listed = new Map<BackendKey, boolean>();
  const byEntry = new Map<string, ProjectWithCounts[]>();
  for (const row of getProjects()) {
    const backend = projectBackend(row.project.id) ?? HOME_BACKEND;
    let isListed = listed.get(backend);
    if (isListed === undefined) listed.set(backend, isListed = catalogTreeListed(backend));
    if (!isListed) continue;
    const entry = entryIdFor(row.project.id);
    const members = byEntry.get(entry);
    if (members === undefined) byEntry.set(entry, [row]);
    else members.push(row);
  }
  // Every member of an entry wears its key: a merged entry holds one chosen
  // checkout per computer, and all of them wear the repository key.
  return [...byEntry.values()].map((members) => ({ key: projectEntryKey(members[0]!.project.id), members }));
}

function decide(previous: readonly RememberedRow[], listed: readonly ListedGroup[]): Continuity {
  const rowOfMember = new Map<string, RememberedRow>();
  for (const row of previous) for (const id of row.memberIds) rowOfMember.set(id, row);
  const continued: (RememberedRow | undefined)[] = listed.map(() => undefined);
  const claimed = new Set<RememberedRow>();
  const claim = (index: number, sameKey: boolean): void => {
    const group = listed[index]!;
    for (const member of group.members) {
      const row = rowOfMember.get(member.project.id);
      if (row === undefined || claimed.has(row) || (sameKey && row.key !== group.key)) continue;
      claimed.add(row);
      continued[index] = row;
      return;
    }
  };
  listed.forEach((_, index) => claim(index, true));
  listed.forEach((_, index) => { if (continued[index] === undefined) claim(index, false); });

  const taken = new Set<string>();
  for (const row of continued) if (row !== undefined) taken.add(row.rowKey);
  const rowOf = new Map<string, string>();
  const groups = listed.map((group, index): SidebarEntryGroup => {
    const row = continued[index];
    let rowKey: string;
    let recordKey: string;
    if (row !== undefined) {
      rowKey = row.rowKey;
      recordKey = row.key;
    } else {
      rowKey = freshRowKey(group, taken);
      // Every row a member of this group was in went to another group, so
      // this one split off: it starts with the record it split from.
      const source = group.members.map((member) => rowOfMember.get(member.project.id)).find((found) => found !== undefined);
      recordKey = source?.key ?? group.key;
    }
    for (const member of group.members) rowOf.set(member.project.id, rowKey);
    return { key: group.key, rowKey, members: group.members, recordKey };
  });

  return { groups, rowOf, rows: rememberedRows(previous, groups, continued), changes: changesOf(previous, groups) };
}

function freshRowKey(group: ListedGroup, taken: Set<string>): string {
  let rowKey = group.key;
  for (let attempt = 0; taken.has(rowKey); attempt++) {
    rowKey = `${group.key}\u0000${group.members[0]!.project.id}${attempt === 0 ? '' : `\u0000${attempt}`}`;
  }
  taken.add(rowKey);
  return rowKey;
}

function changesOf(previous: readonly RememberedRow[], groups: readonly SidebarEntryGroup[]): RecordKeyChanges | null {
  const copies = new Map<string, string>();
  for (const group of groups) if (group.recordKey !== group.key) copies.set(group.key, group.recordKey);
  const keys = new Set(groups.map((group) => group.key));
  const present = new Set(groups.flatMap((group) => group.members.map((member) => member.project.id)));
  const clears = new Set<string>();
  for (const row of previous) {
    if (keys.has(row.key)) continue;
    // A row whose members went into other entries: its record moved, was
    // copied or lost a merge. One whose members are all gone keeps it.
    if ([...row.memberIds].some((id) => present.has(id))) clears.add(row.key);
  }
  return copies.size === 0 && clears.size === 0 ? null : { copies, clears };
}

function rememberedRows(
  previous: readonly RememberedRow[],
  groups: readonly SidebarEntryGroup[],
  continued: readonly (RememberedRow | undefined)[],
): readonly RememberedRow[] {
  const unchanged = groups.length === previous.length && groups.every((group, index) => {
    const row = continued[index];
    return row !== undefined && row.key === group.key && row.memberIds.size === group.members.length
      && group.members.every((member) => row.memberIds.has(member.project.id));
  });
  if (unchanged) return previous;
  return groups.map((group) => ({
    rowKey: group.rowKey,
    key: group.key,
    memberIds: new Set(group.members.map((member) => member.project.id)),
  }));
}

// Reads the project rows and the listing, never disclosure and never live
// activity, so a streaming beat or a fold does not regroup.
const continuity = $derived(decide(remembered, listedGroups()));

/** The sidebar's listed entries, in store order. */
export function sidebarEntryGroups(): readonly SidebarEntryGroup[] {
  return continuity.groups;
}

/** The rowKey of the entry `projectId` is listed in, or '' when it is in none. */
export function sidebarEntryRowOf(projectId: string): string {
  return continuity.rowOf.get(projectId) ?? '';
}

/** Whether an entry is unfolded, read where its record lives now. */
export function entryExpanded(group: SidebarEntryGroup): boolean {
  return !collapsed.has(group.recordKey);
}

/** How many rows an entry's own list shows, read where its record lives now. */
export function entryListLimit(group: SidebarEntryGroup): number {
  return listLimits.limitOf(group.recordKey);
}

// Commit the decision: the record changes it implies, and the rows it
// remembers. Untracked, so a writer called from an effect does not subscribe
// that effect to the sidebar's rows.
function commit(): void {
  untrack(() => {
    const next = continuity;
    if (next.changes !== null) {
      collapsed.apply(next.changes);
      listLimits.apply(next.changes);
    }
    if (next.rows !== remembered) remembered = next.rows;
  });
}

/** Fold or unfold the entry persisted under `key`. */
export function setEntryExpanded(key: string, expanded: boolean): void {
  commit();
  collapsed.set(key, !expanded);
}

/** Show `limit` rows of the entry's own list. */
export function setEntryListLimit(key: string, limit: number): void {
  commit();
  listLimits.setLimit(key, limit);
}

/** Put the entry's own list back to its preview size. */
export function resetEntryListLimit(key: string): void {
  commit();
  listLimits.clear(key);
}

/**
 * Unfold the entry a project belongs to so a row inside it can mount: what
 * every caller that has just created or moved a thread wants.
 */
export function revealProject(projectId: string): void {
  commit();
  collapsed.set(untrack(() => projectEntryKey(projectId)), false);
}

let stopEffect: (() => void) | null = null;

/**
 * Start committing continuity as it changes, for as long as the app runs.
 * Idempotent; returns the stop.
 */
export function startSidebarEntryContinuity(): () => void {
  stopEffect ??= $effect.root(() => {
    $effect.pre(() => {
      void continuity;
      commit();
    });
  });
  return stopSidebarEntryContinuity;
}

export function stopSidebarEntryContinuity(): void {
  stopEffect?.();
  stopEffect = null;
}

/**
 * Re-read the records after hydration lands the durable per-client bucket,
 * beside the rest of the sidebar's view state (syncSidebarFromAppStorage).
 */
export function syncSidebarEntryDisclosureFromAppStorage(): void {
  collapsed.reload();
  listLimits.reload();
}

/** Test seam: what the records hold under `key`, read without a move. */
export function entryRecordForTest(key: string): { expanded: boolean; listLimit: number } {
  return { expanded: !collapsed.has(key), listLimit: listLimits.limitOf(key) };
}

/** Test seam: stop the effect, forget every remembered row and the records' in-memory copies. */
export function resetSidebarEntryContinuityForTest(): void {
  stopSidebarEntryContinuity();
  remembered = [];
  collapsed.resetForTest();
  listLimits.resetForTest();
}
