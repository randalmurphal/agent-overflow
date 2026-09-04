import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import {
  GetDiffContextLines,
  GetEditDiffContextLines,
  GetPRCIJobLog,
  GetPRDetail,
  GetThread,
  SavePRCIJobLog,
  ListPRReviewThreads,
  MarkDiffReviewCommentsSent,
  ReplyToPRThread,
  SendMessage,
  SetPRThreadResolved,
  SendDiffReviewComments,
  SubmitPRReview,
  VerifyEditDiffs,
} from './bindings';
import { openCompanion } from './companionPanes.svelte';
import { peekGitStatus } from './gitStatusStore.svelte';
import { NO_WORKSPACE_REF, workspaceKeyForThread } from '../utils/workspaceKey';
import { getPane } from './panes.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import {
  createDiffReviewComment,
  deleteDiffReviewComment,
  getDiffReviewComments,
  refreshDiffReviewComments,
  setActiveDiffReviewSource,
  updateDiffReviewComment,
} from './diffReviewComments.svelte';
import {
  applyPRSnapshot,
  applyPRThreads,
  attachPR,
  clearPRThreadResolveOverride,
  overriddenPRThreads,
  peekPRError,
  peekPRSnapshot,
  setPRThreadResolveOverride,
  type PRAttachment,
  type PRSnapshot,
} from './prReviewStore.svelte';
import { loadPRCIJobs, peekPRCI } from './prReviewCI.svelte';
import {
  ensurePRConflictFile,
  openPRConflicts,
  peekPRConflicts,
  permitPRConflictReconcile,
  type PRConflicts,
} from './prReviewConflicts.svelte';
import {
  defaultBaseBranch,
  defaultCollapsedPaths,
  draftAnchorExists,
  EDITS_NEEDS_THREAD,
  editSelectionFromKey,
  editSelectionKey,
  loadPatch,
  reviewLineCommentForDraft,
  supportsIgnoreWhitespace,
  type EditDiffEntryView,
  type EditSelection,
} from './reviewPaneLoad';
import { persistScope, readPersistedScope } from './reviewPaneScope';
import { hasScope } from '../transport/scopes';
import { getSettings } from './settings.svelte';
import { getActiveTurn } from './threadStatuses.svelte';
import type { BranchCommit, WorkspaceRef } from '../types/git';
import type {
  CIJob,
  CIJobLogResult,
  CIPipeline,
  DiffReviewComment,
  DiffReviewScope,
  PRDetail,
  ReviewThread,
  ReviewVerdict,
  SubmitPRReviewResult,
  Thread,
} from '../types/models';
import { diffSourceKey } from '../utils/diffSourceKey';
import { type PatchScopeContext } from '../utils/diffSpanCache.svelte';
import { conflictPatchFile } from '../utils/conflictFile';
import { hunkExcerptForComment } from '../utils/prHunkExcerpt';
import {
  prKey,
  prRefFromThread,
  prRefFromUrl,
  prReferenceWire,
  prScopeLabel,
  prSourceKey,
  type PRRef,
} from '../utils/prReference';
import {
  applyContextExpansion,
  expansionFetchRange,
  nextExpansionVersion,
  type ContextExpansionState,
  type ExpandDirection,
} from '../utils/diffContextExpansion';
import {
  mergePatchFilesByPathCached,
  parsePatchFilesCached,
  type DiffGap,
  type PatchFile,
} from '../utils/patchFiles';
import { anchorKey, type CommentAnchor } from '../utils/reviewRows';
import { sortFilesTreeOrder } from '../utils/reviewTree';
import type { CommentListItem } from '../utils/reviewComments';

export type ReviewScope = DiffReviewScope;

/** One row of the Conversation section's chronological feed: a review
 * thread's card, a verdict one-liner, or one contiguous run of pushed
 * commits by one author. Ids are prefixed per kind (`t:`/`v:`/`c:`) so
 * the frozen-order capture can hold all three still at once. */
export type ConversationFeedItem =
  | { kind: 'thread'; id: string; thread: ReviewThread }
  | { kind: 'verdict'; id: string; verdict: ReviewVerdict }
  | { kind: 'commits'; id: string; author: string; commits: readonly BranchCommit[] };

export interface ReviewPaneState {
  /** Subject this state was created for — the registry's staleness check.
   *  A thread row id, or a draft placeholder's synthetic id. */
  readonly identity: string;
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
  /** Live PR detail, shared by every pane on this PR. */
  readonly prDetail: PRDetail | null;
  readonly prThreads: readonly ReviewThread[];
  /** The head the LOADED diff was computed at — not the PR's live head.
   * Comment anchors, span context, and sent-marks all describe the diff
   * on screen, so they read this. */
  readonly prHeadSHA: string;
  /** The live PR data went stale on us: the poll pump or a refresh failed.
   * Separate from `error`, which owns the diff — the diff on screen is
   * still valid, only what surrounds it stopped updating. */
  readonly prUpdateError: string | null;
  /** Scope fields for parse-priming span requests — the same triple
   * the diff-context expansion sends (`app_review_diffs.go` scopes). */
  readonly spanContext: PatchScopeContext;
  /** The PR moved since this pane loaded its diff: derived from the live
   * head against the head this pane loaded at, so a push seen by one pane
   * can never mark another pane's freshly-loaded diff stale. */
  readonly prStale: boolean;
  readonly refreshingPRData: boolean;
  readonly conflictView: boolean;
  readonly conflicts: PRConflicts | null;
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
  /** "Hide whitespace changes" (`-w`). Per pane, default off, not persisted. */
  readonly ignoreWhitespace: boolean;
  /** Whether the current diff source can honor `ignoreWhitespace` — only
   * the gitdiff-backed patches can. See supportsIgnoreWhitespace. */
  readonly canIgnoreWhitespace: boolean;
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
  /** Optimistic resolve/unresolve: the thread flips at once (entity-level,
   * so every pane on the PR agrees) and the override holds against stale
   * poll snapshots until one agrees. A failure reverts and surfaces. */
  setPRThreadResolved(thread: ReviewThread, resolved: boolean): Promise<void>;
  resolveErrorFor(threadId: string): string | null;
  resolvingThread(threadId: string): boolean;
  /** Jump the diff body to a thread's row (conversation → diff). */
  jumpToDiffThread(thread: ReviewThread): void;
  // ------------------------------------------------------------------
  // The PR header's Conversation section: one chronological feed (newest
  // first) of thread cards, review verdicts, and commit pushes. Ordering
  // is FROZEN while the section is open: remote updates never reorder or
  // hide what the reader is looking at. Entries that arrive after the
  // capture count into `conversationNewCount` and join only on reveal.
  // ------------------------------------------------------------------
  readonly conversationOpen: boolean;
  /** The whole feed — thread cards, verdicts, commit pushes — in the
   * frozen chronological order (newest first). Empty while closed. */
  readonly conversationFeed: readonly ConversationFeedItem[];
  /** Feed entries that arrived after the frozen order was captured. */
  readonly conversationNewCount: number;
  /** Thread the section should scroll to; consumed by the section. */
  readonly pendingConversationThreadId: string | null;
  setConversationOpen(open: boolean): void;
  /** Fold the arrived-since-capture entries in (fresh chronological
   * order; reply folds already open stay open). */
  revealNewConversationThreads(): void;
  /** Whether a thread card's REPLIES are unfolded. The card's first
   * comment is always visible; settled threads fold replies by default. */
  conversationThreadExpanded(threadId: string): boolean;
  toggleConversationThread(threadId: string): void;
  /** Open the conversation section scrolled to one thread (inline strip
   * or rail row → conversation). */
  openConversationAt(threadId: string): void;
  consumePendingConversationThreadId(): void;
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
  /** Flips `-w` and re-requests the diff (the patch itself changes, so
   * this is a full reload, not a view toggle). */
  setIgnoreWhitespace(ignore: boolean): Promise<void>;
  dispose(): void;
}

export interface CILogView {
  stageName: string;
  job: CIJob;
}

const statesBySourcePane = new Map<string, ReviewPaneState>();

// Stable identity for "this pane is on no PR": ReviewDiffBody re-anchors
// the reader on a prThreads IDENTITY change, so a fresh [] per read would
// look like the review threads moved on every keystroke.
const EMPTY_PR_THREADS: readonly ReviewThread[] = Object.freeze([]);
const EMPTY_CONVERSATION_FEED: readonly ConversationFeedItem[] = Object.freeze([]);

// Same stability rule for the comment list a workspace-only pane reads.
const EMPTY_COMMENTS: readonly DiffReviewComment[] = Object.freeze([]);

/**
 * What a review pane is looking at. The four values travel together because
 * they answer different questions and only agree by accident:
 * `identity` keys the registry (a draft placeholder has one without a row),
 * `threadId` is the REAL row and is null until the draft materializes,
 * `workspace` is the checkout every workspace-scoped RPC addresses (the zero
 * ref means "no local clone", which is what a pr-anchor thread has), and
 * `thread` carries the row metadata the initial scope choice reads.
 */
export interface ReviewSubject {
  readonly identity: string;
  readonly threadId: string | null;
  readonly workspace: WorkspaceRef;
  readonly thread: Thread | null;
}

/** The one place a review subject is built. Structural in the pane so both
 *  `ThreadPane` and the narrower `PanelContext` projection satisfy it. */
export function reviewSubjectForPane(pane: {
  threadId: string | null;
  thread: Thread | null;
  workspace: WorkspaceRef | null;
}): ReviewSubject | null {
  const thread = pane.thread;
  if (!thread) return null;
  return {
    identity: thread.id,
    threadId: pane.threadId,
    workspace: pane.workspace ?? NO_WORKSPACE_REF,
    thread,
  };
}

export function reviewStateForPane(
  sourcePaneId: string,
  subject: ReviewSubject,
  opts: { deferInitialLoad?: boolean } = {},
): ReviewPaneState {
  const existing = statesBySourcePane.get(sourcePaneId);
  // Subject mismatch replaces rather than reuses: the CompanionPane {#key}
  // remount usually disposes the old state first, but correctness must not
  // depend on Svelte's destroy-before-create ordering.
  if (existing && existing.identity === subject.identity) return existing;
  // The replaced state may own a live PR-update subscription; drop it or
  // the Go-side poll pump outlives the state that could unsubscribe it.
  existing?.dispose();
  const state = createReviewPaneState(sourcePaneId, subject, opts.deferInitialLoad ?? false);
  statesBySourcePane.set(sourcePaneId, state);
  return state;
}

export function disposeReviewStateForPane(sourcePaneId: string, expectedIdentity?: string): void {
  const current = statesBySourcePane.get(sourcePaneId);
  if (expectedIdentity && current?.identity !== expectedIdentity) return;
  current?.dispose?.();
  statesBySourcePane.delete(sourcePaneId);
}

export async function openReviewCompanion(
  sourcePaneId: string,
  subject: ReviewSubject,
  opts: {
    scope?: ReviewScope;
    filePath?: string;
    /** Edits scope: the edit tool call to pin on load. */
    editItemId?: string;
  } = {},
): Promise<ReviewPaneState | null> {
  const companion = openCompanion(sourcePaneId, 'review');
  if (!companion) return null;
  const hasExplicitSelection = opts.scope !== undefined || opts.editItemId !== undefined;
  const state = reviewStateForPane(sourcePaneId, subject, { deferInitialLoad: hasExplicitSelection });
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

function createReviewPaneState(
  sourcePaneId: string,
  subject: ReviewSubject,
  deferInitialLoad: boolean,
): ReviewPaneState {
  const { identity, threadId, workspace } = subject;
  const initialThread = subject.thread;
  // Scope persistence is keyed on a real row; a draft placeholder has no
  // history to restore and nothing to write back.
  const persisted = threadId === null ? null : readPersistedScope(threadId);
  const initialPRRef = prRefFromThread(initialThread ?? {});
  // The thread row's own PR reference — set when the thread was created FROM
  // a pull request, and never rewritten afterwards, so resolving it once is
  // honest memoization. `undefined` means "not looked up yet".
  let threadPRRef: PRRef | null | undefined = $state(
    initialThread === null ? undefined : initialPRRef,
  );
  // Derived, not probed-once: the workspace fallback reads the live
  // git-status store, so the PR becomes selectable the moment status
  // lands (or a PR opens while the pane sits open) instead of only when
  // something re-enters pr scope.
  const prRef: PRRef | null = $derived(threadPRRef ?? workspacePRRef());
  let scope: ReviewScope = $state(
    initialPRRef && workspace.workspacePath === '' ? 'pr' : (persisted?.scope ?? 'workspace'),
  );
  let baseBranch: string | null = $state(persisted?.baseBranch ?? null);
  // The head THIS pane's diff was computed at, stamped with the PR it was
  // computed FOR. The PR's live head lives in the shared store; staleness
  // is the difference between the two, so a push observed once cannot mark
  // a pane that has already reloaded stale.
  //
  // The key half is load-bearing, not bookkeeping: a PR→PR switch changes
  // `prRef` (and therefore the live head this pane reads) synchronously,
  // while the anchor only moves when the new diff finishes loading. A bare
  // SHA would spend that window comparing the OLD PR's loaded head against
  // the NEW PR's live head — two unrelated OIDs, so the stale banner
  // flashed on a diff that had not even been requested yet.
  let loadedPRHead = $state<{ key: string; sha: string } | null>(null);
  let refreshingPRData = $state(false);
  // This pane's reference on the shared PR entity — held exactly while the
  // pane is in pr scope. The subscription, poll pump, CI pipeline and
  // conflict tree under it are shared with every other pane on the PR.
  let prAttachment: PRAttachment | null = null;
  let conflictView = $state(false);
  // The conflict view's reconcile permit, held for exactly as long as the
  // surface is on screen: a head move recomputes the merged tree only for
  // a view somebody is looking at (see prReviewConflicts.svelte.ts).
  let conflictReconcilePermit: (() => void) | null = null;
  let conflictCollapsedPaths: SvelteSet<string> = $state(new SvelteSet<string>());
  // Expanded fold ids per path. Entries are replaced wholesale on expand
  // so the SvelteMap write re-derives conflictFiles.
  const conflictExpandedFolds = new SvelteMap<string, ReadonlySet<number>>();
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
  const resolvingThreadIds: SvelteSet<string> = $state(new SvelteSet<string>());
  const resolveErrors = new SvelteMap<string, string>();
  // The PR header's Conversation section. The order and the
  // expanded-by-default set are CAPTURED, not derived: they must hold
  // still while the reader is in the section, whatever the poll pump
  // replaces underneath (see the interface comment).
  let conversationOpen = $state(false);
  let conversationOrder: readonly string[] = $state([]);
  let conversationDefaultExpanded: ReadonlySet<string> = $state(new Set<string>());
  const conversationExpandOverrides = new SvelteMap<string, boolean>();
  let pendingConversationThreadId: string | null = $state(null);
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
  // "Hide whitespace changes" (`-w`). Unlike viewMode/wordWrap this is
  // NOT a render option: it changes the patch git produces, so flipping
  // it re-requests the diff. Per pane and deliberately not persisted —
  // a hidden `-w` restored at startup would silently understate a diff.
  let ignoreWhitespace = $state(false);
  // Edits-scope files whose expansion attempt was refused (workspace
  // drifted from the historical diff): their gap rows retire for this
  // load. Cleared with the expansions on every reload.
  const unexpandableEditPaths = new SvelteSet<string>();
  // Edits-scope files the backend verified servable at load time
  // (VerifyEditDiffs) — the POSITIVE gate for gap arrows: a file's gaps
  // render only once its path lands here, so an arrow that can't serve
  // never appears. Cleared with the expansions on every reload.
  const editExpandablePaths = new SvelteSet<string>();
  let loadSeq = 0;
  // The shared PR entity this pane is looking at. Null outside pr scope:
  // the PR data survives a scope switch (another pane may still be on it),
  // but a pane that left is not reporting a PR's state as its own.
  const prEntityKey = $derived(scope === 'pr' && prRef ? prKey(prRef) : null);
  const prSnapshot = $derived<PRSnapshot | null>(peekPRSnapshot(prEntityKey));
  // A failed poll/refresh is user-facing state, not a log line — and it is
  // deliberately NOT `error` (which owns the diff): the rendered diff is
  // still valid, only the live PR data behind it went stale.
  const prUpdateError = $derived(peekPRError(prEntityKey));
  // The snapshot's threads through the optimistic resolve overrides.
  // A $derived, not a getter, for identity stability: ReviewDiffBody
  // re-anchors the reader on prThreads identity change, so a fresh
  // projection per read would look like the threads moved constantly.
  const prThreads = $derived.by<readonly ReviewThread[]>(() => {
    const threads = prSnapshot?.threads ?? EMPTY_PR_THREADS;
    return prEntityKey ? overriddenPRThreads(prEntityKey, threads) : threads;
  });
  // The live feed universe: every thread, verdict, and commit push the
  // section could show, chronological newest first. The frozen order is
  // captured FROM this and projected back ONTO it, so content updates
  // (new replies, resolve flips) flow through while position holds.
  const conversationFeedSource = $derived.by<readonly { timeMs: number; item: ConversationFeedItem }[]>(() => {
    const out: { timeMs: number; item: ConversationFeedItem }[] = [];
    for (const thread of prThreads) {
      const parsed = Date.parse(thread.comments[0]?.createdAt ?? '');
      out.push({
        timeMs: Number.isFinite(parsed) ? parsed : 0,
        item: { kind: 'thread', id: `t:${thread.id}`, thread },
      });
    }
    for (const verdict of prSnapshot?.detail?.latestReviews ?? []) {
      const parsed = Date.parse(verdict.submittedAt);
      out.push({
        timeMs: Number.isFinite(parsed) ? parsed : 0,
        item: { kind: 'verdict', id: `v:${verdict.authorLogin}:${verdict.submittedAt}`, verdict },
      });
    }
    // `commits` arrives newest first; one feed row per contiguous run by
    // one author, keyed by the run's OLDEST sha so a later push on top
    // starts a new row instead of re-identifying this one.
    let group: BranchCommit[] = [];
    const flush = () => {
      if (group.length === 0) return;
      out.push({
        timeMs: group[0].authoredAt,
        item: {
          kind: 'commits',
          id: `c:${group[group.length - 1].sha}`,
          author: group[0].author,
          commits: group,
        },
      });
      group = [];
    };
    for (const commit of commits) {
      if (group.length > 0 && group[0].author !== commit.author) flush();
      group.push(commit);
    }
    flush();
    out.sort((a, b) => b.timeMs - a.timeMs || a.item.id.localeCompare(b.item.id));
    return out;
  });
  // The frozen order projected onto the live universe.
  const conversationFeed = $derived.by<readonly ConversationFeedItem[]>(() => {
    if (!conversationOpen || conversationOrder.length === 0) return EMPTY_CONVERSATION_FEED;
    const byId = new Map(conversationFeedSource.map((entry) => [entry.item.id, entry.item]));
    const out: ConversationFeedItem[] = [];
    for (const id of conversationOrder) {
      const item = byId.get(id);
      if (item) out.push(item);
    }
    return out;
  });
  const conversationNewCount = $derived.by(() => {
    if (!conversationOpen) return 0;
    const known = new Set(conversationOrder);
    let count = 0;
    for (const entry of conversationFeedSource) {
      if (!known.has(entry.item.id)) count += 1;
    }
    return count;
  });
  const ciState = $derived(peekPRCI(prEntityKey));
  const conflictsState = $derived(peekPRConflicts(prEntityKey));
  // The loaded head, but only while it still describes the PR on screen.
  // Everything anchored to the diff — the stale banner, span context,
  // draft commitSha, sent-marks — reads this rather than the raw stamp, so
  // none of them can quote a head that belongs to another pull request.
  const loadedPRHeadSHA = $derived(
    loadedPRHead !== null && loadedPRHead.key === prEntityKey ? loadedPRHead.sha : '',
  );
  const prStale = $derived.by(() => {
    const live = prSnapshot?.headSHA ?? '';
    return live !== '' && loadedPRHeadSHA !== '' && live !== loadedPRHeadSHA;
  });
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
      return prSourceKey(prRef);
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
    let parsed: PatchFile[];
    if (scope === 'edits') {
      // A whole-turn concatenation repeats a path when a file was edited
      // more than once in the turn — merge those sections into one
      // renumbered file-ordered section per path (the review surface
      // keys rows/tree/collapse by path; see mergePatchFilesByPath).
      // The merge runs BEFORE tree sorting and through the identity
      // cache: this derived re-runs per expansion click, and only a
      // stable merged lines array keeps the expansion rebuild memo and
      // the span cache's predecessor fallback working — a fresh array
      // per run flashes the whole file plain for a round trip.
      // Gap arrows are verification-gated (merged files included): a
      // file's gaps render only after the load-time VerifyEditDiffs
      // pass proved an expansion request would be served (persisted
      // edit snapshot first, verified workspace file as the
      // pre-snapshot fallback), so no arrow is ever dead-on-arrival —
      // absolute paths outside the workspace, drifted pre-snapshot
      // files, and remote clients all simply never verify. A
      // click-time refusal (rare race) still retires the path via
      // unexpandableEditPaths. Copies, not mutation: the parse cache
      // is shared across panes and scopes.
      parsed = sortFilesTreeOrder(mergePatchFilesByPathCached(parsePatchFilesCached(patchText))).map(
        (file) =>
          editExpandablePaths.has(file.path) && !unexpandableEditPaths.has(file.path)
            ? file
            : { ...file, suppressGaps: true },
      );
    } else {
      parsed = sortFilesTreeOrder(parsePatchFilesCached(patchText));
    }
    if (contextExpansions.size === 0) return parsed;
    return parsed.map((file) => applyContextExpansion(file, contextExpansions.get(file.path)));
  });
  const conflictFiles = $derived.by(() => {
    const conflicts = conflictsState.state;
    if (!conflicts) return [];
    const { baseLabel, headLabel, notes } = conflicts;
    return conflicts.paths.map((path) => {
      const content = conflictsState.contentByPath.get(path);
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
      const paths = conflictsState.state?.paths ?? [];
      return paths.length > 0 && paths.every((path) => conflictCollapsedPaths.has(path));
    }
    return files.length > 0 && files.every((file) => collapsedPaths.has(file.path));
  });
  // The conflict viewer and the CI log view replace the diff body outright,
  // so flipping `-w` from either would reload a surface the user isn't
  // looking at.
  const canIgnoreWhitespace = $derived(
    !conflictView && ciLogView === null && supportsIgnoreWhitespace(scope, selectedCommitSHA),
  );
  // Review comments and turn state belong to a THREAD's history; a draft
  // placeholder has neither, so the comment affordances stay dark rather
  // than pointing at a row that does not exist yet.
  const comments = $derived(
    threadId === null ? EMPTY_COMMENTS : getDiffReviewComments(threadId, scope, sourceKey),
  );
  const drafts = $derived(comments.filter((comment) => comment.status === 'draft'));
  const isTurnActive = $derived(threadId !== null && getActiveTurn(threadId) !== null);

  // The workspace's CURRENT open PR, read live from the shared git-status
  // store. Not memoized: a PR opened while this pane sat open must become
  // selectable, and one that merged must stop being offered.
  function workspacePRRef(): PRRef | null {
    const status = peekGitStatus(workspaceKeyForThread(getPane(sourcePaneId)?.thread ?? null));
    if (!status) return null;
    return prRefFromUrl(
      String(status.forge ?? ''),
      String(status.openPrUrl ?? ''),
      Number(status.openPrNumber ?? 0),
    );
  }

  async function ensurePRRef(): Promise<PRRef | null> {
    if (threadPRRef === undefined) {
      // No row to ask: a draft placeholder's PR, if any, is the workspace's.
      if (threadId === null) threadPRRef = null;
      else {
        try {
          threadPRRef = prRefFromThread((await GetThread(threadId)) as Thread);
        } catch (err) {
          error = userFacingError(err);
          throw err;
        }
      }
    }
    return prRef;
  }

  // `prRef` derives from the git-status store live, but the thread-row
  // half still takes one GetThread round trip to memoize — kick it at
  // mount so a thread created FROM a PR surfaces that PR without waiting
  // for pr-scope entry.
  function probePRRef(): void {
    void ensurePRRef().catch(() => {
      // Not swallowed: ensurePRRef records a thread-lookup failure in
      // `error` before throwing, and no-PR resolves to null, not a throw.
    });
  }

  // Set by dispose(); a load that resolves after disposal must not write
  // back into a dead state — or hold a PR reference nobody will release.
  let disposed = false;

  // A pr-scope load that ran before `prRef` resolved is waiting on input,
  // not failed: a pane restored into persisted pr scope races the
  // git-status fetch at boot and used to stick on "No PR or MR is
  // available" until the user reloaded by hand. The flag is set by the
  // load that came up empty and consumed by the watcher below the moment
  // the derived ref lands.
  let awaitingPRRef = $state(false);
  const disposePRRefWatch = $effect.root(() => {
    $effect(() => {
      if (!awaitingPRRef || prRef === null) return;
      awaitingPRRef = false;
      void reload();
    });
  });

  /**
   * Take (or keep) this pane's reference on the shared PR entity. One
   * reference per pane at a time: re-entering pr scope or switching to a
   * different PR releases the previous one first, so the refcount under
   * the poll pump matches the panes actually looking at it.
   */
  function holdPR(ref: PRRef): PRAttachment | null {
    if (disposed) return null;
    // Every RPC behind this entity rides `git:operate` — the subscribe, the
    // detail read, the review threads, the CI jobs. A pane restored into a
    // saved layout mounts on boot, so a session without that grant would
    // spend one refusal per pane before anybody touched anything. The
    // no-attachment answer already exists for the disposed case, and the
    // pane renders its empty state from it.
    if (!hasScope('git:operate')) return null;
    const key = prKey(ref);
    if (prAttachment?.key === key) return prAttachment;
    releasePR();
    prAttachment = attachPR(key, { ref });
    return prAttachment;
  }

  function releasePR(): void {
    prAttachment?.release();
    prAttachment = null;
    // The anchor `prStale` is measured from describes a diff of THAT PR.
    // Carrying it to the next one would compare two different PRs' heads
    // and raise the banner on a diff that was never loaded.
    loadedPRHead = null;
  }

  function dispose(): void {
    disposed = true;
    disposePRRefWatch();
    resetConflictView();
    closeCILogView();
    releasePR();
  }

  // The one writer of `conflictView`, so the reconcile permit cannot drift
  // out of step with what is on screen: an unreleased permit keeps
  // recomputing merge trees for a surface nobody closed, and a missing one
  // leaves an open view rendering the previous head's merge.
  function setConflictView(open: boolean): void {
    conflictReconcilePermit?.();
    conflictReconcilePermit = null;
    conflictView = open;
    if (!open) return;
    const key = prEntityKey;
    if (key) conflictReconcilePermit = permitPRConflictReconcile(key);
  }

  // Only the pane's VIEW of the conflicts resets on a scope switch: the
  // merged tree belongs to the PR and may still be on screen in another
  // pane. It is released with this pane's reference.
  function resetConflictView(): void {
    setConflictView(false);
    conflictCollapsedPaths = new SvelteSet<string>();
    conflictExpandedFolds.clear();
  }

  async function setScope(nextScope: ReviewScope, opts?: { baseBranch?: string }): Promise<void> {
    const scopeChanged = nextScope !== scope;
    if (scopeChanged) {
      resetConflictView();
      closeCILogView();
      // The frozen ordering describes the previous scope's threads.
      setConversationOpen(false);
    }
    if (scope === 'pr' && nextScope !== 'pr') {
      releasePR();
    }
    if (nextScope === 'pr') {
      // Resolve before entry so the load below can hold the PR. A null
      // ref still ENTERS the scope: the load surfaces the user-facing
      // error, and the ref watcher retries the moment one resolves — a
      // badge click can race the git-status fetch by design.
      await ensurePRRef();
    }
    scope = nextScope;
    baseBranch = nextScope === 'branch'
      ? (opts?.baseBranch?.trim() || baseBranch || await defaultBaseBranch(workspace))
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
    if (threadId !== null) persistScope(threadId, scope, baseBranch);
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

  async function setIgnoreWhitespace(ignore: boolean): Promise<void> {
    if (ignore === ignoreWhitespace) return;
    ignoreWhitespace = ignore;
    // THIS IS A FULL DIFF RE-REQUEST, NOT A RENDER TOGGLE, and that cost
    // is accepted deliberately. `-w` changes the patch git emits — hunks
    // narrow, whitespace-only files vanish — so there is no projection of
    // the `-w` view out of the patch already in hand. Everything derived
    // from the patch text has to be rebuilt: parsed files, the px-pinned
    // row geometry, and the highlight-span cache (keyed by line content).
    // `selectionOnly` keeps it to the diff call — the commit/edit lists
    // and the PR subscription describe the same subject either way.
    openEditors = [];
    draftBodies.clear();
    // Collapse overrides deliberately SURVIVE, unlike selectCommit/selectEdit:
    // those switch which change is under review, whereas this shows the same
    // change with less noise. A file the user opened stays open.
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
    // selector list and the PR snapshot are still valid, so reuse them and
    // fetch just the diff. Only when a previous full load actually
    // populated them; otherwise fall through to a full load.
    const selectionOnly = opts?.selectionOnly === true
      && (scope === 'edits'
        ? edits.length > 0
        : commits.length > 0 && (scope !== 'pr' || prAttachment !== null));
    // The inline-diff affordance pins an edit for the NEXT load;
    // consumed here so a later manual reload doesn't re-pin it.
    const pinnedEditItemID = pendingEditItemID;
    pendingEditItemID = null;
    const seq = loadSeq + 1;
    loadSeq = seq;
    loading = true;
    error = null;
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
        if (seq !== loadSeq || disposed) return;
      }
      // The diff needs the PR detail's base ref (a local three-dot diff is
      // the only source without gh/glab's 20k-line cap), so the shared
      // snapshot is awaited before the patch call. Attaching is what
      // starts the poll pump — and re-attaching for a PR this pane already
      // holds costs nothing.
      let snapshot: PRSnapshot | null = null;
      // The PR this load COMMITS to, captured here and used for the rest of
      // it. `prRef` is reactive and the probe can re-resolve it under the
      // awaits below; re-reading it afterwards would let a diff fetched for
      // one pull request be stamped with another one's key.
      let loadingPRRef: PRRef | null = null;
      let loadingPRKey: string | null = null;
      if (scope === 'pr' && prRef) {
        awaitingPRRef = false;
        loadingPRRef = prRef;
        loadingPRKey = prKey(loadingPRRef);
        const hold = holdPR(loadingPRRef);
        snapshot = hold ? await hold.ready() : null;
        if (seq !== loadSeq || disposed) return;
      } else if (scope !== 'pr') {
        // Scope can change mid-load (the selector stays enabled while a PR
        // loads); a pane that is no longer on a PR holds no reference.
        releasePR();
        awaitingPRRef = false;
      } else {
        // pr scope with no resolvable ref: loadPatch will surface the
        // user-facing error, and the ref watcher retries if one appears.
        awaitingPRRef = true;
      }
      const loaded = await loadPatch(
        { workspace, threadId },
        scope,
        baseBranch,
        selectedCommitSHA,
        loadingPRRef ?? prRef,
        snapshot,
        { pinnedItemId: pinnedEditItemID, current: selectedEdit },
        // Gate on support, not just the flag: a scope switch can leave the
        // toggle on while the new source can't honor it, and passing it
        // anyway would make the button's state a lie.
        ignoreWhitespace && canIgnoreWhitespace,
        selectionOnly ? { commits, edits, editTurnLabels } : undefined,
      );
      if (seq !== loadSeq || disposed) return;
      commits = loaded.commits ?? [];
      edits = loaded.edits ?? [];
      editTurnLabels = loaded.editTurnLabels ?? new Map();
      if (loaded.selectedEdit !== undefined) {
        selectedEdit = loaded.selectedEdit;
      }
      if (loaded.selectedCommitSHA !== undefined) {
        selectedCommitSHA = loaded.selectedCommitSHA;
      }
      clearContextExpansions();
      patchText = loaded.patchText;
      // Fire-and-forget: arrows appear when verification lands; the
      // diff itself renders immediately (gaps just aren't expandable
      // yet).
      if (scope === 'edits') void verifyEditExpandability(seq);
      // Fresh defaults for the new patch, with the user's explicit
      // collapse/expand choices layered back on top.
      const nextCollapsed = defaultCollapsedPaths(parsePatchFilesCached(loaded.patchText));
      for (const [path, collapsed] of collapseOverrides) {
        if (collapsed) nextCollapsed.add(path);
        else nextCollapsed.delete(path);
      }
      collapsedPaths = nextCollapsed;
      if (scope === 'pr' && loadingPRKey && loadingPRRef) {
        // The anchor staleness is measured against: this diff describes
        // this head OF THIS PULL REQUEST, so the banner clears until the PR
        // moves again — for THIS pane, without touching what another pane
        // loaded, and without a switch to a different PR being compared
        // against a head that was never its own.
        loadedPRHead = { key: loadingPRKey, sha: loaded.prHeadSHA ?? loadedPRHeadSHA };
        // The fast path didn't refresh the PR snapshot, so CI state
        // hasn't moved either — the subscription pump covers it.
        if (!selectionOnly) void loadPRCIJobs(loadingPRKey, loadingPRRef);
      }
      // patchText and selectedCommitSHA are already updated above, so
      // the derived reflects this load — no need to re-derive by hand.
      const nextSourceKey = sourceKey;
      if (threadId === null) {
        // A draft placeholder owns no comment store to sync.
        error = null;
        return;
      }
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
      if (seq !== loadSeq || disposed) return;
      clearContextExpansions();
      patchText = '';
      openEditors = [];
      draftBodies.clear();
      collapsedPaths = new SvelteSet<string>();
      if (threadId !== null) setActiveDiffReviewSource(threadId, null);
      error = userFacingError(err);
    } finally {
      if (seq === loadSeq) loading = false;
    }
  }

  if (!deferInitialLoad) void reload();

  function clearContextExpansions(): void {
    unexpandableEditPaths.clear();
    editExpandablePaths.clear();
    if (contextExpansions.size === 0) return;
    contextExpansions.clear();
    contextExpansionVersion += 1;
  }

  // The scope fields the priming RPCs resolve new-side file content
  // from (`app_review_diffs.go`), and the SUBJECT each one resolves it
  // through: this subject's checkout for every scope but edits, whose
  // subject is the thread's own persisted history. A selected commit in
  // branch scope reads through 'commit' scope; pr scope reads the local
  // PR clone, at the selected commit when one is set (falling back to
  // the head).
  function patchScopeContext(): PatchScopeContext {
    if (scope === 'pr') {
      return {
        scope: 'pr',
        commitSHA: selectedCommitSHA ?? '',
        headSHA: loadedPRHeadSHA,
        workspace,
      };
    }
    if (selectedCommitSHA) {
      return { scope: 'commit', commitSHA: selectedCommitSHA, headSHA: '', workspace };
    }
    if (scope === 'edits') {
      // The edit selection routes the backend to that edit's persisted
      // file snapshots (workspace file as pre-snapshot fallback), and
      // content is served only after it verifies against the historical
      // patch (the request carries the patch as VerifyPatch); a drifted
      // file degrades to unprimed spans, never to wrong colors.
      return {
        scope,
        commitSHA: '',
        headSHA: '',
        workspace,
        // Non-null by construction: the edits scope is unreachable
        // without a thread row (ReviewPane offers no such option, and
        // `setScope` refuses it), so this never keys on ''.
        threadId: editsThreadId(),
        editPayloadId: selectedEdit?.kind === 'item' ? selectedEdit.payloadId : '',
        editTurnIndex: selectedEdit?.kind === 'turn' ? selectedEdit.turnIndex : -1,
      };
    }
    return { scope, commitSHA: '', headSHA: '', workspace };
  }

  // One file's patch text, serialized from its merged lines — the ONLY
  // way an edits-scope verifyPatch is built. The load-time verification
  // batch and the click-time expansion request both call this, so the
  // two verdicts compare the same bytes by construction.
  function filePatchText(file: PatchFile): string {
    return file.lines.map((line) => line.content).join('\n');
  }

  // The historical patch text of one edits-scope file, for the
  // backend's has-the-file-drifted verification. Empty for unknown
  // paths (the backend then refuses, which is the safe direction).
  function editVerifyPatch(path: string): string {
    if (scope !== 'edits') return '';
    const file = files.find((candidate) => candidate.path === path);
    if (!file) return '';
    return filePatchText(file);
  }

  // Load-time expandability pass for the edits scope: one batch RPC
  // proves which files an expansion click would actually serve, and
  // only those get gap arrows (editExpandablePaths gates the files
  // derived). Candidates come from the unsuppressed merge, NOT the
  // files derived — that one already suppresses everything still
  // unverified. Any failure (a session without `files:read`
  // included) just leaves paths unverified: no arrows, no error
  // banner, exactly what clicking would have found out the hard way.
  // Edits scope is unreachable without a real row: the option is not
  // rendered on a draft placeholder, and both the diff load and this
  // expansion path refuse it in the same words rather than no-oping.
  // Diff-review comments, comment sends and the steer-the-agent action all
  // address a thread ROW. None of their controls render on a draft
  // placeholder, so reaching one without a row is a bug rather than a
  // user-visible state — say so instead of no-oping.
  function commentThreadId(): string {
    if (threadId === null) throw new Error('Review comments need a started thread.');
    return threadId;
  }

  function editsThreadId(): string {
    if (threadId === null) throw new Error(EDITS_NEEDS_THREAD);
    return threadId;
  }

  async function verifyEditExpandability(seq: number): Promise<void> {
    if (scope !== 'edits' || threadId === null || !patchText) return;
    const merged = mergePatchFilesByPathCached(parsePatchFilesCached(patchText));
    // Added files are fully present — no gaps to gate, so no reason to
    // resolve them.
    const candidates = merged.filter(
      (file) => !file.suppressGaps && file.kind !== 'added' && !file.path.startsWith('/'),
    );
    if (candidates.length === 0) return;
    const context = patchScopeContext();
    try {
      const result = await VerifyEditDiffs(threadId, {
        editPayloadId: context.editPayloadId ?? '',
        editTurnIndex: context.editTurnIndex ?? -1,
        files: candidates.map((file) => ({
          path: file.path,
          verifyPatch: filePatchText(file),
        })),
      });
      if (seq !== loadSeq || disposed) return;
      for (const path of result.expandablePaths ?? []) {
        editExpandablePaths.add(path);
      }
    } catch {
      if (seq !== loadSeq || disposed) return;
      // Unverified stays unexpandable — the honest degrade.
    }
  }

  async function expandDiffContext(path: string, gap: DiffGap, dir: ExpandDirection): Promise<void> {
    const range = expansionFetchRange(gap, dir);
    if (!range) return;
    const seq = loadSeq;
    try {
      const context = patchScopeContext();
      const req = {
        scope: context.scope,
        commitSHA: context.commitSHA,
        headSHA: context.headSHA,
        path,
        startLine: range.start,
        endLine: range.end,
        verifyPatch: editVerifyPatch(path),
        editPayloadId: context.editPayloadId ?? '',
        editTurnIndex: context.editTurnIndex ?? -1,
      };
      // The edits scope's new side is a HISTORICAL file state owned by the
      // thread, so it has its own RPC; every live scope resolves out of the
      // checkout. Same request shape, two different subjects.
      const result = scope === 'edits'
        ? await GetEditDiffContextLines(editsThreadId(), req)
        : await GetDiffContextLines(workspace, req);
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
      await createDiffReviewComment(commentThreadId(), {
        scope,
        sourceKey,
        commitSha: selectedCommitSHA ?? (scope === 'pr' ? loadedPRHeadSHA : undefined),
        filePath: anchor.filePath,
        oldLine: anchor.oldLine,
        newLine: anchor.newLine,
        side: anchor.side,
        selectedText: anchor.selectedText ?? '',
        body: trimmed,
      });
      closeDraftEditor(anchor);
      setActiveDiffReviewSource(commentThreadId(), scope, sourceKey);
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
      await updateDiffReviewComment(commentThreadId(), scope, sourceKey, commentId, { body: trimmed });
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function deleteComment(commentId: string): Promise<void> {
    if (!sourceKey) return;
    try {
      await deleteDiffReviewComment(commentThreadId(), scope, sourceKey, commentId);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function sendComments(): Promise<void> {
    if (!sourceKey || drafts.length === 0 || sendingComments || isTurnActive) return;
    sendingComments = true;
    try {
      const detail = prSnapshot?.detail;
      await SendDiffReviewComments(commentThreadId(), scope, sourceKey, drafts.map((comment) => comment.id), {
        pr: scope === 'pr' && detail
          ? {
              number: detail.number,
              url: detail.url,
              comments: drafts.map((comment) => ({
                commentId: comment.id,
                hunkExcerpt: hunkExcerptForComment(files, comment),
              })),
            }
          : undefined,
      });
      await refreshDiffReviewComments(commentThreadId(), scope, sourceKey);
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
      const result = (await SubmitPRReview(prReferenceWire(prRef), {
        verdict,
        body: summaryBody.trim(),
        comments: submitDrafts.map(reviewLineCommentForDraft).filter((comment) => comment !== null),
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
        await MarkDiffReviewCommentsSent(commentThreadId(), scope, sourceKey, sent.map((comment) => comment.id), `pr:${loadedPRHeadSHA}`);
      }
      await refreshDiffReviewComments(commentThreadId(), scope, sourceKey);
      // Through the store: the posted review is now part of the PR, so
      // every pane looking at it shows the new threads.
      applyPRThreads(prKey(prRef), ((await ListPRReviewThreads(prReferenceWire(prRef))) ?? []) as ReviewThread[]);
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
  // or re-renders the patch). Applied through the store, so a head that
  // moved raises the stale banner on every pane whose diff predates it —
  // and on none whose diff doesn't. The diff never swaps mid-read.
  async function refreshPRThreads(): Promise<void> {
    if (scope !== 'pr' || !prRef || refreshingPRData) return;
    const key = prKey(prRef);
    refreshingPRData = true;
    try {
      const pr = prReferenceWire(prRef);
      const [detail, threads] = await Promise.all([
        GetPRDetail(pr) as Promise<PRDetail>,
        ListPRReviewThreads(pr) as Promise<ReviewThread[] | null>,
      ]);
      if (disposed || scope !== 'pr') return;
      applyPRSnapshot(key, {
        detail: detail ?? null,
        threads: threads ?? [],
        headSHA: String(detail?.headSHA ?? ''),
      });
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
      await ReplyToPRThread(prReferenceWire(prRef), thread.id, first.databaseID, body);
      replyBodies.delete(thread.id);
      applyPRThreads(prKey(prRef), ((await ListPRReviewThreads(prReferenceWire(prRef))) ?? []) as ReviewThread[]);
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
    if (isTurnActive) return;
    const line = thread.line ? `:${thread.line}` : '';
    const content = [
      `Please address this PR review thread at ${thread.path}${line}.`,
      '',
      ...thread.comments.map((comment) => `${comment.authorLogin}: ${comment.body}`),
    ].join('\n');
    try {
      await SendMessage(commentThreadId(), content, []);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function setPRThreadResolved(thread: ReviewThread, resolved: boolean): Promise<void> {
    const key = prEntityKey;
    if (!prRef || !key || resolvingThreadIds.has(thread.id)) return;
    resolvingThreadIds.add(thread.id);
    resolveErrors.delete(thread.id);
    // Optimistic and entity-level: every pane on the PR flips together,
    // and the override outranks in-flight poll snapshots until one agrees.
    setPRThreadResolveOverride(key, thread.id, resolved);
    try {
      await SetPRThreadResolved(prReferenceWire(prRef), thread.id, resolved);
    } catch (err) {
      clearPRThreadResolveOverride(key, thread.id);
      resolveErrors.set(thread.id, userFacingError(err));
    } finally {
      resolvingThreadIds.delete(thread.id);
    }
  }

  // ------------------------------------------------------------------
  // Conversation section
  // ------------------------------------------------------------------

  function threadSettled(thread: ReviewThread): boolean {
    return thread.isResolvable && (thread.isResolved || thread.isOutdated);
  }

  // Captures the feed order (chronological, newest first) and the
  // replies-unfolded-by-default set from the entries in hand.
  // `preserveExpanded` keeps folds that were already open open — a reveal
  // must not fold a thread's replies away because it was remotely
  // resolved while the reader had them open.
  function captureConversationOrder(preserveExpanded: boolean): void {
    conversationOrder = conversationFeedSource.map((entry) => entry.item.id);
    const expanded = new Set<string>();
    const present = new Set<string>();
    for (const entry of conversationFeedSource) {
      if (entry.item.kind !== 'thread') continue;
      present.add(entry.item.thread.id);
      if (!threadSettled(entry.item.thread)) expanded.add(entry.item.thread.id);
    }
    if (preserveExpanded) {
      for (const id of conversationDefaultExpanded) {
        if (present.has(id)) expanded.add(id);
      }
    }
    conversationDefaultExpanded = expanded;
  }

  function setConversationOpen(open: boolean): void {
    if (open === conversationOpen) return;
    conversationOpen = open;
    if (open) {
      // A fresh visit is a fresh view: the order and the reply-fold
      // defaults recompute and the previous visit's manual choices go.
      conversationExpandOverrides.clear();
      captureConversationOrder(false);
    } else {
      conversationOrder = [];
      conversationDefaultExpanded = new Set<string>();
      pendingConversationThreadId = null;
    }
  }

  function conversationThreadExpanded(prThreadId: string): boolean {
    return conversationExpandOverrides.get(prThreadId) ?? conversationDefaultExpanded.has(prThreadId);
  }

  function openConversationAt(prThreadId: string): void {
    setConversationOpen(true);
    // The target may still be behind the "N new" chip (it just arrived on
    // a poll); fold the arrivals in so the jump has somewhere to land.
    if (!conversationOrder.includes(`t:${prThreadId}`)) captureConversationOrder(true);
    if (!conversationThreadExpanded(prThreadId)) conversationExpandOverrides.set(prThreadId, true);
    pendingConversationThreadId = prThreadId;
  }

  // The merged tree and every conflicted file's content belong to the PR
  // (one merge-tree run serves every pane); what this pane owns is
  // whether the surface is showing and which files it has collapsed.
  async function openConflictView(): Promise<void> {
    const detail = prSnapshot?.detail;
    const key = prEntityKey;
    if (!prRef || !detail || !key) {
      // Unreachable from the UI (the affordance lives on the PR header,
      // which only renders with a detail), so it is not a conflict-load
      // failure — it is this pane having no PR to ask about.
      error = 'PR details are not loaded.';
      return;
    }
    closeCILogView();
    setConflictView(true);
    conflictExpandedFolds.clear();
    await openPRConflicts(key, workspace, prRef, detail);
    if (disposed) return;
    // Everything the store could show opens expanded, like the regular
    // diff. A file whose content read failed and that carries no notes has
    // nothing to render, so it stays collapsed (the error is in the
    // banner) — the same outcome the per-path expand loop produced.
    const conflicts = peekPRConflicts(key);
    conflictCollapsedPaths = new SvelteSet<string>(
      (conflicts.state?.paths ?? []).filter((path) => !conflictFileHasBody(path)),
    );
  }

  function conflictFileHasBody(path: string): boolean {
    const conflicts = conflictsState;
    return conflicts.contentByPath.has(path) || (conflicts.state?.notes[path]?.length ?? 0) > 0;
  }

  function closeConflictView(): void {
    setConflictView(false);
  }

  async function toggleConflictCollapsed(path: string): Promise<void> {
    const key = prEntityKey;
    if (!key || !conflictsState.state) return;
    if (!conflictCollapsedPaths.has(path)) {
      conflictCollapsedPaths.add(path);
      return;
    }
    await ensurePRConflictFile(key, path);
    // A note-bearing file expands even when its content load failed —
    // the notes are the conflict's only signal (the path may not exist
    // in the merged tree). The load error still surfaces in the banner.
    if (conflictFileHasBody(path)) {
      conflictCollapsedPaths.delete(path);
    }
  }

  async function toggleCollapseAll(): Promise<void> {
    if (conflictView) {
      const paths = conflictsState.state?.paths ?? [];
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

  async function loadCIJobs(): Promise<void> {
    const key = prEntityKey;
    if (!key || !prRef) return;
    await loadPRCIJobs(key, prRef);
  }

  async function openCIJobLog(stageName: string, job: CIJob): Promise<void> {
    if (!prRef || !job.logsAvailable || !job.id) return;
    // The log view and the conflict view both replace the diff body.
    setConflictView(false);
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
      const result = (await GetPRCIJobLog(prReferenceWire(prRef), job.id)) as CIJobLogResult;
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
      const path = String(await SavePRCIJobLog(prReferenceWire(prRef), view.job.id, view.job.name));
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

  // Orphan detection is only needed where a sourceKey outlives the patch
  // it was written against, so a draft can survive into a diff that no
  // longer shows its line:
  //
  //   - pr scope keys by PR number, so drafts survive head pushes.
  //   - a selected commit keys by SHA (`commit:<sha>`). The commit's
  //     content is immutable, but the RENDERED patch is not: `-w` drops
  //     the whitespace-only rows, so a draft anchored on one of them
  //     carries over with nowhere to land. Without this it would be
  //     invisible in the diff body yet still counted and still sent.
  //
  // Everything else content-hashes the patch, so a changed patch means a
  // changed key and no draft can carry over in the first place.
  // The `-w` half re-uses supportsIgnoreWhitespace rather than testing
  // selectedCommitSHA alone, so a SHA left over from another scope can
  // never turn this on somewhere the toggle was never applied.
  const sourceKeyOutlivesPatch = $derived(
    scope === 'pr'
    || (ignoreWhitespace && selectedCommitSHA !== null && supportsIgnoreWhitespace(scope, selectedCommitSHA)),
  );
  // Derived, not computed per call: the template asks per rendered comment
  // row, and anchor existence walks every file's display rows.
  const orphanedIds = $derived.by(() => {
    const out = new SvelteSet<string>();
    if (!sourceKeyOutlivesPatch) return out;
    for (const comment of drafts) {
      if (!draftAnchorExists(files, comment)) out.add(comment.id);
    }
    return out;
  });

  function orphanedDraftIds(): SvelteSet<string> {
    return orphanedIds;
  }

  return {
    identity,
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
    get prDetail() { return prSnapshot?.detail ?? null; },
    get prThreads() { return prThreads; },
    get conversationOpen() { return conversationOpen; },
    get conversationFeed() { return conversationFeed; },
    get conversationNewCount() { return conversationNewCount; },
    get pendingConversationThreadId() { return pendingConversationThreadId; },
    get prHeadSHA() { return loadedPRHeadSHA; },
    get prUpdateError() { return prUpdateError; },
    get spanContext(): PatchScopeContext {
      return patchScopeContext();
    },
    get prStale() { return prStale; },
    get refreshingPRData() { return refreshingPRData; },
    get conflictView() { return conflictView; },
    get conflicts() { return conflictsState.state; },
    get conflictsLoading() { return conflictsState.loading; },
    get conflictsError() { return conflictsState.error; },
    get conflictContentByPath() { return conflictsState.contentByPath; },
    get conflictCollapsedPaths() { return conflictCollapsedPaths; },
    get conflictFiles() { return conflictFiles; },
    get ciPipeline() { return ciState.pipeline; },
    get ciLoading() { return ciState.loading; },
    get ciError() { return ciState.error; },
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
    get ignoreWhitespace() { return ignoreWhitespace; },
    get canIgnoreWhitespace() { return canIgnoreWhitespace; },

    setScope,
    selectCommit,
    selectEdit,
    reload,
    consumePendingJumpFilePath(): void {
      pendingJumpFilePath = null;
    },
    jumpToComment(item: CommentListItem): void {
      if (!item.inDiff) {
        // No diff row to land on. A PR thread still has a conversation
        // card; a draft on a file outside the diff has neither, and the
        // rail expands it inline instead.
        if (item.threadId) openConversationAt(item.threadId);
        return;
      }
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
    setPRThreadResolved,
    resolveErrorFor(prThreadId: string): string | null {
      return resolveErrors.get(prThreadId) ?? null;
    },
    resolvingThread(prThreadId: string): boolean {
      return resolvingThreadIds.has(prThreadId);
    },
    jumpToDiffThread(thread: ReviewThread): void {
      if (!thread.path) return;
      // Same choreography as jumpToComment: the row lives on the diff
      // surface, so leave any replacement view first.
      closeCILogView();
      closeConflictView();
      collapsedPaths.delete(thread.path);
      expandedPRThreadIds.add(thread.id);
      pendingJumpRowKey = `pt:${thread.id}`;
    },
    setConversationOpen,
    revealNewConversationThreads(): void {
      captureConversationOrder(true);
    },
    conversationThreadExpanded,
    toggleConversationThread(prThreadId: string): void {
      conversationExpandOverrides.set(prThreadId, !conversationThreadExpanded(prThreadId));
    },
    openConversationAt,
    consumePendingConversationThreadId(): void {
      pendingConversationThreadId = null;
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
    setIgnoreWhitespace,
    dispose,
  };
}

function userFacingError(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'Review diff failed.';
}

export function __resetReviewPaneStateForTest(): void {
  for (const state of statesBySourcePane.values()) state.dispose();
  statesBySourcePane.clear();
}

export type { CommentAnchor };
export type { DiffReviewComment };
