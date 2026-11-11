import { withBackendTarget } from '../transport/backends';
import { workspaceKeyBackend } from '../utils/workspaceKey';
// Merge-conflict state, keyed by PR and checkout.
//
// The merged tree and every conflicted file's content belong to the pull
// request as computed in one local checkout: one merge-tree run and one set
// of file reads serve every pane on that PR in that checkout. Panes in
// different checkouts get separate entries, so each computes against a
// clone it can address and none depends on another's checkout staying
// registered. It is not SOURCED by the PR subscription (nothing computes a
// tree until a pane opens the conflict view), so it lives beside the
// snapshot store, and every checkout's entry is dropped through that
// store's `onDrop` when the last holder of the PR goes.

import { SvelteMap } from 'svelte/reactivity';
import { GetMergeConflictFile, GetPRMergeConflicts } from './bindings';
import { errString } from '../utils/errors';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import type { WorkspaceRef } from '../types/git';
import type { PRDetail } from '../types/models';
import { prRevision, prRevisionKey, type PRRevision } from '../utils/prRevision';

export interface PRConflicts {
  treeOID: string;
  baseLabel: string;
  headLabel: string;
  paths: string[];
  /** Per-path merge-tree messages — the only signal for non-textual
   * conflicts (modify/delete, rename/rename, …); rendered as marker
   * rows at the top of that file's body. */
  notes: Partial<Record<string, string[]>>;
  /** Fallback strip: messages that name no conflicted path (rare). */
  messages: string[];
  /** The revision the read was asked at. The backend merges its head with
   * the base branch's tip as origin has it during the read, not with
   * `revision.baseSHA` (the base the forge recorded, which does not follow
   * the branch). A head or base-branch move makes the tree answer for a
   * merge nobody asked about. */
  revision: PRRevision;
}

/** A snapshot parked to be read after the read in flight. */
interface PendingRead {
  ref: PRRef;
  detail: PRDetail;
  /** A person asked for this read (an open), so it may prompt for git
   * credentials; a reconcile's read may not. */
  userInitiated: boolean;
}

/**
 * What a read answers for: the revision and the forge's mergeability. A
 * base branch that moves changes no revision field, but the forge reports
 * what the move did to the merge in its mergeability. Two snapshots with
 * one key are answered by one read; this is the only comparison an open
 * or a reconcile makes against the read in flight or the one on screen.
 */
function conflictReadKey(revision: PRRevision, detail: PRDetail): string {
  return `${prRevisionKey(revision)}\n${String(detail.mergeability ?? '')}`;
}

class PRConflictEntry {
  constructor(
    readonly prKey: string,
    /** The checkout that computes the tree. merge-tree needs a local clone,
     * and a head move recomputes without an attacher present to supply one. */
    readonly workspace: WorkspaceRef,
  ) {}

  state = $state<PRConflicts | null>(null);
  loading = $state(false);
  /** The merge-tree computation's own failure. */
  treeError = $state<string | null>(null);
  /** Per-path content-read failures. Per PATH because the reads run in
   * PARALLEL: one shared slot let a later file's success clear an earlier
   * file's banner while that file was still contentless, so the pane
   * reported a healthy conflict view with a hole in it. */
  readonly errorByPath = new SvelteMap<string, string>();
  readonly contentByPath = new SvelteMap<string, string>();
  /** In-flight per-path content loads, so N panes expanding the same file
   * share one read. */
  readonly inFlight = new Map<string, Promise<void>>();
  /** The tree read in flight, so an open during it settles with its answer. */
  load: Promise<void> | null = null;
  /** conflictReadKey of the read in flight, else of the last read. */
  readKey = '';
  /** The revision that moved WHILE a load was running, or that a view
   * opened before the forge reported one (GitLab preparing the diff). The
   * view is open, so it has to converge on it — dropping it left the pane
   * pinned to a superseded merge forever, because the poll only fires on
   * CHANGE and no later event re-states the revision. */
  pending: PendingRead | null = null;
  seq = 0;

  /** Read the parked pair and clear it. A method rather than an inline
   * read-and-null in the load's `finally`: `pending` is written by a
   * reconcile that lands DURING the awaited load, which TypeScript's
   * assignment narrowing does not model — an inline read after the
   * `pending = null` at the load's start narrows to `null` and the replay
   * becomes unreachable code the compiler rejects. */
  takePending(): PendingRead | null {
    const pending = this.pending;
    this.pending = null;
    return pending;
  }

  /** Park a snapshot to read after the read in flight. The newest snapshot
   * wins, and a person's ask survives a later automatic one. */
  park(ref: PRRef, detail: PRDetail, userInitiated: boolean): void {
    this.pending = { ref, detail, userInitiated: userInitiated || (this.pending?.userInitiated ?? false) };
  }

  /** One banner for the surface: the tree failure if there is one, else the
   * first file that could not be read (named, since its body is missing). */
  get error(): string | null {
    if (this.treeError !== null) return this.treeError;
    const first = this.errorByPath.entries().next();
    if (first.done) return null;
    const [path, message] = first.value;
    return `${path}: ${message}`;
  }
}

export interface PRConflictsView {
  readonly state: PRConflicts | null;
  readonly loading: boolean;
  readonly error: string | null;
  readonly contentByPath: SvelteMap<string, string>;
}

const EMPTY_CONFLICT_CONTENT = new SvelteMap<string, string>();
const EMPTY_CONFLICTS: PRConflictsView = Object.freeze({
  state: null,
  loading: false,
  error: null,
  contentByPath: EMPTY_CONFLICT_CONTENT,
});
// Entries by `entryKey(prKey, workspace)`.
const conflictsByKey = new SvelteMap<string, PRConflictEntry>();

// Entry keys whose conflict view is on screen somewhere. Only those
// reconcile a head move eagerly: recomputing a merged tree runs
// `git merge-tree` and then one read per conflicted file, and doing that for
// a surface nobody is looking at turns a background poll into work nobody
// asked for. Refcounted, since two panes can have the view open on one
// entry, and a closed view is not a correctness hole: openPRConflicts
// reads again on every open.
const viewHolds = new Map<string, number>();

// The PR key already names the computer, so the checkout is identified by
// the ref's own fields.
function entryKey(prKey: string, workspace: WorkspaceRef): string {
  return JSON.stringify([prKey, workspace.projectId, workspace.workspacePath]);
}

/**
 * Declare that a conflict view is on screen for this PR in this checkout.
 * Held for exactly as long as the surface renders; the returned release is
 * idempotent.
 */
export function permitPRConflictReconcile(key: string, workspace: WorkspaceRef): () => void {
  const holdKey = entryKey(key, workspace);
  viewHolds.set(holdKey, (viewHolds.get(holdKey) ?? 0) + 1);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const remaining = (viewHolds.get(holdKey) ?? 1) - 1;
    if (remaining > 0) viewHolds.set(holdKey, remaining);
    else viewHolds.delete(holdKey);
  };
}

function ensureConflicts(key: string, workspace: WorkspaceRef): PRConflictEntry {
  const id = entryKey(key, workspace);
  let entry = conflictsByKey.get(id);
  if (!entry) {
    entry = new PRConflictEntry(key, workspace);
    conflictsByKey.set(id, entry);
  }
  return entry;
}

/** Reactive read; a PR whose conflicts were never opened in this checkout
 *  reads as empty. */
export function peekPRConflicts(key: string | null, workspace: WorkspaceRef): PRConflictsView {
  if (key === null) return EMPTY_CONFLICTS;
  return conflictsByKey.get(entryKey(key, workspace)) ?? EMPTY_CONFLICTS;
}

/**
 * Compute the merged tree for a PR in a checkout and fetch every conflicted
 * file's content. Every open reads again: the merge is against the base
 * branch's live tip, which can move without any change to the revision the
 * pane holds. An open while a read of the same snapshot is in flight shares
 * that read; an open handed another snapshot parks it to read next. Either
 * way the open settles only once the snapshot it was handed (or a newer
 * one) has been read.
 */
export async function openPRConflicts(
  key: string,
  workspace: WorkspaceRef,
  ref: PRRef,
  detail: PRDetail,
): Promise<void> {
  const entry = ensureConflicts(key, workspace);
  const revision = prRevision(detail);
  if (revision === null) {
    // Nothing to merge yet; the snapshot that carries a revision reconciles.
    entry.park(ref, detail, true);
    return;
  }
  if (entry.load === null) {
    void startPRConflictsLoad(entry, ref, revision, detail, true);
  } else if (conflictReadKey(revision, detail) !== entry.readKey) {
    entry.park(ref, detail, true);
  }
  // The load's finally starts the parked read before it clears, so this
  // waits through it.
  while (entry.load) await entry.load;
}

function startPRConflictsLoad(
  entry: PRConflictEntry,
  ref: PRRef,
  revision: PRRevision,
  detail: PRDetail,
  userInitiated: boolean,
): Promise<void> {
  const load = loadPRConflicts(entry, ref, revision, detail, userInitiated);
  entry.load = load;
  return load;
}

async function loadPRConflicts(
  entry: PRConflictEntry,
  ref: PRRef,
  revision: PRRevision,
  detail: PRDetail,
  userInitiated: boolean,
): Promise<void> {
  const { prKey, workspace } = entry;
  const headRefName = detail.headRefName;
  const seq = ++entry.seq;
  entry.readKey = conflictReadKey(revision, detail);
  entry.loading = true;
  entry.treeError = null;
  entry.errorByPath.clear();
  entry.state = null;
  entry.pending = null;
  entry.contentByPath.clear();
  entry.inFlight.clear();
  try {
    const result = await withBackendTarget(workspaceKeyBackend(prKey), () => GetPRMergeConflicts(
      workspace,
      prReferenceWire(ref),
      revision,
      headRefName,
      userInitiated,
    ));
    if (seq !== entry.seq) return;
    entry.state = {
      treeOID: String(result.treeOID ?? ''),
      baseLabel: String(result.baseLabel ?? `origin/${revision.baseRef}`),
      headLabel: String(result.headLabel ?? headRefName),
      paths: result.conflicted ? [...(result.paths ?? [])] : [],
      notes: result.conflicted ? { ...result.notes } : {},
      messages: result.conflicted ? [...(result.messages ?? [])] : [],
      revision,
    };
    entry.treeError = null;
    // Conflict files render expanded, so their content is fetched here
    // (one local git read per file, in parallel). A file whose read fails
    // keeps its error and is the one thing a pane leaves collapsed.
    await Promise.all(entry.state.paths.map((path) => loadConflictFile(entry, path)));
  } catch (err) {
    if (seq !== entry.seq) return;
    entry.treeError = errString(err);
  } finally {
    if (seq === entry.seq) {
      entry.loading = false;
      entry.load = null;
      // A snapshot parked while this load ran is not a lost update: read it
      // now. An open's snapshot reads whether or not a view still holds the
      // entry, since the open is waiting on it; a reconcile's re-checks the
      // hold. Both skip a snapshot this load already answered.
      const pending = entry.takePending();
      if (pending) readParked(entry, pending);
    }
  }
}

/** Load one conflicted file's merged content; a no-op once it is present. */
export async function ensurePRConflictFile(key: string, workspace: WorkspaceRef, path: string): Promise<void> {
  const entry = conflictsByKey.get(entryKey(key, workspace));
  if (entry) await loadConflictFile(entry, path);
}

async function loadConflictFile(entry: PRConflictEntry, path: string): Promise<void> {
  // No tree yet (a read of it is in flight, or it failed): nothing names
  // the content, and the tree read fetches every conflicted file itself.
  if (entry.state === null) return;
  if (entry.contentByPath.has(path)) return;
  const inFlight = entry.inFlight.get(path);
  if (inFlight) {
    await inFlight;
    return;
  }
  const seq = entry.seq;
  const treeOID = entry.state?.treeOID ?? '';
  const { prKey, workspace } = entry;
  const load = (async () => {
    try {
      const content = await withBackendTarget(workspaceKeyBackend(prKey), () => GetMergeConflictFile(workspace, treeOID, path));
      if (seq !== entry.seq) return;
      entry.contentByPath.set(path, String(content ?? ''));
      // Only THIS path's failure is resolved. The reads run in parallel, so
      // clearing a shared slot here dismissed a sibling's banner while that
      // sibling still had no body to render.
      entry.errorByPath.delete(path);
    } catch (err) {
      if (seq !== entry.seq) return;
      entry.errorByPath.set(path, errString(err));
    } finally {
      entry.inFlight.delete(path);
    }
  })();
  entry.inFlight.set(path, load);
  await load;
}

/**
 * A conflict tree describes ONE merge. When the PR moves under it (a push,
 * a base change, a rebase, or the base branch moving, which the forge
 * reports as a mergeability change) the tree OID names an object that
 * answers for a merge that is no longer the one on screen; before this, a
 * moved head left the old tree in place and every file read after it
 * rendered the previous merge's content.
 *
 * Recompute rather than blank: whoever is LOOKING at the conflict view is
 * asking "does this merge cleanly", and the answer for the new head is the
 * honest one. Only for a PR whose view is actually open — the tree is
 * recomputed lazily on reopen otherwise, so a poll for a PR nobody is
 * inspecting stays a poll.
 */
export function reconcileConflictsWithHead(
  key: string,
  ref: PRRef | undefined,
  detail: PRDetail | null,
  liveHeadSHA: string,
): void {
  if (!detail || !ref) return;
  for (const entry of conflictsByKey.values()) {
    if (entry.prKey === key) reconcileEntryWithHead(entry, ref, detail, liveHeadSHA);
  }
}

function readParked(entry: PRConflictEntry, pending: PendingRead): void {
  const { ref, detail, userInitiated } = pending;
  if (!userInitiated) {
    reconcileEntryWithHead(entry, ref, detail, String(detail.headSHA ?? ''));
    return;
  }
  const revision = prRevision(detail);
  if (revision === null) return;
  if (entry.treeError === null && conflictReadKey(revision, detail) === entry.readKey) return;
  void startPRConflictsLoad(entry, ref, revision, detail, true);
}

function reconcileEntryWithHead(
  entry: PRConflictEntry,
  ref: PRRef,
  detail: PRDetail,
  liveHeadSHA: string,
): void {
  if (!viewHolds.has(entryKey(entry.prKey, entry.workspace))) return;
  const revision = prRevision(detail, liveHeadSHA);
  if (revision === null) return;
  const key = conflictReadKey(revision, detail);
  if (entry.loading) {
    // The snapshot moved while the merge-tree run (and its per-file reads)
    // were still going. Park it: the load in flight answers for a merge
    // nobody asked about any more, and dropping the new snapshot left the
    // open view on it forever (later polls dedup to nothing, so no event
    // re-states it).
    if (key !== entry.readKey) entry.park(ref, detail, false);
    return;
  }
  // Compared against the read recorded ON the entry, not the previous
  // snapshot: after a reconnect there is no previous snapshot, and the
  // tree is stale all the same.
  if (!entry.state) {
    // A FAILED load is not a settled answer about the snapshot: the next
    // frame retries it. A view parked waiting for a revision loads now.
    // Only a view that never loaded at all (no state, no error, nothing
    // parked) has nothing here to reconcile.
    const parked = entry.takePending();
    if (entry.treeError === null && parked === null) return;
    // A parked open keeps its ask through the reconcile that runs it.
    void startPRConflictsLoad(entry, ref, revision, detail, parked?.userInitiated ?? false);
    return;
  }
  if (key === entry.readKey) return;
  void startPRConflictsLoad(entry, ref, revision, detail, false);
}

/**
 * Drop a PR's conflict state in every checkout. The token is bumped BEFORE
 * the delete so in-flight loads drop their results instead of resurrecting
 * an entry nobody holds.
 */
export function dropPRConflicts(key: string): void {
  for (const [id, entry] of conflictsByKey) {
    if (entry.prKey !== key) continue;
    entry.seq += 1;
    conflictsByKey.delete(id);
  }
}

/** Test seam: drop every entry and permit, as a fresh module load would. */
export function __resetPRConflictsForTest(): void {
  for (const entry of conflictsByKey.values()) dropPRConflicts(entry.prKey);
  viewHolds.clear();
}
