// Merge-conflict state, keyed by PR.
//
// The merged tree and every conflicted file's content belong to the pull
// request: one merge-tree run and one set of file reads serve every pane
// looking at it. Like the CI cache next door it is not SOURCED by the PR
// subscription — nothing computes a tree until a pane opens the conflict
// view — so it lives beside the snapshot store and is dropped through
// that store's `onDrop` when the last holder of the PR goes.

import { SvelteMap } from 'svelte/reactivity';
import { GetMergeConflictFile, GetPRMergeConflicts } from './bindings';
import { errString } from '../utils/errors';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import type { PRDetail } from '../types/models';

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
  /** The pair the merged tree was computed from. `treeOID` names a tree
   * that only exists for THIS pair; when either moves the tree is gone,
   * and every file read against it would answer for a merge nobody
   * asked about. */
  baseRefName: string;
  headSHA: string;
}

class PRConflictEntry {
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
  /** The thread whose clone computed the tree — merge-tree needs a local
   * checkout, and a head move has to recompute without an attacher
   * present to supply one. */
  threadId = '';
  /** The (base, head) pair that moved WHILE a load was running. The view is
   * open, so it has to converge on it once the in-flight load settles —
   * dropping it left the pane pinned to a superseded merge forever, because
   * the poll only fires on CHANGE and no later event re-states the pair. */
  pending: { ref: PRRef; detail: PRDetail } | null = null;
  seq = 0;

  /** Read the parked pair and clear it. A method rather than an inline
   * read-and-null in the load's `finally`: `pending` is written by a
   * reconcile that lands DURING the awaited load, which TypeScript's
   * assignment narrowing does not model — an inline read after the
   * `pending = null` at the load's start narrows to `null` and the replay
   * becomes unreachable code the compiler rejects. */
  takePending(): { ref: PRRef; detail: PRDetail } | null {
    const pending = this.pending;
    this.pending = null;
    return pending;
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
const conflictsByKey = new SvelteMap<string, PRConflictEntry>();

// Keys whose conflict view is on screen somewhere. Only those reconcile a
// head move eagerly: recomputing a merged tree runs `git merge-tree` and
// then one read per conflicted file, and doing that for a surface nobody
// is looking at turns a background poll into work nobody asked for.
// Refcounted — two panes can have the view open on one PR — and a closed
// view is not a correctness hole: openPRConflicts recomputes whenever the
// entry's (base, head) pair no longer matches the detail it is handed.
const viewHolds = new Map<string, number>();

/**
 * Declare that a conflict view is on screen for this PR. Held for exactly
 * as long as the surface renders; the returned release is idempotent.
 */
export function permitPRConflictReconcile(key: string): () => void {
  viewHolds.set(key, (viewHolds.get(key) ?? 0) + 1);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const remaining = (viewHolds.get(key) ?? 1) - 1;
    if (remaining > 0) viewHolds.set(key, remaining);
    else viewHolds.delete(key);
  };
}

function ensureConflicts(key: string): PRConflictEntry {
  let entry = conflictsByKey.get(key);
  if (!entry) {
    entry = new PRConflictEntry();
    conflictsByKey.set(key, entry);
  }
  return entry;
}

/** Reactive read; a PR whose conflicts were never opened reads as empty. */
export function peekPRConflicts(key: string | null): PRConflictsView {
  if (key === null) return EMPTY_CONFLICTS;
  return conflictsByKey.get(key) ?? EMPTY_CONFLICTS;
}

/**
 * Compute (or reuse) the merged tree for a PR and fetch every conflicted
 * file's content. Reuses a tree already computed for the same base/head —
 * a second pane opening the view pays nothing, and so does a view
 * reopening onto a PR that has not moved since it was closed.
 */
export async function openPRConflicts(
  key: string,
  threadId: string,
  ref: PRRef,
  detail: PRDetail,
): Promise<void> {
  const entry = ensureConflicts(key);
  const headSHA = String(detail.headSHA ?? '');
  const baseRefName = String(detail.baseRefName ?? '');
  if (entry.loading) return;
  if (entry.state && entry.state.headSHA === headSHA && entry.state.baseRefName === baseRefName) {
    return;
  }
  await loadPRConflicts(entry, key, threadId, ref, detail);
}

async function loadPRConflicts(
  entry: PRConflictEntry,
  key: string,
  threadId: string,
  ref: PRRef,
  detail: PRDetail,
): Promise<void> {
  const seq = ++entry.seq;
  entry.threadId = threadId;
  entry.loading = true;
  entry.treeError = null;
  entry.errorByPath.clear();
  entry.state = null;
  entry.pending = null;
  entry.contentByPath.clear();
  entry.inFlight.clear();
  try {
    const result = await GetPRMergeConflicts(
      threadId,
      prReferenceWire(ref),
      detail.baseRefName,
      detail.headRefName,
    );
    if (seq !== entry.seq) return;
    entry.state = {
      treeOID: String(result.treeOID ?? ''),
      baseLabel: String(result.baseLabel ?? `origin/${detail.baseRefName}`),
      headLabel: String(result.headLabel ?? detail.headRefName),
      paths: result.conflicted ? [...(result.paths ?? [])] : [],
      notes: result.conflicted ? { ...(result.notes ?? {}) } : {},
      messages: result.conflicted ? [...(result.messages ?? [])] : [],
      baseRefName: String(detail.baseRefName ?? ''),
      headSHA: String(detail.headSHA ?? ''),
    };
    entry.treeError = null;
    // Conflict files render expanded, so their content is fetched here
    // (one local git read per file, in parallel). A file whose read fails
    // keeps its error and is the one thing a pane leaves collapsed.
    await Promise.all(entry.state.paths.map((path) => ensurePRConflictFile(key, path)));
  } catch (err) {
    if (seq !== entry.seq) return;
    entry.treeError = errString(err);
  } finally {
    if (seq === entry.seq) {
      entry.loading = false;
      // A pair that moved while this load ran is not a lost update: settle
      // into it now. reconcile re-checks the pair, so a pending request the
      // completed load already satisfied costs nothing.
      const pending = entry.takePending();
      if (pending) {
        reconcileConflictsWithHead(key, pending.ref, pending.detail, String(pending.detail.headSHA ?? ''));
      }
    }
  }
}

/** Load one conflicted file's merged content; a no-op once it is present. */
export async function ensurePRConflictFile(key: string, path: string): Promise<void> {
  const entry = conflictsByKey.get(key);
  if (!entry || entry.contentByPath.has(path)) return;
  const inFlight = entry.inFlight.get(path);
  if (inFlight) {
    await inFlight;
    return;
  }
  const seq = entry.seq;
  const treeOID = entry.state?.treeOID ?? '';
  const threadId = entry.threadId;
  const load = (async () => {
    try {
      const content = await GetMergeConflictFile(threadId, treeOID, path);
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
 * A conflict tree describes ONE (base, head) pair. When the PR moves under
 * it — a push, a base change, a rebase — the tree OID names an object that
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
  if (!viewHolds.has(key)) return;
  const entry = conflictsByKey.get(key);
  if (!entry || !detail || !ref) return;
  const headSHA = String(detail.headSHA ?? liveHeadSHA ?? '');
  const baseRefName = String(detail.baseRefName ?? '');
  if (headSHA === '' && baseRefName === '') return;
  if (entry.loading) {
    // The pair moved while the merge-tree run (and its per-file reads) were
    // still going. Park it: the load in flight answers for a merge nobody
    // asked about any more, and dropping the new pair left the open view on
    // it forever — later polls dedup to nothing, so no event re-states it.
    entry.pending = { ref, detail };
    return;
  }
  // Compared against the pair recorded ON the entry, not the previous
  // snapshot: after a reconnect there is no previous snapshot, and the
  // tree is stale all the same.
  if (!entry.state) {
    // A FAILED load is not a settled answer about the pair that moved under
    // it — the view would sit on that error forever, because later polls
    // dedup to nothing. Only a view that never loaded at all (no state, no
    // error) has nothing here to reconcile.
    if (entry.treeError === null) return;
  } else if (entry.state.headSHA === headSHA && entry.state.baseRefName === baseRefName) {
    return;
  }
  if (!entry.threadId) return;
  void loadPRConflicts(entry, key, entry.threadId, ref, detail);
}

/**
 * Drop a PR's conflict state. The token is bumped BEFORE the delete so
 * in-flight loads drop their results instead of resurrecting an entry
 * nobody holds.
 */
export function dropPRConflicts(key: string): void {
  const entry = conflictsByKey.get(key);
  if (!entry) return;
  entry.seq += 1;
  conflictsByKey.delete(key);
}

/** Test seam: drop every entry and permit, as a fresh module load would. */
export function __resetPRConflictsForTest(): void {
  for (const key of [...conflictsByKey.keys()]) dropPRConflicts(key);
  viewHolds.clear();
}
