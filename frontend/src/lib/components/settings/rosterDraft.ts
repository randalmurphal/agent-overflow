// One roster list's draft in the settings page (AgentProfilesSettings): the
// entries the person edits, each beside the saved entry it was edited from,
// so a change saved elsewhere replaces what is unchanged here and keeps
// what the person changed.
//
// Policy for an entry changed both here and elsewhere: the person's version
// stays in the draft, marked with the other version, and the list is not
// saved until they keep theirs (keepMine, which a save then writes over the
// other version) or take the other version. An entry is matched
// to its saved counterpart by value, then by `counterpart` (a profile's
// name); a note has no identity, so a note changed both places is kept
// beside the other version. An entry deleted here but changed elsewhere
// comes back as changed there.
//
// A deletion is the deleted entry itself, kept in `removed`: what it deletes
// is its origin, so a save that carried the entry before it was deleted
// moves the deletion with it, and an entry added and deleted before its
// first save deletes nothing unless that save stores it.

export interface DraftEntry<T> {
  /** What the inputs edit. */
  value: T;
  /** The saved entry `value` was edited from; null when added here. */
  origin: T | null;
  /**
   * Set when the saved list changed this entry too while it had edits here:
   * the other version, or null when the entry was changed or removed there
   * beyond recognition.
   */
  theirs?: T | null;
}

export interface Draft<T> {
  entries: DraftEntry<T>[];
  /** Entries deleted here whose deletion is not saved yet. */
  removed: DraftEntry<T>[];
}

function clone<T>(value: T): T {
  return { ...value };
}

/** Whether two entries hold the same fields. */
export function same<T extends object>(a: T, b: T): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const key of keys) {
    if ((a as Record<string, unknown>)[key] !== (b as Record<string, unknown>)[key]) return false;
  }
  return true;
}

export function draftOf<T extends object>(saved: readonly T[]): Draft<T> {
  return { entries: saved.map((entry) => ({ value: clone(entry), origin: clone(entry) })), removed: [] };
}

/** Whether an entry holds something the saved list does not. */
export function dirty<T extends object>(entry: DraftEntry<T>, trim: (value: T) => T): boolean {
  return entry.origin === null || entry.theirs !== undefined || !same(trim(entry.value), entry.origin);
}

function take<T extends object>(saved: readonly T[], used: Set<number>, match: (entry: T) => boolean): number {
  const at = saved.findIndex((entry, index) => !used.has(index) && match(entry));
  if (at >= 0) used.add(at);
  return at;
}

/**
 * The draft after the saved list became `saved`: the saved entries, with
 * each dirty entry kept in place of the one it was edited from, and entries
 * deleted here left out.
 */
export function reconcile<T extends object>(
  draft: Draft<T>,
  saved: readonly T[],
  trim: (value: T) => T,
  counterpart?: (origin: T, entry: T) => boolean,
): Draft<T> {
  const used = new Set<number>();
  const kept = new Map<number, DraftEntry<T>>();
  const added: DraftEntry<T>[] = [];
  for (const entry of draft.entries) {
    if (!dirty(entry, trim)) continue;
    const origin = entry.origin;
    if (origin === null) {
      added.push(entry);
      continue;
    }
    let at = take(saved, used, (candidate) => same(candidate, origin));
    if (at >= 0) {
      kept.set(at, entry);
      continue;
    }
    at = counterpart ? take(saved, used, (candidate) => counterpart(origin, candidate)) : -1;
    if (at >= 0) {
      kept.set(at, { value: entry.value, origin: clone(saved[at]), theirs: clone(saved[at]) });
      continue;
    }
    added.push({ value: entry.value, origin: null, theirs: null });
  }
  const removed: DraftEntry<T>[] = [];
  const skipped = new Set<number>();
  for (const gone of draft.removed) {
    const origin = gone.origin;
    if (origin === null) continue;
    const at = take(saved, used, (candidate) => same(candidate, origin));
    if (at < 0) continue;
    skipped.add(at);
    removed.push(gone);
  }
  const entries: DraftEntry<T>[] = [];
  saved.forEach((entry, index) => {
    if (skipped.has(index)) return;
    entries.push(kept.get(index) ?? { value: clone(entry), origin: clone(entry) });
  });
  return { entries: [...entries, ...added], removed };
}

/** Deletes the entry at `index`: it waits in `removed` until a save carries the deletion. */
export function remove<T extends object>(draft: Draft<T>, index: number): void {
  const [entry] = draft.entries.splice(index, 1);
  if (entry) draft.removed.push(entry);
}

/**
 * The draft once `sent` is saved: each entry the save took has what was sent
 * as its origin and no mark, deleted since or not, and the deletions the save
 * carried are no longer pending. Typing that followed the snapshot stays
 * dirty against it; an entry added or deleted since stays unsaved.
 */
export function markSaved<T extends object>(
  draft: Draft<T>,
  entries: readonly DraftEntry<T>[],
  sent: readonly T[],
  removed: readonly DraftEntry<T>[],
): void {
  entries.forEach((entry, index) => {
    entry.origin = clone(sent[index]);
    delete entry.theirs;
  });
  draft.removed = draft.removed.filter((gone) => !removed.includes(gone));
}

/** Whether two lists hold the same entries in the same order. */
export function sameList<T extends object>(a: readonly T[], b: readonly T[]): boolean {
  return a.length === b.length && a.every((entry, index) => same(entry, b[index]));
}

/** Whether an entry waits on the person to keep theirs or take the other. */
export function marked<T extends object>(draft: Draft<T>): boolean {
  return draft.entries.some((entry) => entry.theirs !== undefined);
}

/** Whether the draft holds anything the saved list does not. */
export function unsaved<T extends object>(draft: Draft<T>, trim: (value: T) => T): boolean {
  return draft.removed.some((gone) => gone.origin !== null) || draft.entries.some((entry) => dirty(entry, trim));
}

/**
 * The draft with a marked entry's mark cleared: the person's version stays
 * and is unsaved against the other version, so the next save writes it.
 */
export function keepMine<T extends object>(draft: Draft<T>, index: number): void {
  const entry = draft.entries[index];
  if (entry) delete entry.theirs;
}

/**
 * The draft with a marked entry put back as the saved list has it: the
 * other version, or gone when the saved list no longer has one.
 */
export function takeTheirs<T extends object>(draft: Draft<T>, index: number): void {
  const entry = draft.entries[index];
  if (!entry || entry.theirs === undefined) return;
  if (entry.theirs === null) {
    draft.entries.splice(index, 1);
    return;
  }
  draft.entries[index] = { value: clone(entry.theirs), origin: clone(entry.theirs) };
}
