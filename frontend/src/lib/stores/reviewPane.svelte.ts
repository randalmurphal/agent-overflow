import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import {
  GetBranchBaseDiff,
  GetCommitDiff,
  GetDiffContextLines,
  GetGitStatus,
  GetMergeConflictFile,
  GetPRCIJobLog,
  GetPRCIJobs,
  GetPRCommitDiff,
  GetPRDetail,
  GetPRDiff,
  GetPRMergeConflicts,
  GetPayloadData,
  GetThread,
  GetTurnEditsDiff,
  SavePRCIJobLog,
  GetWorkspaceCurrentDiff,
  GitListBranches,
  ListBranchCommits,
  ListPRCommits,
  ListThreadEditDiffs,
  ListPRReviewThreads,
  MarkDiffReviewCommentsSent,
  ReplyToPRThread,
  SendMessage,
  SendDiffReviewComments,
  SubmitPRReview,
  SetPRUpdatesActive,
  SubscribePRUpdates,
  UnsubscribePRUpdates,
} from './bindings';
import { appStorageGet, appStorageSet } from './appStorage';
import { openCompanion } from './companionPanes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import {
  createDiffReviewComment,
  deleteDiffReviewComment,
  getDiffReviewComments,
  refreshDiffReviewComments,
  setActiveDiffReviewSource,
  updateDiffReviewComment,
} from './diffReviewComments.svelte';
import { getSettings } from './settings.svelte';
import { getActiveTurn } from './threadStatuses.svelte';
import type { BranchCommit, GitBranch } from '../types/git';
import type {
  CIJob,
  CIJobLogResult,
  CIPipeline,
  DiffReviewComment,
  DiffReviewScope,
  PRDetail,
  ReviewLineComment,
  ReviewThread,
  SubmitPRReviewResult,
  Thread,
} from '../types/models';
import { diffSourceKey } from '../utils/diffSourceKey';
import { seedPayloadPatchSpans, type PatchScopeContext } from '../utils/diffSpanCache.svelte';
import { conflictPatchFile } from '../utils/conflictFile';
import { hunkExcerptForComment } from '../utils/prHunkExcerpt';
import { prRefFromThread, prRefFromUrl, prScopeLabel, type PRRef } from '../utils/prReference';
import {
  applyContextExpansion,
  expansionFetchRange,
  nextExpansionVersion,
  type ContextExpansionState,
  type ExpandDirection,
} from '../utils/diffContextExpansion';
import {
  filePatchDisplayRows,
  mergePatchFilesByPath,
  parsePatchFilesCached,
  type DiffGap,
  type PatchFile,
} from '../utils/patchFiles';
import { anchorKey, type CommentAnchor } from '../utils/reviewRows';
import { sortFilesTreeOrder } from '../utils/reviewTree';
import type { CommentListItem } from '../utils/reviewComments';

export type ReviewScope = DiffReviewScope;

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

export interface ReviewPaneState {
  /** Thread this state was created for — the registry's staleness check. */
  readonly threadId: string;
  readonly scope: ReviewScope;
  readonly baseBranch: string | null;
  readonly prRef: PRRef | null;
  readonly prScopeLabel: string | null;
  readonly patchText: string;
  readonly sourceKey: string;
  readonly files: PatchFile[];
  readonly comments: readonly DiffReviewComment[];
  readonly drafts: readonly DiffReviewComment[];
  readonly openEditors: readonly CommentAnchor[];
  /** Commits the branch/PR carries (newest first); empty outside those
   * scopes and for non-git workspaces. Feeds the commit selector. */
  readonly commits: readonly BranchCommit[];
  /** Selected single commit, or null for the full-range diff. */
  readonly selectedCommitSHA: string | null;
  /** Edit tool calls of the thread (timeline order); populated in
   * edits scope only. Feeds the turn-grouped edit selector. */
  readonly edits: readonly EditDiffEntryView[];
  /** Turn index → first user prompt summary, for selector group labels. */
  readonly editTurnLabels: ReadonlyMap<number, string>;
  /** Current edits-scope selection (never null once loaded with edits). */
  readonly selectedEditKey: string | null;
  /** Edit tool call to select on the next edits-scope load — set by
   * the inline-diff affordance before setScope('edits'). */
  pendingEditItemID: string | null;
  pendingJumpFilePath: string | null;
  /** Diff row key to jump to (comments-list click); consumed by the diff body. */
  readonly pendingJumpRowKey: string | null;
  readonly loading: boolean;
  readonly error: string | null;
  readonly sendingComments: boolean;
  readonly prDetail: PRDetail | null;
  readonly prThreads: readonly ReviewThread[];
  readonly prHeadSHA: string;
  /** Scope fields for parse-priming span requests — the same triple
   * the diff-context expansion sends (`app_diff_context.go` scopes). */
  readonly spanContext: PatchScopeContext;
  readonly prStale: boolean;
  readonly refreshingPRData: boolean;
  readonly conflictView: boolean;
  readonly conflicts: PRMergeConflictsView | null;
  readonly conflictsLoading: boolean;
  readonly conflictsError: string | null;
  readonly conflictContentByPath: SvelteMap<string, string>;
  readonly conflictCollapsedPaths: SvelteSet<string>;
  readonly conflictFiles: PatchFile[];
  readonly ciPipeline: CIPipeline | null;
  readonly ciLoading: boolean;
  readonly ciError: string | null;
  readonly ciLogView: CILogView | null;
  readonly ciLog: CIJobLogResult | null;
  readonly ciLogLoading: boolean;
  readonly ciLogError: string | null;
  readonly ciLogSavedPath: string | null;
  readonly submitTarget: 'agent' | 'pr';
  /** submitTarget with single-commit view forced to 'agent': drafts on a
   * commit diff carry that diff's line numbers, which the forge would
   * misanchor against the PR head diff. */
  readonly effectiveSubmitTarget: 'agent' | 'pr';
  readonly verdict: 'comment' | 'approve' | 'request-changes';
  readonly summaryBody: string;
  readonly submitError: string | null;
  readonly isTurnActive: boolean;
  readonly collapsedPaths: SvelteSet<string>;
  /** Every file on the active surface (conflict view or diff) is collapsed. */
  readonly allCollapsed: boolean;
  readonly expandedPRThreadIds: SvelteSet<string>;
  readonly viewMode: 'stacked' | 'split';
  readonly wordWrap: boolean;
  setScope(scope: ReviewScope, opts?: { baseBranch?: string }): Promise<void>;
  /** Switch the loaded diff to a single commit (null → full range). */
  selectCommit(sha: string | null): Promise<void>;
  /** Switch the edits-scope diff (selector value encoding: `item:<id>`
   * or `turn:<n>`; null → default, the latest turn). */
  selectEdit(key: string | null): Promise<void>;
  reload(): Promise<void>;
  consumePendingJumpFilePath(): void;
  /** Jump the diff body to a comment row: leaves conflict/CI-log views,
   * expands the file (and the thread, for collapsed PR threads). */
  jumpToComment(item: CommentListItem): void;
  consumePendingJumpRowKey(): void;
  openDraftEditor(anchor: CommentAnchor): void;
  closeDraftEditor(anchor: CommentAnchor): void;
  /** Store-backed editor text — survives virtualizer row unmounts. */
  draftBodyFor(anchor: CommentAnchor): string;
  setDraftBody(anchor: CommentAnchor, body: string): void;
  /** True exactly once after openDraftEditor, for the mount autofocus. */
  consumeDraftEditorFocus(anchor: CommentAnchor): boolean;
  createComment(anchor: CommentAnchor, body: string): Promise<void>;
  updateComment(commentId: string, body: string): Promise<void>;
  deleteComment(commentId: string): Promise<void>;
  sendComments(): Promise<void>;
  submitPRReview(): Promise<void>;
  setSubmitTarget(target: 'agent' | 'pr'): void;
  setVerdict(verdict: 'comment' | 'approve' | 'request-changes'): void;
  setSummaryBody(body: string): void;
  orphanedDraftIds(): SvelteSet<string>;
  togglePRThread(threadId: string): void;
  replyBodyFor(threadId: string): string;
  setReplyBody(threadId: string, body: string): void;
  sendPRThreadReply(thread: ReviewThread): Promise<void>;
  sendPRThreadToAgent(thread: ReviewThread): Promise<void>;
  replyErrorFor(threadId: string): string | null;
  sendingReply(threadId: string): boolean;
  /** PR scope: re-fetches detail + review threads WITHOUT reloading the
   * diff. A moved head raises the stale banner like the poll pump. */
  refreshPRThreads(): Promise<void>;
  openConflictView(): Promise<void>;
  closeConflictView(): void;
  toggleConflictCollapsed(path: string): Promise<void>;
  expandConflictFold(path: string, foldId: number): void;
  loadCIJobs(): Promise<void>;
  openCIJobLog(stageName: string, job: CIJob): Promise<void>;
  closeCILogView(): void;
  refreshCILog(): Promise<void>;
  saveCILog(): Promise<string | null>;
  sendCILogToChat(): Promise<void>;
  /** Fetches hidden hunk-gap context and merges it into the diff. */
  expandDiffContext(path: string, gap: DiffGap, dir: ExpandDirection): Promise<void>;
  toggleCollapsed(path: string): void;
  toggleCollapseAll(): Promise<void>;
  setViewMode(mode: 'stacked' | 'split'): void;
  setWordWrap(wrap: boolean): void;
  dispose(): void;
}

interface PersistedReviewScope {
  scope: ReviewScope;
  baseBranch?: string | null;
}

export interface CILogView {
  stageName: string;
  job: CIJob;
}

interface PRMergeConflictsView {
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
}

const statesBySourcePane = new Map<string, ReviewPaneState>();
const prStatesBySubscription = new Map<string, { applyPRUpdate(event: PRUpdatedEvent): void }>();

export interface PRUpdatedEvent {
  subscriptionId: string;
  threadId: string;
  pr: PRRef;
  detail: PRDetail;
  threads: ReviewThread[];
  headSHA: string;
}

function registerPRReviewState(subscriptionId: string, state: { applyPRUpdate(event: PRUpdatedEvent): void }): void {
  prStatesBySubscription.set(subscriptionId, state);
  // A load can finish while the window sits minimized; the fresh pump
  // must start paused like every other live one.
  if (documentHidden()) setPRUpdatesActive(subscriptionId, false);
}

function unregisterPRReviewState(subscriptionId: string): void {
  prStatesBySubscription.delete(subscriptionId);
}

export function applyPRUpdatedEvent(event: PRUpdatedEvent): void {
  prStatesBySubscription.get(event.subscriptionId)?.applyPRUpdate(event);
}

function documentHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden';
}

function setPRUpdatesActive(subscriptionId: string, active: boolean): void {
  void SetPRUpdatesActive(subscriptionId, active).catch((err: unknown) => {
    // Not user-surfaced: a failed pause keeps the status quo (the pump
    // just keeps polling), and a failed resume implies a dying transport
    // whose server-side connection cleanup closes the pump anyway.
    console.error('review: SetPRUpdatesActive failed', { subscriptionId, active, err });
  });
}

// A hidden window doesn't need PR polling: pause every live subscription's
// Go-side pump while the document is hidden and resume (with a catch-up
// poll when a tick was missed) once it becomes visible again.
export function handleReviewVisibilityChange(): void {
  const active = !documentHidden();
  for (const id of prStatesBySubscription.keys()) {
    setPRUpdatesActive(id, active);
  }
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', handleReviewVisibilityChange);
}

export function reviewStateForPane(sourcePaneId: string, threadId: string, thread?: Thread | null): ReviewPaneState {
  const existing = statesBySourcePane.get(sourcePaneId);
  // Thread mismatch replaces rather than reuses: the CompanionPane {#key}
  // remount usually disposes the old state first, but correctness must not
  // depend on Svelte's destroy-before-create ordering.
  if (existing && existing.threadId === threadId) return existing;
  // The replaced state may own a live PR-update subscription; drop it or
  // the Go-side poll pump outlives the state that could unsubscribe it.
  existing?.dispose();
  const state = createReviewPaneState(sourcePaneId, threadId, thread ?? null);
  statesBySourcePane.set(sourcePaneId, state);
  return state;
}

export function disposeReviewStateForPane(sourcePaneId: string): void {
  statesBySourcePane.get(sourcePaneId)?.dispose?.();
  statesBySourcePane.delete(sourcePaneId);
}

export async function openReviewCompanion(
  sourcePaneId: string,
  threadId: string,
  opts: {
    scope?: ReviewScope;
    filePath?: string;
    /** Edits scope: the edit tool call to pin on load. */
    editItemId?: string;
  } = {},
): Promise<ReviewPaneState | null> {
  const companion = openCompanion(sourcePaneId, 'review');
  if (!companion) return null;
  const state = reviewStateForPane(sourcePaneId, threadId);
  if (opts.filePath) {
    state.pendingJumpFilePath = opts.filePath;
  }
  if (opts.editItemId) {
    state.pendingEditItemID = opts.editItemId;
  }
  if (opts.scope) {
    await state.setScope(opts.scope);
  } else if (opts.editItemId) {
    // A pinned edit implies edits scope; setScope reloads even when the
    // pane already sits there, which is what consumes the pending pin.
    await state.setScope('edits');
  }
  return state;
}

function createReviewPaneState(sourcePaneId: string, threadId: string, initialThread: Thread | null): ReviewPaneState {
  const persisted = readPersistedScope(threadId);
  const initialPRRef = prRefFromThread(initialThread ?? {});
  let prRef: PRRef | null = $state(initialPRRef);
  let scope: ReviewScope = $state(
    initialPRRef && (initialThread?.workspacePath ?? '') === '' ? 'pr' : (persisted?.scope ?? 'workspace'),
  );
  let baseBranch: string | null = $state(persisted?.baseBranch ?? null);
  let prDetail: PRDetail | null = $state(null);
  let prThreads: ReviewThread[] = $state([]);
  let prHeadSHA = $state('');
  let prStale = $state(false);
  let refreshingPRData = $state(false);
  let subscriptionId: string | null = null;
  let conflictView = $state(false);
  let conflicts: PRMergeConflictsView | null = $state(null);
  let conflictsLoading = $state(false);
  let conflictsError: string | null = $state(null);
  const conflictContentByPath = new SvelteMap<string, string>();
  let conflictCollapsedPaths: SvelteSet<string> = $state(new SvelteSet<string>());
  const conflictFileLoads = new Map<string, Promise<void>>();
  // Expanded fold ids per path. Entries are replaced wholesale on expand
  // so the SvelteMap write re-derives conflictFiles.
  const conflictExpandedFolds = new SvelteMap<string, ReadonlySet<number>>();
  let ciPipeline: CIPipeline | null = $state(null);
  let ciLoading = $state(false);
  let ciError: string | null = $state(null);
  let ciLoadSeq = 0;
  let ciLogView: CILogView | null = $state(null);
  let ciLog: CIJobLogResult | null = $state(null);
  let ciLogLoading = $state(false);
  let ciLogError: string | null = $state(null);
  let ciLogSavedPath: string | null = $state(null);
  let ciLogSeq = 0;
  let submitTarget: 'agent' | 'pr' = $state('agent');
  let verdict: 'comment' | 'approve' | 'request-changes' = $state('comment');
  let summaryBody = $state('');
  let submitError: string | null = $state(null);
  const replyBodies = new SvelteMap<string, string>();
  const replyErrors = new SvelteMap<string, string>();
  const sendingReplyIds: SvelteSet<string> = $state(new SvelteSet<string>());
  let expandedPRThreadIds: SvelteSet<string> = $state(new SvelteSet<string>());
  let commits: BranchCommit[] = $state([]);
  let selectedCommitSHA: string | null = $state(null);
  let edits: EditDiffEntryView[] = $state([]);
  let editTurnLabels: ReadonlyMap<number, string> = $state(new Map());
  let selectedEdit: EditSelection | null = $state(null);
  let pendingEditItemID: string | null = null;
  const effectiveSubmitTarget = $derived<'agent' | 'pr'>(
    scope === 'pr' && !selectedCommitSHA ? submitTarget : 'agent',
  );
  let pendingJumpFilePath: string | null = $state(null);
  let pendingJumpRowKey: string | null = $state(null);
  let patchText = $state('');
  let loading = $state(false);
  let error: string | null = $state(null);
  // Hunk-gap expansions, per file path. The map itself is plain state:
  // merges mutate entries in place and bump the version counter, which
  // is the sole reactive signal the `files` derived reads.
  const contextExpansions = new Map<string, ContextExpansionState>();
  let contextExpansionVersion = $state(0);
  let sendingComments = $state(false);
  let openEditors: CommentAnchor[] = $state([]);
  // Draft-editor text lives HERE, not in the row component: editor rows
  // are virtualized, so scrolling one out of the render window unmounts
  // it — row-local state would silently drop the user's typed text.
  const draftBodies = new SvelteMap<string, string>();
  // One-shot focus request, set on user-initiated open and consumed by
  // the editor's mount effect. Without it, an editor row remounting as
  // it re-enters the render buffer would steal focus mid-scroll.
  let pendingEditorFocusKey: string | null = null;
  let collapsedPaths: SvelteSet<string> = $state(new SvelteSet<string>());
  // Explicit user collapse/expand choices, by path. Reloads re-derive
  // the DEFAULT collapse set from the fresh patch but must not undo
  // what the user deliberately opened or closed mid-read; overrides
  // reset when the scope (and therefore the diff's subject) changes.
  const collapseOverrides = new Map<string, boolean>();
  let viewMode: 'stacked' | 'split' = $state('stacked');
  let wordWrap = $state(getSettings().diffWordWrap);
  // Edits-scope files whose expansion attempt was refused (workspace
  // drifted from the historical diff): their gap rows retire for this
  // load. Cleared with the expansions on every reload.
  const unexpandableEditPaths = new SvelteSet<string>();
  let loadSeq = 0;
  const sourceKey = $derived.by(() => {
    if (scope === 'edits') {
      // An edit payload is immutable, so its id is the stable identity;
      // a whole-turn view keys by the turn (its edit set only grows
      // while the turn is still streaming).
      if (!selectedEdit) return '';
      return selectedEdit.kind === 'item'
        ? `edit:${selectedEdit.payloadId}`
        : `edit-turn:${selectedEdit.turnIndex}`;
    }
    if (selectedCommitSHA) {
      // A single commit's content is immutable — the SHA itself is the
      // stable identity, in both branch and pr scope.
      return `commit:${selectedCommitSHA}`;
    }
    if (scope === 'pr' && prRef) {
      // Stable across PR head movement: drafts must survive pushes; each
      // draft's commitSha records the head SHA it was anchored to.
      return `pr:${prRef.forge}:${prRef.namespace}/${prRef.repo}:${prRef.number}`;
    }
    return patchText ? diffSourceKey(patchText) : '';
  });
  // Tree display order (dirs first, alphabetical), so the diff body,
  // rail tree, j/k nav, and comment grouping all read top-to-bottom in
  // the same sequence. Git's raw patch order is plain lexicographic,
  // which interleaves root files between directories. Hunk-gap
  // expansions overlay per file, keyed by the version counter.
  const files = $derived.by(() => {
    void contextExpansionVersion;
    let parsed = sortFilesTreeOrder(parsePatchFilesCached(patchText));
    if (scope === 'edits') {
      // A whole-turn concatenation repeats a path when a file was edited
      // more than once in the turn — merge those sections into one file
      // (the review surface keys rows/tree/collapse by path); merged
      // multi-section files get suppressGaps from the helper (their
      // sections' line numbers describe different moments, so gap
      // coordinates are incoherent). Single-section files keep their
      // gap rows — expansion serves the CURRENT workspace file only
      // after the backend verifies it still matches this historical
      // patch, and a refusal retires the file's gaps here. Copies, not
      // mutation: the parse cache is shared across panes and scopes.
      parsed = mergePatchFilesByPath(parsed).map((file) =>
        unexpandableEditPaths.has(file.path) ? { ...file, suppressGaps: true } : file,
      );
    }
    if (contextExpansions.size === 0) return parsed;
    return parsed.map((file) => applyContextExpansion(file, contextExpansions.get(file.path)));
  });
  const conflictFiles = $derived.by(() => {
    if (!conflicts) return [];
    const { baseLabel, headLabel, notes } = conflicts;
    return conflicts.paths.map((path) => {
      const content = conflictContentByPath.get(path);
      const pathNotes = notes[path];
      // A structural conflict's content can be unfetchable (the path may
      // not exist in the merged tree at all) — its notes still render.
      if (content !== undefined || pathNotes?.length) {
        return conflictPatchFile(path, content ?? '', {
          baseLabel,
          headLabel,
          notes: pathNotes,
          expandedFolds: conflictExpandedFolds.get(path),
        });
      }
      return { path, kind: 'conflict', additions: 0, deletions: 0, lines: [] };
    });
  });
  // Whether every file on the ACTIVE surface (conflict view or diff) is
  // collapsed — drives the toolbar's expand-all/collapse-all toggle.
  const allCollapsed = $derived.by(() => {
    if (conflictView) {
      const paths = conflicts?.paths ?? [];
      return paths.length > 0 && paths.every((path) => conflictCollapsedPaths.has(path));
    }
    return files.length > 0 && files.every((file) => collapsedPaths.has(file.path));
  });
  const comments = $derived(getDiffReviewComments(threadId, scope, sourceKey));
  const drafts = $derived(comments.filter((comment) => comment.status === 'draft'));
  const isTurnActive = $derived(getActiveTurn(threadId) !== null);

  async function ensurePRRef(): Promise<PRRef | null> {
    if (prRef) return prRef;
    try {
      const thread = (await GetThread(threadId)) as Thread;
      prRef = prRefFromThread(thread);
      if (prRef) return prRef;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
    try {
      const status = await GetGitStatus(threadId);
      const ref = prRefFromUrl(
        String(status?.forge ?? ''),
        String(status?.openPrUrl ?? ''),
        Number(status?.openPrNumber ?? 0),
      );
      prRef = ref;
      return ref;
    } catch {
      return null;
    }
  }

  // The scope dropdown's PR option renders only once prRef resolves, and
  // ensurePRRef otherwise runs only on ENTRY into pr scope — without
  // probing at mount (and on reload, for a PR opened while the pane sat
  // open), a thread whose BRANCH has an open PR (the git-status detection
  // path) could never surface the option at all.
  function probePRRef(): void {
    if (prRef) return;
    void ensurePRRef().catch(() => {
      // Not swallowed: ensurePRRef records a thread-lookup failure in
      // `error` before throwing, and no-PR resolves to null, not a throw.
    });
  }

  // Set by dispose(); a reload that resolves after disposal must drop the
  // subscription it just created instead of registering it on a dead state.
  let disposed = false;

  async function unsubscribePR(): Promise<void> {
    const id = subscriptionId;
    if (!id) return;
    subscriptionId = null;
    unregisterPRReviewState(id);
    try {
      await UnsubscribePRUpdates(id);
    } catch (err) {
      error = userFacingError(err);
    }
  }

  function dispose(): void {
    disposed = true;
    resetConflictState();
    resetCIState();
    const id = subscriptionId;
    if (!id) return;
    subscriptionId = null;
    unregisterPRReviewState(id);
    void UnsubscribePRUpdates(id).catch((err: unknown) => {
      error = userFacingError(err);
    });
  }

  function applyPRUpdate(event: PRUpdatedEvent): void {
    if (event.subscriptionId !== subscriptionId) return;
    if (event.headSHA && prHeadSHA && event.headSHA !== prHeadSHA) {
      prDetail = event.detail;
      prThreads = event.threads ?? [];
      prHeadSHA = event.headSHA;
      prStale = true;
      void loadCIJobs();
      return;
    }
    prDetail = event.detail;
    prThreads = event.threads ?? [];
    prHeadSHA = event.headSHA || event.detail?.headSHA || prHeadSHA;
    // The pump only fires on snapshot change, so this refresh tracks
    // check/pipeline movement without its own poll.
    void loadCIJobs();
  }

  function resetConflictState(): void {
    conflictView = false;
    conflicts = null;
    conflictsLoading = false;
    conflictsError = null;
    conflictContentByPath.clear();
    conflictCollapsedPaths = new SvelteSet<string>();
    conflictFileLoads.clear();
    conflictExpandedFolds.clear();
  }

  async function setScope(nextScope: ReviewScope, opts?: { baseBranch?: string }): Promise<void> {
    const scopeChanged = nextScope !== scope;
    if (scopeChanged) {
      resetConflictState();
      resetCIState();
    }
    if (scope === 'pr' && nextScope !== 'pr') {
      await unsubscribePR();
    }
    if (nextScope === 'pr') {
      const ref = await ensurePRRef();
      if (!ref) {
        error = 'No PR or MR is available for this thread.';
        return;
      }
    }
    scope = nextScope;
    baseBranch = nextScope === 'branch'
      ? (opts?.baseBranch?.trim() || baseBranch || await defaultBaseBranch(threadId))
      : null;
    // Back to "latest" on scope entry — the previous selection belongs
    // to another commit range. A base-branch change within branch scope
    // keeps it; reload's validation drops a selection that left the new
    // range.
    if (scopeChanged) {
      selectedCommitSHA = null;
      selectedEdit = null;
    }
    openEditors = [];
    draftBodies.clear();
    collapseOverrides.clear();
    persistScope(threadId, scope, baseBranch);
    await reload();
  }

  async function selectCommit(sha: string | null): Promise<void> {
    if (sha === selectedCommitSHA) return;
    selectedCommitSHA = sha;
    openEditors = [];
    draftBodies.clear();
    collapseOverrides.clear();
    await reload({ selectionOnly: true });
  }

  async function selectEdit(key: string | null): Promise<void> {
    const next = editSelectionFromKey(key, edits);
    if (editSelectionKey(next) === editSelectionKey(selectedEdit)) return;
    selectedEdit = next;
    openEditors = [];
    draftBodies.clear();
    collapseOverrides.clear();
    await reload({ selectionOnly: true });
  }

  async function reload(opts?: { selectionOnly?: boolean }): Promise<void> {
    // A commit/edit selection changes only which diff is shown — the
    // selector list, PR detail, and subscription are all still valid,
    // so reuse them and fetch just the diff. Only when a previous full
    // load actually populated them; otherwise fall through to a full
    // load.
    const selectionOnly = opts?.selectionOnly === true
      && (scope === 'edits'
        ? edits.length > 0
        : commits.length > 0 && (scope !== 'pr' || subscriptionId !== null));
    // The inline-diff affordance pins an edit for the NEXT load;
    // consumed here so a later manual reload doesn't re-pin it.
    const pinnedEditItemID = pendingEditItemID;
    pendingEditItemID = null;
    const seq = loadSeq + 1;
    loadSeq = seq;
    loading = true;
    error = null;
    if (!selectionOnly) {
      // Unconditionally, not just in pr scope: scope can change mid-load
      // (the selector stays enabled while a PR loads), and an in-flight
      // pr load that resolved during setScope's awaits may have
      // registered a subscription after the scope flipped. No-op when
      // none is held.
      await unsubscribePR();
    }
    try {
      if (!selectionOnly) {
        if (scope === 'pr' && !prRef) {
          // Persisted 'pr' scope restores before the thread/git status is at
          // hand; resolve the reference here instead of failing the load.
          await ensurePRRef();
        } else {
          // Fire-and-forget: a PR opened after this pane mounted becomes
          // selectable on the next reload without blocking the diff load.
          probePRRef();
        }
      }
      const loaded = await loadPatch(
        threadId,
        scope,
        baseBranch,
        selectedCommitSHA,
        prRef,
        { pinnedItemId: pinnedEditItemID, current: selectedEdit },
        selectionOnly ? { commits, prDetail, edits, editTurnLabels } : undefined,
      );
      if (seq !== loadSeq || disposed) {
        // A newer load or dispose superseded this one — the subscription it
        // opened has no owner, so close it before dropping the result.
        if (loaded.subscriptionId) void UnsubscribePRUpdates(loaded.subscriptionId);
        return;
      }
      commits = loaded.commits ?? [];
      edits = loaded.edits ?? [];
      editTurnLabels = loaded.editTurnLabels ?? new Map();
      if (loaded.selectedEdit !== undefined) {
        selectedEdit = loaded.selectedEdit;
      }
      if (loaded.selectedCommitSHA !== undefined) {
        selectedCommitSHA = loaded.selectedCommitSHA;
      }
      if (loaded.prDetail) prDetail = loaded.prDetail;
      if (loaded.prThreads) prThreads = loaded.prThreads;
      if (loaded.prHeadSHA !== undefined) prHeadSHA = loaded.prHeadSHA;
      if (loaded.subscriptionId) {
        subscriptionId = loaded.subscriptionId;
        registerPRReviewState(subscriptionId, { applyPRUpdate });
      }
      clearContextExpansions();
      patchText = loaded.patchText;
      // Fresh defaults for the new patch, with the user's explicit
      // collapse/expand choices layered back on top.
      const nextCollapsed = defaultCollapsedPaths(parsePatchFilesCached(loaded.patchText));
      for (const [path, collapsed] of collapseOverrides) {
        if (collapsed) nextCollapsed.add(path);
        else nextCollapsed.delete(path);
      }
      collapsedPaths = nextCollapsed;
      if (scope === 'pr') {
        prStale = false;
        // The fast path didn't refresh the PR snapshot, so CI state
        // hasn't moved either — the subscription pump covers it.
        if (!selectionOnly) void loadCIJobs();
      }
      // patchText and selectedCommitSHA are already updated above, so
      // the derived reflects this load — no need to re-derive by hand.
      const nextSourceKey = sourceKey;
      if (!nextSourceKey) {
        openEditors = [];
        draftBodies.clear();
        setActiveDiffReviewSource(threadId, null);
        error = null;
        return;
      }
      try {
        await refreshDiffReviewComments(threadId, scope, nextSourceKey);
        if (seq !== loadSeq) return;
        setActiveDiffReviewSource(threadId, scope, nextSourceKey);
        error = null;
      } catch (err) {
        if (seq !== loadSeq) return;
        error = userFacingError(err);
      }
    } catch (err) {
      if (seq !== loadSeq) return;
      clearContextExpansions();
      patchText = '';
      openEditors = [];
      draftBodies.clear();
      collapsedPaths = new SvelteSet<string>();
      setActiveDiffReviewSource(threadId, null);
      error = userFacingError(err);
    } finally {
      if (seq === loadSeq) loading = false;
    }
  }

  void reload();

  function clearContextExpansions(): void {
    unexpandableEditPaths.clear();
    if (contextExpansions.size === 0) return;
    contextExpansions.clear();
    contextExpansionVersion += 1;
  }

  // The scope triple GetDiffContextLines / HighlightPatchWithContext
  // use to resolve new-side file content (`app_diff_context.go`). A
  // selected commit in branch scope reads through 'commit' scope; pr
  // scope reads the local PR clone, at the selected commit when one is
  // set (falling back to the head).
  function patchScopeContext(): PatchScopeContext {
    if (scope === 'pr') {
      return { scope: 'pr', commitSHA: selectedCommitSHA ?? '', headSHA: prHeadSHA };
    }
    if (selectedCommitSHA) {
      return { scope: 'commit', commitSHA: selectedCommitSHA, headSHA: '' };
    }
    // 'edits' resolves the workspace file too, but only after the
    // backend verifies it still matches the historical patch (the
    // request carries the patch as VerifyPatch); a drifted file
    // degrades to unprimed spans, never to wrong colors.
    return { scope, commitSHA: '', headSHA: '' };
  }

  // The historical patch text of one edits-scope file, for the
  // backend's has-the-file-drifted verification. Empty for unknown
  // paths (the backend then refuses, which is the safe direction).
  function editVerifyPatch(path: string): string {
    if (scope !== 'edits') return '';
    const file = files.find((candidate) => candidate.path === path);
    if (!file) return '';
    return file.lines.map((line) => line.content).join('\n');
  }

  async function expandDiffContext(path: string, gap: DiffGap, dir: ExpandDirection): Promise<void> {
    const range = expansionFetchRange(gap, dir);
    if (!range) return;
    const seq = loadSeq;
    try {
      const context = patchScopeContext();
      const result = await GetDiffContextLines(threadId, {
        scope: context.scope,
        commitSHA: context.commitSHA,
        headSHA: context.headSHA,
        path,
        startLine: range.start,
        endLine: range.end,
        verifyPatch: editVerifyPatch(path),
      });
      // The diff reloaded underneath the fetch — its line numbering may
      // no longer be the one this slice was addressed against.
      if (seq !== loadSeq || disposed) return;
      const state = contextExpansions.get(path)
        ?? { lines: new Map<number, string>(), eofLine: null, version: 0 };
      const lines = result.lines ?? [];
      for (let index = 0; index < lines.length; index += 1) {
        state.lines.set(result.startLine + index, lines[index]);
      }
      if (result.eof) state.eofLine = result.totalLines;
      state.version = nextExpansionVersion();
      contextExpansionVersion += 1;
      contextExpansions.set(path, state);
      error = null;
    } catch (err) {
      if (seq !== loadSeq || disposed) return;
      if (scope === 'edits') {
        // The workspace file has drifted from this historical edit (or
        // is gone) — expansion can't be offered truthfully. Retire the
        // file's gap affordances instead of raising an error banner:
        // the diff itself is still fully valid.
        unexpandableEditPaths.add(path);
        return;
      }
      error = userFacingError(err);
    }
  }

  function openDraftEditor(anchor: CommentAnchor): void {
    const key = anchorKey(anchor);
    pendingEditorFocusKey = key;
    if (openEditors.some((editor) => anchorKey(editor) === key)) return;
    openEditors = [...openEditors, anchor];
  }

  function closeDraftEditor(anchor: CommentAnchor): void {
    const key = anchorKey(anchor);
    openEditors = openEditors.filter((editor) => anchorKey(editor) !== key);
    draftBodies.delete(key);
    if (pendingEditorFocusKey === key) pendingEditorFocusKey = null;
  }

  async function createComment(anchor: CommentAnchor, body: string): Promise<void> {
    const trimmed = body.trim();
    if (!sourceKey || !trimmed) return;
    try {
      await createDiffReviewComment(threadId, {
        scope,
        sourceKey,
        commitSha: selectedCommitSHA ?? (scope === 'pr' ? prHeadSHA : undefined),
        filePath: anchor.filePath,
        oldLine: anchor.oldLine,
        newLine: anchor.newLine,
        side: anchor.side,
        selectedText: anchor.selectedText ?? '',
        body: trimmed,
      });
      closeDraftEditor(anchor);
      setActiveDiffReviewSource(threadId, scope, sourceKey);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function updateComment(commentId: string, body: string): Promise<void> {
    const trimmed = body.trim();
    if (!sourceKey || !trimmed) return;
    try {
      await updateDiffReviewComment(threadId, scope, sourceKey, commentId, { body: trimmed });
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function deleteComment(commentId: string): Promise<void> {
    if (!sourceKey) return;
    try {
      await deleteDiffReviewComment(threadId, scope, sourceKey, commentId);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function sendComments(): Promise<void> {
    if (!sourceKey || drafts.length === 0 || sendingComments || getActiveTurn(threadId) !== null) return;
    sendingComments = true;
    try {
      await SendDiffReviewComments(threadId, scope, sourceKey, drafts.map((comment) => comment.id), {
        pr: scope === 'pr' && prDetail
          ? {
              number: prDetail.number,
              url: prDetail.url,
              comments: drafts.map((comment) => ({
                commentId: comment.id,
                hunkExcerpt: hunkExcerptForComment(files, comment),
              })),
            }
          : undefined,
      });
      await refreshDiffReviewComments(threadId, scope, sourceKey);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    } finally {
      sendingComments = false;
    }
  }

  async function submitPRReview(): Promise<void> {
    // A single-commit view's drafts carry line numbers from that commit's
    // diff, which SubmitPRReview would anchor against the PR head diff —
    // wrong lines or hard failures. The UI hides the 'pr' target there;
    // this guard backs it up.
    if (scope !== 'pr' || selectedCommitSHA || !prRef || !sourceKey || sendingComments) return;
    const orphaned = orphanedDraftIds();
    const submitDrafts = drafts.filter((comment) => !orphaned.has(comment.id));
    // A bare Approve is a valid review; comment and request-changes need
    // content (GitHub's API rejects those events without a body).
    if (submitDrafts.length === 0 && !summaryBody.trim() && verdict !== 'approve') {
      submitError = 'No non-orphaned PR comments to submit.';
      return;
    }
    sendingComments = true;
    submitError = null;
    try {
      const result = (await SubmitPRReview(prReference(prRef), {
        verdict,
        body: summaryBody.trim(),
        comments: submitDrafts.map(reviewLineCommentForDraft).filter((comment): comment is ReviewLineComment => comment !== null),
      })) as SubmitPRReviewResult;
      let sent = submitDrafts;
      if (result.partialFailurePath) {
        // The review (with every line comment) posted; file-level comments
        // post one-by-one after it and stop at the first failure, so only
        // the first postedFileComments of them made it up.
        const fileLevel = submitDrafts.filter((comment) => comment.side === 'file');
        const unposted = new Set(fileLevel.slice(result.postedFileComments).map((comment) => comment.id));
        sent = submitDrafts.filter((comment) => !unposted.has(comment.id));
        submitError = `Posting file-level comment for ${result.partialFailurePath} failed: ${result.partialFailure ?? 'unknown error'}`;
      } else if (result.partialFailure) {
        // Everything posted; a follow-up step (GitLab approve) failed.
        submitError = `Review posted, but a follow-up step failed: ${result.partialFailure}`;
      }
      if (sent.length > 0) {
        await MarkDiffReviewCommentsSent(threadId, scope, sourceKey, sent.map((comment) => comment.id), `pr:${prHeadSHA}`);
      }
      await refreshDiffReviewComments(threadId, scope, sourceKey);
      prThreads = ((await ListPRReviewThreads(prReference(prRef))) ?? []) as ReviewThread[];
      if (!result.partialFailure) {
        summaryBody = '';
        submitError = null;
      }
      error = null;
    } catch (err) {
      submitError = userFacingError(err);
      error = submitError;
      throw err;
    } finally {
      sendingComments = false;
    }
  }

  // Comments-only refresh: PR detail + review threads, WITHOUT touching
  // the diff (they are fetched by separate calls, so this never reloads
  // or re-renders the patch). A moved head raises the stale banner the
  // same way the poll pump does — the diff never swaps mid-read.
  async function refreshPRThreads(): Promise<void> {
    if (scope !== 'pr' || !prRef || refreshingPRData) return;
    refreshingPRData = true;
    try {
      const pr = prReference(prRef);
      const [detail, threads] = await Promise.all([
        GetPRDetail(pr) as Promise<PRDetail>,
        ListPRReviewThreads(pr) as Promise<ReviewThread[] | null>,
      ]);
      if (disposed || scope !== 'pr') return;
      const headSHA = String(detail?.headSHA ?? '');
      if (headSHA && prHeadSHA && headSHA !== prHeadSHA) prStale = true;
      prDetail = detail;
      prThreads = threads ?? [];
      if (headSHA) prHeadSHA = headSHA;
      error = null;
    } catch (err) {
      if (disposed) return;
      error = userFacingError(err);
    } finally {
      refreshingPRData = false;
    }
  }

  async function sendPRThreadReply(thread: ReviewThread): Promise<void> {
    if (!prRef) return;
    const body = (replyBodies.get(thread.id) ?? '').trim();
    if (!body || sendingReplyIds.has(thread.id)) return;
    const first = thread.comments[0];
    if (!first) {
      replyErrors.set(thread.id, 'Thread has no top-level comment to reply to.');
      return;
    }
    sendingReplyIds.add(thread.id);
    replyErrors.delete(thread.id);
    try {
      await ReplyToPRThread(prReference(prRef), thread.id, first.databaseID, body);
      replyBodies.delete(thread.id);
      prThreads = ((await ListPRReviewThreads(prReference(prRef))) ?? []) as ReviewThread[];
    } catch (err) {
      const message = userFacingError(err);
      replyErrors.set(thread.id, message);
      error = message;
      throw err;
    } finally {
      sendingReplyIds.delete(thread.id);
    }
  }

  async function sendPRThreadToAgent(thread: ReviewThread): Promise<void> {
    if (getActiveTurn(threadId) !== null) return;
    const line = thread.line ? `:${thread.line}` : '';
    const content = [
      `Please address this PR review thread at ${thread.path}${line}.`,
      '',
      ...thread.comments.map((comment) => `${comment.authorLogin}: ${comment.body}`),
    ].join('\n');
    try {
      await SendMessage(threadId, content, []);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function openConflictView(): Promise<void> {
    if (!prRef || !prDetail) {
      conflictsError = 'PR details are not loaded.';
      return;
    }
    closeCILogView();
    conflictView = true;
    conflictsLoading = true;
    conflictsError = null;
    conflicts = null;
    conflictContentByPath.clear();
    conflictCollapsedPaths = new SvelteSet<string>();
    conflictFileLoads.clear();
    conflictExpandedFolds.clear();
    try {
      const result = await GetPRMergeConflicts(
        threadId,
        prReference(prRef),
        prDetail.baseRefName,
        prDetail.headRefName,
      );
      conflicts = {
        treeOID: String(result.treeOID ?? ''),
        baseLabel: String(result.baseLabel ?? `origin/${prDetail.baseRefName}`),
        headLabel: String(result.headLabel ?? prDetail.headRefName),
        paths: result.conflicted ? [...(result.paths ?? [])] : [],
        notes: result.conflicted ? { ...(result.notes ?? {}) } : {},
        messages: result.conflicted ? [...(result.messages ?? [])] : [],
      };
      conflictCollapsedPaths = new SvelteSet<string>(conflicts.paths);
      conflictsError = null;
      // Conflict files open expanded like the regular diff; the loads
      // fan out in parallel (one local git read per conflicted file).
      // A file whose load fails stays collapsed and surfaces the error.
      await Promise.all(conflicts.paths.map((path) => toggleConflictCollapsed(path)));
    } catch (err) {
      conflictsError = userFacingError(err);
    } finally {
      conflictsLoading = false;
    }
  }

  function closeConflictView(): void {
    conflictView = false;
    conflictsLoading = false;
    conflictsError = null;
  }

  async function toggleConflictCollapsed(path: string): Promise<void> {
    if (!conflicts) return;
    if (!conflictCollapsedPaths.has(path)) {
      conflictCollapsedPaths.add(path);
      return;
    }
    await ensureConflictFileLoaded(path);
    // A note-bearing file expands even when its content load failed —
    // the notes are the conflict's only signal (the path may not exist
    // in the merged tree). The load error still surfaces in the banner.
    if (conflictContentByPath.has(path) || conflicts.notes[path]?.length) {
      conflictCollapsedPaths.delete(path);
    }
  }

  async function toggleCollapseAll(): Promise<void> {
    if (conflictView) {
      const paths = conflicts?.paths ?? [];
      if (allCollapsed) {
        // Expanding a conflict file loads its content; an explicit
        // expand-all fans the loads out in parallel.
        await Promise.all(paths.map((path) => toggleConflictCollapsed(path)));
      } else {
        for (const path of paths) conflictCollapsedPaths.add(path);
      }
      return;
    }
    if (allCollapsed) {
      collapsedPaths.clear();
      for (const file of files) collapseOverrides.set(file.path, false);
    } else {
      for (const file of files) {
        collapsedPaths.add(file.path);
        collapseOverrides.set(file.path, true);
      }
    }
  }

  function expandConflictFold(path: string, foldId: number): void {
    const next = new Set(conflictExpandedFolds.get(path) ?? []);
    next.add(foldId);
    conflictExpandedFolds.set(path, next);
  }

  function resetCIState(): void {
    ciLoadSeq += 1;
    ciPipeline = null;
    ciLoading = false;
    ciError = null;
    closeCILogView();
  }

  async function loadCIJobs(): Promise<void> {
    if (scope !== 'pr' || !prRef) return;
    const seq = ++ciLoadSeq;
    ciLoading = true;
    try {
      const pipeline = (await GetPRCIJobs(prReference(prRef))) as CIPipeline;
      if (seq !== ciLoadSeq || disposed) return;
      ciPipeline = pipeline ?? null;
      ciError = null;
    } catch (err) {
      if (seq !== ciLoadSeq || disposed) return;
      ciError = userFacingError(err);
    } finally {
      if (seq === ciLoadSeq) ciLoading = false;
    }
  }

  async function openCIJobLog(stageName: string, job: CIJob): Promise<void> {
    if (!prRef || !job.logsAvailable || !job.id) return;
    // The log view and the conflict view both replace the diff body.
    conflictView = false;
    ciLogView = { stageName, job };
    ciLogSavedPath = null;
    await fetchCILog(job);
  }

  async function refreshCILog(): Promise<void> {
    const job = ciLogView?.job;
    if (!job) return;
    await fetchCILog(job);
  }

  async function fetchCILog(job: CIJob): Promise<void> {
    if (!prRef || !job.id) return;
    const seq = ++ciLogSeq;
    ciLogLoading = true;
    ciLogError = null;
    try {
      const result = (await GetPRCIJobLog(prReference(prRef), job.id)) as CIJobLogResult;
      if (seq !== ciLogSeq || disposed) return;
      ciLog = result;
    } catch (err) {
      if (seq !== ciLogSeq || disposed) return;
      ciLog = null;
      ciLogError = userFacingError(err);
    } finally {
      if (seq === ciLogSeq) ciLogLoading = false;
    }
  }

  function closeCILogView(): void {
    ciLogSeq += 1;
    ciLogView = null;
    ciLog = null;
    ciLogLoading = false;
    ciLogError = null;
    ciLogSavedPath = null;
  }

  async function saveCILog(): Promise<string | null> {
    const view = ciLogView;
    if (!prRef || !view?.job.id) return null;
    try {
      const path = String(await SavePRCIJobLog(prReference(prRef), view.job.id, view.job.name));
      if (ciLogView === view) ciLogSavedPath = path;
      return path;
    } catch (err) {
      if (ciLogView === view) ciLogError = userFacingError(err);
      return null;
    }
  }

  async function sendCILogToChat(): Promise<void> {
    const view = ciLogView;
    if (!prRef || !view) return;
    const path = await saveCILog();
    if (!path) return;
    const draft = getComposerDraftForPane(sourcePaneId);
    if (!draft) {
      ciLogError = 'The source chat pane is not available.';
      return;
    }
    const message = [
      `Investigate CI job \`${view.job.name}\` (${view.stageName}) on PR #${prRef.number} — status: ${view.job.status}.`,
      `Full log saved at: ${path}`,
    ].join('\n');
    const existing = draft.content.trim();
    draft.setContent(existing ? `${existing}\n\n${message}` : message);
  }

  async function ensureConflictFileLoaded(path: string): Promise<void> {
    if (conflictContentByPath.has(path)) return;
    const existing = conflictFileLoads.get(path);
    if (existing) {
      await existing;
      return;
    }
    const load = (async () => {
      try {
        const content = await GetMergeConflictFile(threadId, conflicts?.treeOID ?? '', path);
        conflictContentByPath.set(path, String(content ?? ''));
        conflictsError = null;
      } catch (err) {
        conflictsError = userFacingError(err);
      } finally {
        conflictFileLoads.delete(path);
      }
    })();
    conflictFileLoads.set(path, load);
    await load;
  }

  // Derived, not computed per call: the template asks per rendered comment
  // row, and anchor existence walks every file's display rows.
  const orphanedIds = $derived.by(() => {
    const out = new SvelteSet<string>();
    if (scope !== 'pr') return out;
    for (const comment of drafts) {
      if (!draftAnchorExists(files, comment)) out.add(comment.id);
    }
    return out;
  });

  function orphanedDraftIds(): SvelteSet<string> {
    return orphanedIds;
  }

  return {
    threadId,
    get scope() { return scope; },
    get baseBranch() { return baseBranch; },
    get prRef() { return prRef; },
    get prScopeLabel() { return prRef ? prScopeLabel(prRef) : null; },
    get patchText() { return patchText; },
    get sourceKey() { return sourceKey; },
    get files() { return files; },
    get comments() { return comments; },
    get drafts() { return drafts; },
    get openEditors() { return openEditors; },
    get commits() { return commits; },
    get selectedCommitSHA() { return selectedCommitSHA; },
    get edits() { return edits; },
    get editTurnLabels() { return editTurnLabels; },
    get selectedEditKey() { return editSelectionKey(selectedEdit); },
    get pendingEditItemID() { return pendingEditItemID; },
    set pendingEditItemID(value: string | null) { pendingEditItemID = value; },
    get pendingJumpFilePath() { return pendingJumpFilePath; },
    set pendingJumpFilePath(value: string | null) { pendingJumpFilePath = value; },
    get pendingJumpRowKey() { return pendingJumpRowKey; },
    get loading() { return loading; },
    get error() { return error; },
    get sendingComments() { return sendingComments; },
    get prDetail() { return prDetail; },
    get prThreads() { return prThreads; },
    get prHeadSHA() { return prHeadSHA; },
    get spanContext(): PatchScopeContext {
      return patchScopeContext();
    },
    get prStale() { return prStale; },
    get refreshingPRData() { return refreshingPRData; },
    get conflictView() { return conflictView; },
    get conflicts() { return conflicts; },
    get conflictsLoading() { return conflictsLoading; },
    get conflictsError() { return conflictsError; },
    get conflictContentByPath() { return conflictContentByPath; },
    get conflictCollapsedPaths() { return conflictCollapsedPaths; },
    get conflictFiles() { return conflictFiles; },
    get ciPipeline() { return ciPipeline; },
    get ciLoading() { return ciLoading; },
    get ciError() { return ciError; },
    get ciLogView() { return ciLogView; },
    get ciLog() { return ciLog; },
    get ciLogLoading() { return ciLogLoading; },
    get ciLogError() { return ciLogError; },
    get ciLogSavedPath() { return ciLogSavedPath; },
    get submitTarget() { return submitTarget; },
    get effectiveSubmitTarget() { return effectiveSubmitTarget; },
    get verdict() { return verdict; },
    get summaryBody() { return summaryBody; },
    get submitError() { return submitError; },
    get isTurnActive() { return isTurnActive; },
    get collapsedPaths() { return collapsedPaths; },
    get allCollapsed() { return allCollapsed; },
    get expandedPRThreadIds() { return expandedPRThreadIds; },
    get viewMode() { return viewMode; },
    get wordWrap() { return wordWrap; },

    setScope,
    selectCommit,
    selectEdit,
    reload,
    consumePendingJumpFilePath(): void {
      pendingJumpFilePath = null;
    },
    jumpToComment(item: CommentListItem): void {
      if (!item.inDiff) return;
      // The comment rows live on the diff surface — leave any
      // replacement view first.
      closeCILogView();
      closeConflictView();
      collapsedPaths.delete(item.filePath);
      if (item.threadId) expandedPRThreadIds.add(item.threadId);
      pendingJumpRowKey = item.rowKey;
    },
    consumePendingJumpRowKey(): void {
      pendingJumpRowKey = null;
    },
    openDraftEditor,
    closeDraftEditor,
    draftBodyFor(anchor: CommentAnchor): string {
      return draftBodies.get(anchorKey(anchor)) ?? '';
    },
    setDraftBody(anchor: CommentAnchor, body: string): void {
      draftBodies.set(anchorKey(anchor), body);
    },
    consumeDraftEditorFocus(anchor: CommentAnchor): boolean {
      if (pendingEditorFocusKey !== anchorKey(anchor)) return false;
      pendingEditorFocusKey = null;
      return true;
    },
    createComment,
    updateComment,
    deleteComment,
    sendComments,
    submitPRReview,
    setSubmitTarget(target: 'agent' | 'pr'): void {
      submitTarget = target;
    },
    setVerdict(nextVerdict: 'comment' | 'approve' | 'request-changes'): void {
      verdict = nextVerdict;
    },
    setSummaryBody(body: string): void {
      summaryBody = body;
    },
    orphanedDraftIds,
    togglePRThread(prThreadId: string): void {
      if (expandedPRThreadIds.has(prThreadId)) expandedPRThreadIds.delete(prThreadId);
      else expandedPRThreadIds.add(prThreadId);
    },
    replyBodyFor(prThreadId: string): string {
      return replyBodies.get(prThreadId) ?? '';
    },
    setReplyBody(prThreadId: string, body: string): void {
      replyBodies.set(prThreadId, body);
    },
    refreshPRThreads,
    sendPRThreadReply,
    sendPRThreadToAgent,
    replyErrorFor(prThreadId: string): string | null {
      return replyErrors.get(prThreadId) ?? null;
    },
    sendingReply(prThreadId: string): boolean {
      return sendingReplyIds.has(prThreadId);
    },
    openConflictView,
    closeConflictView,
    toggleConflictCollapsed,
    expandConflictFold,
    loadCIJobs,
    openCIJobLog,
    closeCILogView,
    refreshCILog,
    saveCILog,
    sendCILogToChat,
    expandDiffContext,
    toggleCollapsed(path: string): void {
      const collapsed = !collapsedPaths.has(path);
      if (collapsed) collapsedPaths.add(path);
      else collapsedPaths.delete(path);
      collapseOverrides.set(path, collapsed);
    },
    toggleCollapseAll,
    setViewMode(mode: 'stacked' | 'split'): void {
      viewMode = mode;
    },
    setWordWrap(wrap: boolean): void {
      wordWrap = wrap;
    },
    dispose,
  };
}

interface LoadedPatch {
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
  prDetail?: PRDetail;
  prThreads?: ReviewThread[];
  prHeadSHA?: string;
  subscriptionId?: string;
}

/** The edits-scope selection reload should aim for: a freshly pinned
 * tool call (inline-diff affordance) wins over the current selection. */
interface EditDesire {
  pinnedItemId: string | null;
  current: EditSelection | null;
}

/** State a selection-only reload reuses instead of refetching: the
 * commit/edit lists are unchanged by picking an entry, and in pr scope
 * the live subscription already holds the detail. */
interface ExistingLoad {
  commits: BranchCommit[];
  prDetail: PRDetail | null;
  edits: EditDiffEntryView[];
  editTurnLabels: ReadonlyMap<number, string>;
}

/** Decode a selector value (`item:<id>` / `turn:<n>`) against the
 * current entries; unknown or null values resolve to the default. */
function editSelectionFromKey(key: string | null, entries: readonly EditDiffEntryView[]): EditSelection | null {
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
function defaultEditSelection(entries: readonly EditDiffEntryView[]): EditSelection | null {
  if (entries.length === 0) return null;
  return { kind: 'turn', turnIndex: entries[entries.length - 1].turnIndex };
}

/** Validate a desired selection against the fresh list: a pinned tool
 * call wins; a stale selection falls back to the default. */
function resolveEditSelection(desire: EditDesire, entries: readonly EditDiffEntryView[]): EditSelection | null {
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
function resolveSelectedCommit(
  selectedCommitSHA: string | null,
  commits: readonly BranchCommit[],
): string | null {
  if (selectedCommitSHA && commits.some((commit) => commit.sha === selectedCommitSHA)) {
    return selectedCommitSHA;
  }
  return null;
}

async function loadPatch(
  threadId: string,
  scope: ReviewScope,
  baseBranch: string | null,
  selectedCommitSHA: string | null,
  prRef: PRRef | null,
  editDesire: EditDesire,
  existing?: ExistingLoad,
): Promise<LoadedPatch> {
  switch (scope) {
    case 'pr': {
      if (!prRef) throw new Error('No PR or MR is available for this thread.');
      if (existing) return loadPRPatchCommitOnly(threadId, prRef, selectedCommitSHA, existing);
      return loadPRPatch(threadId, prRef, selectedCommitSHA);
    }
    case 'workspace':
      return { patchText: ((await GetWorkspaceCurrentDiff(threadId)) ?? '') as string };
    case 'edits': {
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
      const branch = baseBranch?.trim() || await defaultBaseBranch(threadId);
      if (existing) {
        const commitSHA = resolveSelectedCommit(selectedCommitSHA, existing.commits);
        const patchText = commitSHA
          ? ((await GetCommitDiff(threadId, commitSHA)) ?? '') as string
          : ((await GetBranchBaseDiff(threadId, branch)) ?? '') as string;
        return { patchText, commits: existing.commits, selectedCommitSHA: commitSHA };
      }
      if (selectedCommitSHA) {
        // Sequenced: the selection must be validated against the fresh
        // list before deciding which diff to fetch.
        const commits = ((await ListBranchCommits(threadId, branch)) ?? []) as BranchCommit[];
        const commitSHA = resolveSelectedCommit(selectedCommitSHA, commits);
        const patchText = commitSHA
          ? ((await GetCommitDiff(threadId, commitSHA)) ?? '') as string
          : ((await GetBranchBaseDiff(threadId, branch)) ?? '') as string;
        return { patchText, commits, selectedCommitSHA: commitSHA };
      }
      const [commits, patchText] = await Promise.all([
        ListBranchCommits(threadId, branch).then((rows) => (rows ?? []) as BranchCommit[]),
        GetBranchBaseDiff(threadId, branch).then((patch) => (patch ?? '') as string),
      ]);
      return { patchText, commits, selectedCommitSHA: null };
    }
  }
}

async function loadPRPatch(
  threadId: string,
  ref: PRRef,
  selectedCommitSHA: string | null,
): Promise<LoadedPatch> {
  const pr = prReference(ref);
  // The subscription resolves the PR detail, whose baseRefName the diff
  // needs to compute a local three-dot diff (gh/glab's PR-diff API caps at
  // 20k lines; large PRs must go through the local-clone path). Sequenced,
  // not parallel — the base ref is only known once the detail lands.
  const subResult = await SubscribePRUpdates(threadId, pr);
  try {
    const detail = subResult.detail as PRDetail;
    const baseRef = detail?.baseRefName ?? '';
    const headSHA = String(subResult.headSHA ?? detail?.headSHA ?? '');
    // Per-commit PR review needs the local clone; without one the backend
    // returns an empty list and the selector stays hidden. The known head
    // SHA lets the backend skip its fetch when the objects are local.
    const commits = baseRef
      ? (((await ListPRCommits(threadId, pr, baseRef, headSHA)) ?? []) as BranchCommit[])
      : [];
    const commitSHA = resolveSelectedCommit(selectedCommitSHA, commits);
    const patchText = commitSHA
      ? String((await GetPRCommitDiff(threadId, pr, commitSHA)) ?? '')
      : String((await GetPRDiff(threadId, pr, baseRef)) ?? '');
    return {
      patchText,
      commits,
      selectedCommitSHA: commitSHA,
      prDetail: detail,
      prThreads: (subResult.threads ?? []) as ReviewThread[],
      prHeadSHA: headSHA,
      subscriptionId: String(subResult.id),
    };
  } catch (err) {
    await UnsubscribePRUpdates(String(subResult.id ?? ''));
    throw err;
  }
}

/** Commit-selection fast path: the caller's PR subscription stays live,
 * so only the diff itself is refetched — no re-subscribe, no detail or
 * commit-list round-trips. Returns no subscriptionId on purpose. */
async function loadPRPatchCommitOnly(
  threadId: string,
  ref: PRRef,
  selectedCommitSHA: string | null,
  existing: ExistingLoad,
): Promise<LoadedPatch> {
  const pr = prReference(ref);
  const baseRef = existing.prDetail?.baseRefName ?? '';
  const commitSHA = resolveSelectedCommit(selectedCommitSHA, existing.commits);
  const patchText = commitSHA
    ? String((await GetPRCommitDiff(threadId, pr, commitSHA)) ?? '')
    : String((await GetPRDiff(threadId, pr, baseRef)) ?? '');
  return { patchText, commits: existing.commits, selectedCommitSHA: commitSHA };
}

function prReference(ref: PRRef): Parameters<typeof GetPRDiff>[1] {
  return {
    Forge: ref.forge,
    Namespace: ref.namespace,
    Repo: ref.repo,
    Number: ref.number,
  };
}

async function defaultBaseBranch(threadId: string): Promise<string> {
  const branches = ((await GitListBranches(threadId)) ?? []) as GitBranch[];
  const defaultBranch = branches.find((branch) => branch.isDefault);
  if (!defaultBranch?.name) {
    throw new Error('default branch not found');
  }
  return defaultBranch.name;
}

function defaultCollapsedPaths(files: readonly PatchFile[]): SvelteSet<string> {
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

function storageKey(threadId: string): string {
  return `reviewScope:${threadId}`;
}

function readPersistedScope(threadId: string): PersistedReviewScope | null {
  const raw = appStorageGet(storageKey(threadId));
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<PersistedReviewScope>;
    if (!isReviewScope(parsed.scope)) return null;
    return {
      scope: parsed.scope,
      baseBranch: typeof parsed.baseBranch === 'string' ? parsed.baseBranch : null,
    };
  } catch {
    return null;
  }
}

function persistScope(threadId: string, scope: ReviewScope, baseBranch: string | null): void {
  appStorageSet(storageKey(threadId), JSON.stringify({ scope, baseBranch }));
}

function isReviewScope(value: unknown): value is ReviewScope {
  return value === 'workspace' || value === 'branch' || value === 'pr' || value === 'edits';
}

function userFacingError(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'Review diff failed.';
}

export function __resetReviewPaneStateForTest(): void {
  statesBySourcePane.clear();
  prStatesBySubscription.clear();
}

export type { CommentAnchor };
export type { DiffReviewComment };
