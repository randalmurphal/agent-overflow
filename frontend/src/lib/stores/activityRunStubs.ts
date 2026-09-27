// The client run record: what a pane knows about one activity run whose
// members it holds only part of
// (docs/architecture/timeline-window-pages.md §6).
//
// A history page ships every prose row in its range but only a window of
// each activity run's members, plus a stub counting the rest. The pane
// therefore holds three descriptions of one run: the members loaded as
// `Item`s, narrow copies of members a window cut SHED, and the server's
// stub for everything outside the span the stub was built for. This
// module owns the arithmetic over those three — the held-window digest,
// the physical row count, and the header's inputs — and nothing else: no
// reactivity, no fetching, no rendering.
//
// Keyed by the run's `firstItemId`, which is stable for the life of the
// window: runs grow only at their newer end (new rows land at the write
// head; head-healed prompts are prose), so the first member never moves.

import type { Item } from '../types/models';
import type {
  ActivityRunGroup,
  ActivityRunGroupKey,
  ActivityRunStub,
} from '../../../bindings/agent-overflow/internal/store/models';
import {
  FNV1A64_ZERO,
  formatFnv1a64,
  parseFnv1a64,
  xorFnv1a64,
  type Fnv1a64,
} from '../utils/fnv1a';
import { fileChangeDisplayRowCount } from '../utils/fileChangeRows';
import { parseJsonObject } from '../utils/parseJsonObject';
import { completionEndsLaunch } from '../utils/parkedStop';
import { windowDigestRowHash } from './threadWindowDigest';
import type { ActivityRunSpan } from '../utils/activityRunSpans';

/**
 * A member the pane dropped from memory, narrowed to what the header and
 * the digest still need of it.
 *
 * The display-row count is resolved AT SHED TIME (`fileChangeDisplayRowCount`
 * reads `payloadMeta` and `meta`, which can be kilobytes of diff JSON), so
 * a shed row costs a handful of short strings instead of retaining the
 * blob the count came from. `mcp` is the same narrowing for the group
 * identity: the `{server, tool}` object as JSON text, exactly the field
 * the server puts on `ActivityRunGroupKey`.
 */
export interface ShedRow {
  id: string;
  rev: number;
  kind: string;
  toolName: string;
  status: string;
  completionOf: string;
  mcp: string;
  fileRows: number;
}

/** Narrow a loaded member into the copy a shed row keeps. */
export function shedRowOf(item: Item): ShedRow {
  return {
    id: item.id,
    rev: item.rev,
    kind: item.kind,
    toolName: item.toolName ?? '',
    status: item.status ?? '',
    completionOf: item.completionOf ?? '',
    mcp: mcpTextOf(item),
    fileRows: fileChangeDisplayRowCount(item),
  };
}

/** `json_extract(items.meta, '$.mcp')` as the client can read it. */
export function mcpTextOf(item: Item): string {
  const meta = parseJsonObject(item.meta);
  const mcp = meta?.mcp;
  if (mcp === null || mcp === undefined || typeof mcp !== 'object') return '';
  return JSON.stringify(mcp);
}

/** A timeline position: a row's `(turnIndex, itemIndex)`. */
export interface RunCoordinate {
  turnIndex: number;
  itemIndex: number;
}

export function compareCoordinates(a: RunCoordinate, b: RunCoordinate): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  return a.itemIndex - b.itemIndex;
}

/**
 * A row the pane refused because it belongs to a part of a held run the
 * pane does not hold (`threadActivityRuns.runCoveringUnshipped`), with the
 * key of that run's record.
 */
export interface RunRefusal {
  runKey: string;
  item: Item;
}

/**
 * One run the pane holds part of.
 *
 * `loadedFirstItemId` / `loadedLastItemId` are the span the PANE actually
 * holds, which is not always the span the stub was built for: a cursor
 * page can cross into a held run and ship a different slice of it, and a
 * live append extends the span past the one the server described. When
 * the two disagree the pane's span wins and the record is `dirty`: every
 * count derived from the stub then describes rows the pane no longer
 * holds exactly, so the record refreshes and contributes no held window
 * until it does. Two moves keep it clean, because the record accounts for
 * them itself: members shed from the span's older side (`shed`), and
 * members appended after a span that ends at the run's newest member,
 * which change nothing the stub counts (`extendOverTailAppend`).
 */
export interface ActivityRunRecord {
  /** The run's identity and the map key: its first physical member. */
  readonly runFirstItemId: string;
  stub: ActivityRunStub;
  loadedFirstItemId: string;
  loadedLastItemId: string;
  /**
   * Members the pane shed since the last stub, oldest first, always
   * contiguous with the loaded span on its OLDER side. A stub refresh or
   * a members response describes them again and clears the list.
   */
  shed: ShedRow[];
  dirty: boolean;
  /**
   * Bumped by every server stub the record takes. A newer one supersedes a
   * read in flight; a local extension of the stub does not.
   */
  stubVersion: number;
  /**
   * Bumped by local invalidations other than members appended after the
   * loaded span. A read already in flight cannot acknowledge them.
   */
  invalidationVersion: number;
  /**
   * Bumped by members appended after the loaded span. A read in flight
   * still describes its own span, and folding it extends the record over
   * them (`foldPageStub`), unless the read replaced the span.
   */
  appendVersion: number;
  /** A window cut supersedes an outstanding member-navigation request. */
  cutVersion: number;
  /**
   * The newest position of a row refused as a member of this run
   * (`noteRefusedRow`), until a members answer read after the refusal
   * settles it (`settleRefusedRows`). The record stays dirty meanwhile:
   * a page stub cannot settle it, because the page may have been read
   * before the row existed.
   */
  refusedThrough: RunCoordinate | null;
}

/** Records by `runFirstItemId`. */
export type ActivityRunRecords = Map<string, ActivityRunRecord>;

/**
 * Fold one stub, from a page or a members answer, into the records.
 *
 * `span` is the run's members as the pane holds them AFTER the rows
 * merged, or null when it holds none. The stub always replaces the stored
 * one, which it is newer than by construction, but the pane's span is
 * authoritative about what is loaded, so a stub describing a different
 * span leaves the record dirty and the shed list untouched. A stub that
 * agrees with the pane describes every member outside the span, shed rows
 * included, so it clears them.
 *
 * A stub whose span ended at the run's newest member when it was read also
 * agrees with a span that continues past that member: the members after
 * it were appended since. The record then takes the stub's span, clean,
 * and the caller moves it over the appended members
 * (`extendOverTailAppend`) or re-points it (`noteSpanMoved`).
 *
 * A record with an unsettled refusal (`refusedThrough`) stays dirty
 * whatever the stub describes.
 */
export function foldPageStub(
  records: ActivityRunRecords,
  stub: ActivityRunStub,
  span: ActivityRunSpan | null,
): ActivityRunRecord {
  const key = stub.firstItemId;
  const loadedFirstItemId = span?.firstItemId ?? '';
  const describedLastItemId = stub.loadedFirstItemId === loadedFirstItemId
    ? describedLastMember(stub, span)
    : null;
  const existing = records.get(key);
  const record: ActivityRunRecord = existing ?? {
    runFirstItemId: key,
    stub,
    loadedFirstItemId,
    loadedLastItemId: '',
    shed: [],
    dirty: false,
    stubVersion: 0,
    invalidationVersion: 0,
    appendVersion: 0,
    cutVersion: 0,
    refusedThrough: null,
  };
  record.stub = stub;
  record.stubVersion += 1;
  record.loadedFirstItemId = loadedFirstItemId;
  if (describedLastItemId !== null) {
    record.loadedLastItemId = describedLastItemId;
    record.shed = [];
    record.dirty = record.refusedThrough !== null;
  } else {
    record.loadedLastItemId = span?.lastItemId ?? '';
    record.dirty = true;
  }
  records.set(key, record);
  return record;
}

/**
 * The member of `span` a stub's span ends at, or null when the stub does
 * not describe the span's newer side: its last member, or an earlier one
 * that was the run's newest when the stub was read.
 */
function describedLastMember(stub: ActivityRunStub, span: ActivityRunSpan | null): string | null {
  const last = span?.lastItemId ?? '';
  if (stub.loadedLastItemId === last) return last;
  return span !== null && spanEndsRun(stub) && span.items.some(item => item.id === stub.loadedLastItemId)
    ? stub.loadedLastItemId
    : null;
}

/**
 * Whether a stub's span ended at its run's newest member: it counts no
 * member after the span. Callers look the span's last member up in the
 * pane's span, which an empty span (every member counted before it) never
 * matches.
 */
function spanEndsRun(stub: ActivityRunStub): boolean {
  return stub.unshippedAfter === 0;
}

/**
 * Re-point a record at the span the pane holds now, without a server
 * answer. Used when a wholesale item replacement moved a run's loaded
 * members (a streamed append `extendOverTailAppend` cannot fold, a
 * reconcile). The stub still describes the old span, so the record goes
 * dirty and a refresh restates it. A span that only gained members after
 * its newer end counts as an append (`appendVersion`), which a read in
 * flight still describes. Returns whether the span moved.
 */
export function noteSpanMoved(
  record: ActivityRunRecord,
  span: ActivityRunSpan,
): boolean {
  const first = span.firstItemId;
  const last = span.lastItemId;
  const previousLast = record.loadedLastItemId;
  if (record.loadedFirstItemId === first && previousLast === last) return false;
  const appended = record.loadedFirstItemId === first && previousLast !== ''
    && span.items.some(item => item.id === previousLast);
  record.loadedFirstItemId = first;
  record.loadedLastItemId = last;
  if (appended) {
    record.appendVersion += 1;
    record.dirty = true;
  } else {
    invalidateActivityRun(record);
  }
  return true;
}

/**
 * Extend a record over members appended at its run's newer end, without a
 * server answer. The stub describes only the members outside its span, and
 * when that span ends at the run's newest member no row landing after it
 * changes them, unless it is a completion of a launch outside the run's
 * loaded span. The record then takes the stub a refresh would return: the
 * same aggregates with the run's newer edge, member count and loaded span
 * moved to the new last member. An answer read before the append does not
 * count the appended members; folding it applies this rule to them again
 * (`appendVersion`). Returns false, changing nothing, for any other change;
 * the caller re-points the record instead (`noteSpanMoved`).
 */
export function extendOverTailAppend(
  record: ActivityRunRecord,
  span: ActivityRunSpan,
): boolean {
  const stub = record.stub;
  const previousLast = record.loadedLastItemId;
  if (record.dirty || !spanEndsRun(stub)
    || span.firstItemId !== record.loadedFirstItemId || span.lastItemId === previousLast) {
    return false;
  }
  const at = span.items.findIndex(item => item.id === previousLast);
  if (at < 0) return false;
  const held = new Set(span.items.map(item => item.id));
  for (let index = at + 1; index < span.items.length; index += 1) {
    const completionOf = span.items[index].completionOf ?? '';
    if (completionOf !== '' && !held.has(completionOf)) return false;
  }
  const last = span.items[span.items.length - 1];
  record.stub = {
    ...stub,
    lastItemId: last.id,
    lastTurnIndex: last.turnIndex,
    lastItemIndex: last.itemIndex,
    memberCount: stub.memberCount + span.items.length - 1 - at,
    loadedLastItemId: last.id,
  };
  record.loadedLastItemId = last.id;
  record.appendVersion += 1;
  return true;
}

export function invalidateActivityRun(record: ActivityRunRecord): void {
  record.invalidationVersion += 1;
  record.dirty = true;
}

/**
 * Account for a row the pane refused as a member of this run: the record
 * goes dirty and keeps the newest refused position for the refresh to
 * check (`settleRefusedRows`).
 */
export function noteRefusedRow(record: ActivityRunRecord, row: RunCoordinate): void {
  invalidateActivityRun(record);
  const through = record.refusedThrough;
  if (through && compareCoordinates(row, through) <= 0) return;
  record.refusedThrough = { turnIndex: row.turnIndex, itemIndex: row.itemIndex };
}

/**
 * Settle a record's refused rows with a members answer read after the
 * newest of them, clearing `refusedThrough`. A run is contiguous and
 * every refused row lies at or after its first member, so all of them are
 * members when the answer's run ends at or after the newest one. Returns
 * true when it ends before it: that row is in no part of the run, and the
 * pane holds it nowhere.
 */
export function settleRefusedRows(record: ActivityRunRecord, stub: ActivityRunStub): boolean {
  const through = record.refusedThrough;
  if (!through) return false;
  record.refusedThrough = null;
  return compareCoordinates({ turnIndex: stub.lastTurnIndex, itemIndex: stub.lastItemIndex }, through) < 0;
}

/**
 * Record narrow copies of the members a window cut dropped from the
 * OLDER side of a run's loaded span.
 *
 * `dropped` is those members oldest-first; `nextFirstItemId` /
 * `nextLastItemId` are the span that survives. Shed rows are the digest's
 * and the header's only account of those rows until the next stub, so a
 * gap between the shed list and the surviving span would silently
 * under-count the run. Both halves are checked and a violation throws:
 * the caller is the retention cut, which computes the drop from the same
 * span this record names, so a mismatch is a defect, not an input.
 */
export function shedOlderMembers(
  record: ActivityRunRecord,
  dropped: readonly Item[],
  nextFirstItemId: string,
  nextLastItemId: string,
): void {
  if (dropped.length === 0) return;
  if (dropped[0].id !== record.loadedFirstItemId) {
    throw new Error(
      `activity run ${record.runFirstItemId}: shed rows start at ${dropped[0].id}, `
        + `not at the loaded span's first member ${record.loadedFirstItemId}`,
    );
  }
  if (nextLastItemId !== record.loadedLastItemId) {
    throw new Error(
      `activity run ${record.runFirstItemId}: a cut may only shed the loaded span's `
        + `older side, but the span's newest member moved from `
        + `${record.loadedLastItemId} to ${nextLastItemId}`,
    );
  }
  if (nextFirstItemId === '') {
    throw new Error(
      `activity run ${record.runFirstItemId}: shedding every loaded member leaves no `
        + 'span for the shed rows to be contiguous with; drop the record instead',
    );
  }
  for (const item of dropped) record.shed.push(shedRowOf(item));
  record.loadedFirstItemId = nextFirstItemId;
}

/**
 * Physical rows this record accounts for BEYOND the pane's loaded items:
 * everything shed plus every member outside the stub's span. The pane
 * counts its own loaded rows (§5).
 */
export function physicalCount(record: ActivityRunRecord): number {
  return record.shed.length + record.stub.unshippedBefore + record.stub.unshippedAfter;
}

/**
 * This record's contribution to the held-window digest: the shed rows'
 * hashes XORed with the stub's `unshippedDigest`.
 *
 * Null when the contribution cannot be stated — the record is dirty, a
 * shed row carries no usable revision, or the stub's digest is not this
 * algorithm's output. The pane then sends no held window at all, which
 * costs one page.
 */
export function windowDigestContribution(record: ActivityRunRecord): Fnv1a64 | null {
  if (record.dirty) return null;
  const unshipped = parseFnv1a64(record.stub.unshippedDigest);
  if (!unshipped) return null;
  let folded = unshipped;
  for (const row of record.shed) {
    if (row.rev < 0) return null;
    folded = xorFnv1a64(folded, windowDigestRowHash(row));
  }
  return folded;
}

/**
 * Fold every record into one held-window contribution, or null when any
 * of them cannot state theirs.
 */
export function heldRunsFold(
  records: ActivityRunRecords,
): { count: number; digest: Fnv1a64 } | null {
  let count = 0;
  let digest = FNV1A64_ZERO;
  for (const record of records.values()) {
    const contribution = windowDigestContribution(record);
    if (!contribution) return null;
    count += physicalCount(record);
    digest = xorFnv1a64(digest, contribution);
  }
  return { count, digest };
}

/**
 * What the run's header needs from the parts of the run the pane does not
 * hold as items (§4). Shed rows are members like any other and are
 * classified live; the stub's fields are the server's aggregate of the
 * rest, produced by the same rules.
 */
export interface ActivityRunStubFacts {
  memberCount: number;
  /** Members earlier than the loaded span: the stub's count plus the shed rows. */
  unshippedBefore: number;
  unshippedAfter: number;
  unshippedGroups: readonly ActivityRunGroup[];
  unshippedPairedLaunchIds: readonly string[];
  shippedSupersededLaunchIds: readonly string[];
  unshippedFailed: boolean;
  runningBefore: ActivityRunGroupKey | null;
  runningAfter: ActivityRunGroupKey | null;
  shed: readonly ShedRow[];
  loadedFirstItemId: string;
  loadedLastItemId: string;
}

/** Read a record's header and boundary inputs. */
export function stubFacts(record: ActivityRunRecord): ActivityRunStubFacts {
  const stub = record.stub;
  return {
    memberCount: stub.memberCount,
    unshippedBefore: stub.unshippedBefore + record.shed.length,
    unshippedAfter: stub.unshippedAfter,
    unshippedGroups: stub.unshippedGroups,
    unshippedPairedLaunchIds: stub.unshippedPairedLaunchIds,
    shippedSupersededLaunchIds: stub.shippedSupersededLaunchIds,
    unshippedFailed: stub.unshippedFailed,
    runningBefore: stub.runningBefore,
    runningAfter: stub.runningAfter,
    shed: record.shed,
    loadedFirstItemId: record.loadedFirstItemId,
    loadedLastItemId: record.loadedLastItemId,
  };
}

/**
 * Restate a record as ONE stub that also accounts for its shed rows.
 *
 * The live pane keeps shed rows separate because it can classify them
 * itself (`stubFacts`). A window written to a cache cannot: it is restored
 * with neither the shed rows nor the `Item`s they came from, so everything
 * they contribute has to be inside the stub by then. This folds them in
 * the way the server's byte trim folds a shipped member back out of a page
 * (`foldShippedMember`, older side, oldest-first), which is what keeps a
 * restored window's digest and header identical to one the server would
 * have produced for the same span.
 *
 * `loadedMembers` is the run's surviving loaded members, needed for the
 * pairing facts: whether a shed completion's launch is a member of the
 * run (it then counts zero rows), and which unshipped launches a
 * completion the window still holds names (`unshippedPairedLaunchIds`).
 * A shed launch whose ending completion is unshipped is named by the
 * stub's `shippedSupersededLaunchIds`; shedding it makes both halves
 * unshipped, so it leaves that list.
 *
 * Null when the record cannot state its contribution — dirty, or a stub
 * digest this build cannot parse. The caller must not persist a window it
 * cannot describe.
 */
export function foldedStub(
  record: ActivityRunRecord,
  loadedMembers: readonly Item[],
): ActivityRunStub | null {
  if (record.dirty) return null;
  let digest = parseFnv1a64(record.stub.unshippedDigest);
  if (!digest) return null;
  if (record.shed.length === 0) return record.stub;

  // Members the pane can name: the rows it holds, the copies it shed, and
  // the launches the stub already names as members (`UnshippedPairedLaunchIDs`
  // exists precisely because their completion is one of these rows). Members
  // it cannot name are already inside the stub's own aggregate.
  const memberIds = new Set<string>(record.stub.unshippedPairedLaunchIds);
  for (const row of record.shed) memberIds.add(row.id);
  for (const item of loadedMembers) memberIds.add(item.id);
  // Launches an ending completion member supersedes, whether or not the
  // window keeps that completion. A parked stop supersedes nothing
  // (`completionEndsLaunch`).
  const completedByMember = new Set<string>(record.stub.shippedSupersededLaunchIds);
  // Launches a completion the window keeps names, of any status: they pair.
  const pairedByLoaded = new Set<string>();
  for (const row of record.shed) {
    if (completionEndsLaunch(row) && memberIds.has(row.completionOf)) completedByMember.add(row.completionOf);
  }
  for (const item of loadedMembers) {
    const of = item.kind === 'tool_completion' ? item.completionOf ?? '' : '';
    if (of === '' || !memberIds.has(of)) continue;
    pairedByLoaded.add(of);
    if (completionEndsLaunch(item)) completedByMember.add(of);
  }

  const groups = new Map<string, ActivityRunGroup>();
  for (const group of record.stub.unshippedGroups) {
    groups.set(groupMapKey(group), { ...group });
  }
  let unshippedBefore = record.stub.unshippedBefore;
  let unshippedFailed = record.stub.unshippedFailed;
  let runningBefore = record.stub.runningBefore;
  let superseded = [...record.stub.shippedSupersededLaunchIds];

  for (const row of record.shed) {
    digest = xorFnv1a64(digest, windowDigestRowHash(row));
    const completionOfMember =
      row.kind === 'tool_completion'
      && row.completionOf !== ''
      && memberIds.has(row.completionOf)
        ? row.completionOf
        : '';
    const displayRows = completionOfMember === '' ? row.fileRows : 0;
    if (displayRows > 0) {
      const key: ActivityRunGroupKey = {
        kind: row.kind,
        toolName: row.toolName,
        mcp: row.mcp,
      };
      const mapKey = groupMapKey(key);
      const existing = groups.get(mapKey);
      if (existing) existing.rows += displayRows;
      else groups.set(mapKey, { ...key, rows: displayRows });
    }
    if (!completedByMember.has(row.id)) {
      if (row.status === 'errored' || row.status === 'killed') unshippedFailed = true;
      if (row.status === 'running' || row.status === 'streaming') {
        runningBefore = { kind: row.kind, toolName: row.toolName, mcp: row.mcp };
      }
    }
    unshippedBefore += 1;
    if (superseded.includes(row.id)) {
      superseded = superseded.filter((id) => id !== row.id);
    }
  }
  // The unshipped launches a kept completion names: the stub's own, and
  // the shed ones. A launch with several completions (a background
  // agent's parked stops and its ending one) stays paired while any of
  // them is kept.
  const paired = [...new Set([...record.stub.unshippedPairedLaunchIds, ...record.shed.map((row) => row.id)])]
    .filter((id) => pairedByLoaded.has(id))
    .sort();

  return {
    ...record.stub,
    loadedFirstItemId: record.loadedFirstItemId,
    loadedLastItemId: record.loadedLastItemId,
    unshippedBefore,
    unshippedDigest: formatFnv1a64(digest),
    unshippedGroups: sortedGroups(groups),
    unshippedPairedLaunchIds: paired,
    shippedSupersededLaunchIds: superseded,
    unshippedFailed,
    runningBefore,
  };
}

/** `(kind, toolName, mcp)` as one map key. The separator cannot occur in a kind. */
function groupMapKey(key: ActivityRunGroupKey): string {
  return `${key.kind}\u0000${key.toolName}\u0000${key.mcp}`;
}

/** Sorted by (kind, toolName, mcp), matching `sortedActivityRunGroups`. */
function sortedGroups(groups: ReadonlyMap<string, ActivityRunGroup>): ActivityRunGroup[] {
  return [...groups.values()].sort((a, b) => {
    if (a.kind !== b.kind) return a.kind < b.kind ? -1 : 1;
    if (a.toolName !== b.toolName) return a.toolName < b.toolName ? -1 : 1;
    if (a.mcp === b.mcp) return 0;
    return a.mcp < b.mcp ? -1 : 1;
  });
}

/**
 * Concatenate two pages' run descriptions, keyed by `firstItemId`.
 *
 * `next` is the page fetched LATER and wins every collision: it is the
 * more recent read of that run. A run the two pages split between them
 * ends up described by only one of their spans, which disagrees with the
 * span the concatenated window holds — `foldPageStub` sees that, marks
 * the record dirty, and one stub refresh restates it. That is the
 * designed cost of concatenating pages the server composed separately.
 */
export function mergeRunStubs(
  previous: readonly ActivityRunStub[] | null | undefined,
  next: readonly ActivityRunStub[] | null | undefined,
): ActivityRunStub[] {
  const byRun = new Map<string, ActivityRunStub>();
  for (const stub of previous ?? []) byRun.set(stub.firstItemId, stub);
  for (const stub of next ?? []) byRun.set(stub.firstItemId, stub);
  return [...byRun.values()];
}
