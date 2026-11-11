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
import { PRConflictEntry, type ConflictRead, type PRConflicts } from './prReviewConflictsEntry.svelte';
import { errString } from '../utils/errors';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import type { WorkspaceRef } from '../types/git';
import type { PRDetail } from '../types/models';
import { prRevision } from '../utils/prRevision';

export type { PRConflicts };

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
  // A view shown again reads what it missed while it was off screen; the
  // runner decides whether anything is owed.
  conflictsByKey.get(holdKey)?.kick();
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
    entry = new PRConflictEntry(key, workspace, () => viewHolds.has(id), readConflictTree);
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
 * that read. The open settles once a read that covers its snapshot (or a
 * newer one) has settled, or the PR's last holder has left.
 */
export function openPRConflicts(
  key: string,
  workspace: WorkspaceRef,
  ref: PRRef,
  detail: PRDetail,
): Promise<void> {
  return ensureConflicts(key, workspace).open(ref, detail, prRevision(detail));
}

/** The runner's one read: the tree, then every conflicted file. Never
 * rejects; true when the tree was read. */
async function readConflictTree(entry: PRConflictEntry, read: ConflictRead): Promise<boolean> {
  const { prKey, workspace } = entry;
  const { revision, detail } = read;
  const headRefName = detail.headRefName;
  const epoch = ++entry.epoch;
  entry.treeError = null;
  entry.errorByPath.clear();
  entry.state = null;
  entry.contentByPath.clear();
  entry.inFlight.clear();
  try {
    const result = await withBackendTarget(workspaceKeyBackend(prKey), () => GetPRMergeConflicts(
      workspace,
      prReferenceWire(read.ref),
      revision,
      headRefName,
      read.userInitiated,
    ));
    if (epoch !== entry.epoch) return true;
    entry.state = {
      treeOID: String(result.treeOID ?? ''),
      baseLabel: String(result.baseLabel ?? `origin/${revision.baseRef}`),
      headLabel: String(result.headLabel ?? headRefName),
      paths: result.conflicted ? [...(result.paths ?? [])] : [],
      notes: result.conflicted ? { ...result.notes } : {},
      messages: result.conflicted ? [...(result.messages ?? [])] : [],
      revision,
    };
    // Conflict files render expanded, so their content is fetched here
    // (one local git read per file, in parallel). A file whose read fails
    // keeps its error and is the one thing a pane leaves collapsed.
    await Promise.all(entry.state.paths.map((path) => loadConflictFile(entry, path)));
    return true;
  } catch (err) {
    if (epoch === entry.epoch) entry.treeError = errString(err);
    return false;
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
  const epoch = entry.epoch;
  const treeOID = entry.state.treeOID;
  const { prKey, workspace } = entry;
  const load = (async () => {
    try {
      const content = await withBackendTarget(workspaceKeyBackend(prKey), () => GetMergeConflictFile(workspace, treeOID, path));
      if (epoch !== entry.epoch) return;
      entry.contentByPath.set(path, String(content ?? ''));
      // Only THIS path's failure is resolved. The reads run in parallel, so
      // clearing a shared slot here dismissed a sibling's banner while that
      // sibling still had no body to render.
      entry.errorByPath.delete(path);
    } catch (err) {
      if (epoch !== entry.epoch) return;
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
  const revision = prRevision(detail, liveHeadSHA);
  for (const entry of conflictsByKey.values()) {
    if (entry.prKey === key) entry.frame(ref, detail, revision);
  }
}

/**
 * Drop a PR's conflict state in every checkout. A retired entry stops its
 * runner, releases every open waiting on it, and drops the answers of
 * reads still in flight instead of resurrecting an entry nobody holds.
 */
export function dropPRConflicts(key: string): void {
  for (const [id, entry] of conflictsByKey) {
    if (entry.prKey !== key) continue;
    entry.retire();
    conflictsByKey.delete(id);
  }
}

/** Test seam: drop every entry and permit, as a fresh module load would. */
export function __resetPRConflictsForTest(): void {
  for (const entry of conflictsByKey.values()) dropPRConflicts(entry.prKey);
  viewHolds.clear();
}
