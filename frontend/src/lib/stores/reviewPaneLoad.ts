// The review pane's LOAD half: what a diff request is, how a selection is
// resolved against a freshly-fetched list, and what the resulting patch
// collapses by default.
//
// Everything here is a pure function of its arguments — no runes, no
// closure over pane state, no reactive store reads. That is the boundary:
// the factory in `reviewPane.svelte.ts` owns reactive pane state and calls
// into this module, never the other way round, which is what keeps the
// selection resolvers and the diff-source switch testable on their own.
//
// The one lookup that is not an argument is the transport's entity index
// (`onThreadBackend` below), because the pr scope has to name a machine
// that its zero workspace ref cannot. That index is a plain Map keyed by
// id, not pane state, so the functions here stay callable from a test with
// nothing mounted.

import {
  GetBranchBaseDiff,
  GetCommitDiff,
  GetPRCommitDiff,
  GetPRDiff,
  GetPayloadData,
  GetTurnEditsDiff,
  GetWorkspaceCurrentDiff,
  GitListBranches,
  ListBranchCommits,
  ListPRCommits,
  ListThreadEditDiffs,
} from './bindings';
import type { PRSnapshot } from './prReviewStore.svelte';
import type { BranchCommit, GitBranch, WorkspaceRef } from '../types/git';
import type { DiffReviewComment, DiffReviewScope, ReviewLineComment } from '../types/models';
import { seedPayloadPatchSpans } from '../utils/diffSpanCache.svelte';
import { filePatchDisplayRows, type PatchFile } from '../utils/patchFiles';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import { withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { SvelteSet } from 'svelte/reactivity';

/** Whether the diff for this selection comes from `internal/gitdiff`, the
 * only source that can apply `-w`:
 *
 * - workspace / branch — `GetWorkspaceCurrentDiff` / `GetBranchBaseDiff`.
 * - any selected commit, including in pr scope — `GetCommitDiff` /
 *   `GetPRCommitDiff`.
 * - pr whole-diff — NO. It is a local `git diff --merge-base` when the
 *   thread has a clone and the forge's own diff API when it doesn't, and
 *   the API cannot ignore whitespace at all. A toggle that quietly worked
 *   only for cloned threads is worse than no toggle.
 * - edits — NO. Those are persisted tool-call patches replayed verbatim,
 *   never a git recomputation.
 *
 * Shared by the toolbar's enablement and by loadPatch, so the control can
 * never offer a mode the load path won't deliver. */
export function supportsIgnoreWhitespace(
  scope: DiffReviewScope,
  selectedCommitSHA: string | null,
): boolean {
  if (selectedCommitSHA) return scope === 'branch' || scope === 'pr';
  return scope === 'workspace' || scope === 'branch';
}

/** One edit tool call in the Edits selector — metadata only, the diff
 * loads on selection. Mirrors the ListThreadEditDiffs wire entry. */
export interface EditDiffEntryView {
  itemId: string;
  payloadId: string;
  turnIndex: number;
  title: string;
  paths: string[];
  insertions: number;
  deletions: number;
  createdAt: number;
}

/** Edits-scope selection: one tool call's diff, or a whole turn's
 * edits concatenated in order. */
export type EditSelection =
  | { kind: 'item'; itemId: string; payloadId: string }
  | { kind: 'turn'; turnIndex: number };

/** Selector `<option>` value encoding for an EditSelection. */
export function editSelectionKey(selection: EditSelection | null): string | null {
  if (!selection) return null;
  return selection.kind === 'item' ? `item:${selection.itemId}` : `turn:${selection.turnIndex}`;
}

export interface LoadedPatch {
  patchText: string;
  /** Commit selector rows for the loaded range; omitted → empty. */
  commits?: BranchCommit[];
  /** The commit the diff was actually computed for — a stale selection
   * that left the range resolves back to null (full range). */
  selectedCommitSHA?: string | null;
  /** Edit selector rows (edits scope); omitted → empty. */
  edits?: EditDiffEntryView[];
  editTurnLabels?: ReadonlyMap<number, string>;
  /** The edit selection the diff was actually computed for — a pinned
   * or stale selection resolves against the fresh list. */
  selectedEdit?: EditSelection | null;
  /** pr scope: the head the diff was computed at. The pane keeps it as
   * the anchor `prStale` is measured from. */
  prHeadSHA?: string;
}

/** The edits-scope selection reload should aim for: a freshly pinned
 * tool call (inline-diff affordance) wins over the current selection. */
export interface EditDesire {
  pinnedItemId: string | null;
  current: EditSelection | null;
}

/** State a selection-only reload reuses instead of refetching: the
 * commit/edit lists are unchanged by picking an entry. */
export interface ExistingLoad {
  commits: BranchCommit[];
  edits: EditDiffEntryView[];
  editTurnLabels: ReadonlyMap<number, string>;
}

/** Decode a selector value (`item:<id>` / `turn:<n>`) against the
 * current entries; unknown or null values resolve to the default. */
export function editSelectionFromKey(
  key: string | null,
  entries: readonly EditDiffEntryView[],
): EditSelection | null {
  if (key) {
    if (key.startsWith('item:')) {
      const itemId = key.slice('item:'.length);
      const entry = entries.find((candidate) => candidate.itemId === itemId);
      if (entry) return { kind: 'item', itemId: entry.itemId, payloadId: entry.payloadId };
    } else if (key.startsWith('turn:')) {
      const turnIndex = Number(key.slice('turn:'.length));
      if (entries.some((candidate) => candidate.turnIndex === turnIndex)) {
        return { kind: 'turn', turnIndex };
      }
    }
  }
  return defaultEditSelection(entries);
}

/** Default edits-scope selection: the latest turn's whole set. */
export function defaultEditSelection(entries: readonly EditDiffEntryView[]): EditSelection | null {
  if (entries.length === 0) return null;
  return { kind: 'turn', turnIndex: entries[entries.length - 1].turnIndex };
}

/** Validate a desired selection against the fresh list: a pinned tool
 * call wins; a stale selection falls back to the default. */
export function resolveEditSelection(
  desire: EditDesire,
  entries: readonly EditDiffEntryView[],
): EditSelection | null {
  if (desire.pinnedItemId) {
    const pinned = entries.find((candidate) => candidate.itemId === desire.pinnedItemId);
    if (pinned) return { kind: 'item', itemId: pinned.itemId, payloadId: pinned.payloadId };
  }
  const current = desire.current;
  if (current?.kind === 'item' && entries.some((candidate) => candidate.itemId === current.itemId)) {
    return current;
  }
  if (current?.kind === 'turn' && entries.some((candidate) => candidate.turnIndex === current.turnIndex)) {
    return current;
  }
  return defaultEditSelection(entries);
}

/** Validate a selection against the fresh list, not just the diff call:
 * after a rebase, base change, or force-push the selected SHA can vanish
 * — fall back to the full range instead of erroring. */
export function resolveSelectedCommit(
  selectedCommitSHA: string | null,
  commits: readonly BranchCommit[],
): string | null {
  if (selectedCommitSHA && commits.some((commit) => commit.sha === selectedCommitSHA)) {
    return selectedCommitSHA;
  }
  return null;
}

/**
 * Who the diff is FOR. Every live scope (workspace, branch, commit, pr) is a
 * fact about the checkout and needs only `workspace`; `edits` replays this
 * THREAD's persisted tool-call patches and is the one scope that needs a
 * real thread row. Null `threadId` is a draft placeholder — it has no
 * history to replay, so requesting edits with one is a caller bug and throws
 * rather than silently loading nothing.
 */
/** Raised wherever the edits scope is reached without a thread row. The
 *  scope's subject IS the thread's own edit history, so there is nothing to
 *  fall back to — the option is not offered on a draft placeholder, and both
 *  the load path and the context-expansion path say so in the same words. */
export const EDITS_NEEDS_THREAD =
  'The Edits view needs a started thread — its subject is the thread\'s own history.';

export interface DiffSubject {
  workspace: WorkspaceRef;
  threadId: string | null;
}

/**
 * Issue one PR-scope call on the THREAD's machine.
 *
 * The pr scope is the only diff source whose workspace ref can be the zero
 * ref (a pr-anchor thread has no local clone), and the `workspace` route
 * reads a machine out of the ref's project id. A zero ref therefore names
 * nobody and resolves home, which sends a second machine's PR straight to
 * the wrong forge credentials and the wrong clone. The thread is the only
 * subject left that still knows where the PR lives, so it is what names the
 * backend.
 *
 * A real ref already routes correctly, and pinning it to the same machine
 * the ref resolves to changes nothing, so this wraps every pr call rather
 * than branching on the ref, and there is no zero-ref special case to
 * forget. `issue` must dispatch exactly one RPC synchronously, the same
 * contract `withBackendTarget` states.
 */
function onThreadBackend<T>(threadId: string | null, issue: () => T): T {
  const owner = threadId === null ? undefined : threadBackend(threadId);
  return owner === undefined ? issue() : withBackendTarget(owner, issue);
}

export async function loadPatch(
  subject: DiffSubject,
  scope: DiffReviewScope,
  baseBranch: string | null,
  selectedCommitSHA: string | null,
  prRef: PRRef | null,
  // pr scope: the shared PR snapshot the caller already awaited. The diff
  // needs its base ref, so it is an input here rather than something this
  // function fetches.
  prSnapshot: PRSnapshot | null,
  editDesire: EditDesire,
  // `-w`, applied only by the branches whose binding accepts it — see
  // supportsIgnoreWhitespace. The unsupported calls take no such argument,
  // so the flag structurally cannot reach a source that would ignore it.
  ignoreWhitespace: boolean,
  existing?: ExistingLoad,
): Promise<LoadedPatch> {
  const { workspace, threadId } = subject;
  switch (scope) {
    case 'pr': {
      const detail = prSnapshot?.detail;
      if (!prRef || !detail) throw new Error('No PR or MR is available for this thread.');
      const pr = prReferenceWire(prRef);
      // The detail's baseRefName is what lets the backend compute a local
      // three-dot diff (gh/glab's PR-diff API caps at 20k lines; large PRs
      // must go through the local-clone path).
      const baseRef = detail.baseRefName ?? '';
      const headSHA = String(prSnapshot.headSHA || detail.headSHA || '');
      // Per-commit PR review needs the local clone; without one the backend
      // returns an empty list and the selector stays hidden. The known head
      // SHA lets the backend skip its fetch when the objects are local.
      const commits = existing
        ? existing.commits
        : baseRef
          ? (((await onThreadBackend(threadId, () => ListPRCommits(workspace, pr, baseRef, headSHA))) ??
              []) as BranchCommit[])
          : [];
      const commitSHA = resolveSelectedCommit(selectedCommitSHA, commits);
      // GetPRDiff takes no ignoreWhitespace: the PR whole-diff can come from
      // the forge API, which cannot ignore whitespace at all.
      const patchText = commitSHA
        ? String(
            (await onThreadBackend(threadId, () =>
              GetPRCommitDiff(workspace, pr, commitSHA, ignoreWhitespace),
            )) ?? '',
          )
        : String((await onThreadBackend(threadId, () => GetPRDiff(workspace, pr, baseRef))) ?? '');
      return { patchText, commits, selectedCommitSHA: commitSHA, prHeadSHA: headSHA };
    }
    case 'workspace':
      return {
        patchText: ((await GetWorkspaceCurrentDiff(workspace, ignoreWhitespace)) ?? '') as string,
      };
    case 'edits': {
      if (threadId === null) throw new Error(EDITS_NEEDS_THREAD);
      let entries = existing?.edits;
      let turnLabels = existing?.editTurnLabels;
      if (!entries || !turnLabels) {
        const list = await ListThreadEditDiffs(threadId);
        entries = (list?.entries ?? []).map((entry) => ({
          itemId: String(entry.itemId),
          payloadId: String(entry.payloadId),
          turnIndex: Number(entry.turnIndex),
          title: String(entry.title ?? ''),
          paths: (entry.paths ?? []).map(String),
          insertions: Number(entry.insertions ?? 0),
          deletions: Number(entry.deletions ?? 0),
          createdAt: Number(entry.createdAt ?? 0),
        }));
        turnLabels = new Map((list?.turnLabels ?? []).map((label) => [Number(label.turnIndex), String(label.label ?? '')]));
      }
      const selection = resolveEditSelection(editDesire, entries);
      let patchText = '';
      if (selection?.kind === 'item') {
        const payload = await GetPayloadData(threadId, selection.payloadId);
        patchText = String(payload?.data ?? '');
        // Persist-time spans travel with the data (primed when the file
        // still matched at edit time) — seed them so the first paint is
        // colored without the RPC path. Fire-and-forget cache warmer.
        void seedPayloadPatchSpans(threadId, payload?.patchSpans);
      } else if (selection) {
        const turnDiff = await GetTurnEditsDiff(threadId, selection.turnIndex);
        patchText = String(turnDiff?.data ?? '');
        void seedPayloadPatchSpans(threadId, turnDiff?.patchSpans);
      }
      return { patchText, edits: entries, editTurnLabels: turnLabels, selectedEdit: selection };
    }
    case 'branch': {
      const branch = baseBranch?.trim() || await defaultBaseBranch(workspace);
      if (existing) {
        const commitSHA = resolveSelectedCommit(selectedCommitSHA, existing.commits);
        const patchText = commitSHA
          ? ((await GetCommitDiff(workspace, commitSHA, ignoreWhitespace)) ?? '') as string
          : ((await GetBranchBaseDiff(workspace, branch, ignoreWhitespace)) ?? '') as string;
        return { patchText, commits: existing.commits, selectedCommitSHA: commitSHA };
      }
      if (selectedCommitSHA) {
        // Sequenced: the selection must be validated against the fresh
        // list before deciding which diff to fetch.
        const commits = ((await ListBranchCommits(workspace, branch)) ?? []) as BranchCommit[];
        const commitSHA = resolveSelectedCommit(selectedCommitSHA, commits);
        const patchText = commitSHA
          ? ((await GetCommitDiff(workspace, commitSHA, ignoreWhitespace)) ?? '') as string
          : ((await GetBranchBaseDiff(workspace, branch, ignoreWhitespace)) ?? '') as string;
        return { patchText, commits, selectedCommitSHA: commitSHA };
      }
      const [commits, patchText] = await Promise.all([
        ListBranchCommits(workspace, branch).then((rows) => (rows ?? []) as BranchCommit[]),
        GetBranchBaseDiff(workspace, branch, ignoreWhitespace).then((patch) => (patch ?? '') as string),
      ]);
      return { patchText, commits, selectedCommitSHA: null };
    }
  }
}

export async function defaultBaseBranch(workspace: WorkspaceRef): Promise<string> {
  const branches = ((await GitListBranches(workspace)) ?? []) as GitBranch[];
  const defaultBranch = branches.find((branch) => branch.isDefault);
  if (!defaultBranch?.name) {
    throw new Error('default branch not found');
  }
  return defaultBranch.name;
}

export function defaultCollapsedPaths(files: readonly PatchFile[]): SvelteSet<string> {
  const collapsed = new SvelteSet<string>();
  for (const file of files) {
    if (isLockfileish(file.path) || filePatchDisplayRows(file).length > 400) {
      collapsed.add(file.path);
    }
  }
  return collapsed;
}

export function reviewLineCommentForDraft(comment: DiffReviewComment): ReviewLineComment | null {
  if (comment.side === 'file') {
    return { path: comment.filePath, body: comment.body, side: 'file' };
  }
  if (comment.side === 'old' && comment.oldLine) {
    return { path: comment.filePath, body: comment.body, line: comment.oldLine, side: 'left' };
  }
  if (comment.side === 'new' && comment.newLine) {
    return { path: comment.filePath, body: comment.body, line: comment.newLine, side: 'right' };
  }
  if (comment.side === 'context' && comment.newLine) {
    return { path: comment.filePath, body: comment.body, line: comment.newLine, side: 'right' };
  }
  return null;
}

export function draftAnchorExists(files: readonly PatchFile[], comment: DiffReviewComment): boolean {
  if (comment.side === 'file') return files.some((file) => file.path === comment.filePath);
  const file = files.find((candidate) => candidate.path === comment.filePath);
  if (!file) return false;
  const rows = filePatchDisplayRows(file);
  return rows.some((row) => {
    if (row.gap) return false;
    if (comment.side === 'old') return row.side === 'old' && row.oldLine === comment.oldLine;
    if (comment.side === 'new') return row.side === 'new' && row.newLine === comment.newLine;
    return row.oldLine === comment.oldLine && row.newLine === comment.newLine;
  });
}

function isLockfileish(path: string): boolean {
  const name = path.split('/').pop() ?? path;
  return name === 'pnpm-lock.yaml' ||
    name === 'package-lock.json' ||
    name === 'go.sum' ||
    name.endsWith('.lock');
}
