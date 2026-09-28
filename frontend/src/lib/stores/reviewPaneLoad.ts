// The review pane's LOAD half: what a diff request is, how a selection is
// resolved against a freshly-fetched list, and what the resulting patch
// collapses by default.
//
// Everything here is a pure function of its arguments — no runes, no
// closure over pane state, no reactive store reads. That is the boundary:
// the factory in `reviewPane.svelte.ts` owns reactive pane state and calls
// into this module, never the other way round, which is what keeps the
// selection resolvers and the diff-source switch testable on their own.

import {
  GetPayloadPatchSpans,
  GitListBranches,
  ListBranchCommits,
  ListPRCommits,
  ListThreadEditDiffs,
  OpenBranchBaseDiff,
  OpenCommitDiff,
  OpenEditDiff,
  OpenPRCommitDiff,
  OpenPRDiff,
  OpenTurnEditsDiff,
  OpenWorkspaceDiff,
} from './bindings';
import type { PRSnapshot } from './prReviewStore.svelte';
import { ReviewDiffSource } from './reviewDiffStream';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { BranchCommit, GitBranch, WorkspaceRef } from '../types/git';
import type { DiffReviewComment, DiffReviewScope, ReviewLineComment } from '../types/models';
import { seedPayloadPatchSpans } from '../utils/diffSpanCache.svelte';
import { mergePatchFilesByPath } from '../utils/patchFiles';
import { anchorRow, displayRowCount } from '../utils/patchRows';
import { patchFileFromReviewFile, reviewFileFromPatchFile, whenResident, type ReviewFile } from '../utils/patchStore';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { prReferenceWire, type PRRef } from '../utils/prReference';

/** Whether the diff for this selection comes from `internal/gitdiff`, the
 * only source that can apply `-w`: every scope but edits, whose patches
 * are persisted tool-call output replayed verbatim, never a git
 * recomputation.
 *
 * Shared by the toolbar's enablement and by loadPatch, so the control can
 * never offer a mode the load path won't deliver. The selected commit is
 * part of the signature because the orphan check asks per selection. */
export function supportsIgnoreWhitespace(
  scope: DiffReviewScope,
  _selectedCommitSHA: string | null,
): boolean {
  return scope !== 'edits';
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
  /** The diff to read from the backend; null when there is nothing to
   * show (an Edits view with no edits). A source that is not read must be
   * abandoned. */
  patch: ReviewDiffSource | null;
  /** Edits: sections that repeat a path show as one file per path
   * (EditFileMerge). */
  mergesPaths?: boolean;
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
  /** The computer that serves the subject's diffs. */
  backend: BackendKey;
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
  const { workspace, threadId, backend } = subject;
  const diff = (open: ConstructorParameters<typeof ReviewDiffSource>[1]) => new ReviewDiffSource(backend, open);
  switch (scope) {
    case 'pr': {
      const detail = prSnapshot?.detail;
      if (!prRef || !detail) throw new Error('No PR or MR is available for this thread.');
      const pr = prReferenceWire(prRef);
      // The backend diffs the PR head against the detail's base ref in the
      // local clone.
      const baseRef = detail.baseRefName ?? '';
      const headSHA = String(prSnapshot.headSHA || detail.headSHA || '');
      // Per-commit PR review needs the local clone; without one the backend
      // returns an empty list and the selector stays hidden. The known head
      // SHA lets the backend skip its fetch when the objects are local.
      const commits = existing
        ? existing.commits
        : baseRef
          ? (((await ListPRCommits(workspace, pr, baseRef, headSHA)) ??
              []) as BranchCommit[])
          : [];
      const commitSHA = resolveSelectedCommit(selectedCommitSHA, commits);
      const patch = commitSHA
        ? diff(() => OpenPRCommitDiff(workspace, pr, commitSHA, ignoreWhitespace))
        : diff(() => OpenPRDiff(workspace, pr, baseRef, ignoreWhitespace));
      return { patch, commits, selectedCommitSHA: commitSHA, prHeadSHA: headSHA };
    }
    case 'workspace':
      return { patch: diff(() => OpenWorkspaceDiff(workspace, ignoreWhitespace)) };
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
      const patch = selection?.kind === 'item'
        ? diff(() => OpenEditDiff(threadId, selection.payloadId))
        : selection
          ? diff(() => OpenTurnEditsDiff(threadId, selection.turnIndex))
          : null;
      return { patch, mergesPaths: true, edits: entries, editTurnLabels: turnLabels, selectedEdit: selection };
    }
    case 'branch': {
      const branch = baseBranch?.trim() || await defaultBaseBranch(workspace);
      const branchDiff = (commitSHA: string | null) => commitSHA
        ? diff(() => OpenCommitDiff(workspace, commitSHA, ignoreWhitespace))
        : diff(() => OpenBranchBaseDiff(workspace, branch, ignoreWhitespace));
      if (existing) {
        const commitSHA = resolveSelectedCommit(selectedCommitSHA, existing.commits);
        return { patch: branchDiff(commitSHA), commits: existing.commits, selectedCommitSHA: commitSHA };
      }
      if (selectedCommitSHA) {
        // Sequenced: the selection must be validated against the fresh
        // list before deciding which diff to fetch.
        const commits = ((await ListBranchCommits(workspace, branch)) ?? []) as BranchCommit[];
        const commitSHA = resolveSelectedCommit(selectedCommitSHA, commits);
        return { patch: branchDiff(commitSHA), commits, selectedCommitSHA: commitSHA };
      }
      // The diff opens while the commit list loads.
      const patch = branchDiff(null).start();
      let commits: BranchCommit[];
      try {
        commits = ((await ListBranchCommits(workspace, branch)) ?? []) as BranchCommit[];
      } catch (err) {
        patch.abandon();
        throw err;
      }
      return { patch, commits, selectedCommitSHA: null };
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

/** Whether a file starts collapsed: lockfiles and files over 400 rows. */
export function collapsedByDefault(file: ReviewFile): boolean {
  return isLockfileish(file.path) || displayRowCount(file) > 400;
}

interface EditFileGroup {
  sections: ReviewFile[];
  file: ReviewFile;
}

/**
 * The edits scope's files. A whole-turn diff repeats a path when a file was
 * edited more than once in the turn; those sections merge into one
 * file-ordered section per path (the review surface keys rows, tree and
 * collapse state by path; see mergePatchFilesByPath). Fed a parser's files
 * as they grow, a path's merge is rebuilt only when a section joins it.
 *
 * A merge reads its sections' text and holds its own copy of it. A path
 * whose section text is evicted keeps its previous merge until `settle`
 * reads the text again.
 */
export class EditFileMerge {
  private sections: readonly ReviewFile[] = [];
  private seen = 0;
  private groups = new Map<string, EditFileGroup>();
  private pending = new Set<EditFileGroup>();

  files(sections: readonly ReviewFile[]): ReviewFile[] {
    if (sections !== this.sections) {
      // A retried read fills a new parser.
      this.sections = sections;
      this.seen = 0;
      this.groups = new Map();
      this.pending = new Set();
    }
    const joined = new Set<EditFileGroup>();
    for (; this.seen < sections.length; this.seen += 1) {
      const section = sections[this.seen];
      const group = this.groups.get(section.path);
      if (group) {
        group.sections.push(section);
        joined.add(group);
      } else {
        this.groups.set(section.path, { sections: [section], file: section });
      }
    }
    for (const group of joined) {
      if (group.sections.every((section) => section.body.resident())) this.merge(group);
      else this.pending.add(group);
    }
    return Array.from(this.groups.values(), (group) => group.file);
  }

  /** Merges the paths that waited for evicted text, reading it again.
   * Rejects with PatchTextLost when it can no longer be read. */
  async settle(): Promise<void> {
    for (const group of this.pending) {
      await whenResident(group.sections.flatMap((section) => section.body.segments), () => this.merge(group));
    }
  }

  private merge(group: EditFileGroup): void {
    group.file = reviewFileFromPatchFile(mergePatchFilesByPath(group.sections.map(patchFileFromReviewFile))[0]);
    this.pending.delete(group);
  }
}

/**
 * Seeds the highlight cache with each edit payload's persisted spans,
 * primed when the file still matched at edit time, so the first paint is
 * colored without the RPC path. One payload at a time until `cancelled`;
 * a seed that does not arrive leaves its file on the RPC path.
 */
export async function seedEditPatchSpans(
  threadId: string,
  backend: BackendKey,
  payloadIds: readonly string[],
  cancelled: () => boolean,
): Promise<void> {
  for (const payloadId of payloadIds) {
    if (cancelled()) return;
    let spans;
    try {
      spans = await withBackendTarget(backend, () => GetPayloadPatchSpans(threadId, payloadId));
    } catch (err) {
      reportFrontendDiagnostic('review diff: edit span seeds failed', err instanceof Error ? err.message : String(err));
      return;
    }
    await seedPayloadPatchSpans(threadId, spans);
  }
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

export function draftAnchorExists(files: readonly ReviewFile[], comment: DiffReviewComment): boolean {
  if (comment.side === 'file') return files.some((file) => file.path === comment.filePath);
  const file = files.find((candidate) => candidate.path === comment.filePath);
  if (!file) return false;
  return anchorRow(file, comment) >= 0;
}

function isLockfileish(path: string): boolean {
  const name = path.split('/').pop() ?? path;
  return name === 'pnpm-lock.yaml' ||
    name === 'package-lock.json' ||
    name === 'go.sum' ||
    name.endsWith('.lock');
}
