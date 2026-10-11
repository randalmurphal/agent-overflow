import { threadMachine } from './attachedBackends.svelte';
import { companionSubjectKey } from './companionSubject';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { threadHasScope } from '../transport/entityScopes';
import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import {
  GetDiffContextLines,
  GetEditDiffContextLines,
  GetPRDetail,
  SavePRCIJobLog,
  SavePRCIJobLogSection,
  ListPRReviewThreads,
  MarkDiffReviewCommentsSent,
  ReplyToPRThread,
  SetPRThreadResolved,
  SendDiffReviewComments,
  SubmitPRReview,
  VerifyEditDiffs,
} from './bindings';
import { openCompanion } from './companionPanes.svelte';
import { peekGitStatus, peekGitStatusError } from './gitStatusStore.svelte';
import { workspaceKeyForThread } from '../utils/workspaceKey';
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
  peekPRFailure,
  peekPRSnapshot,
  setPRThreadResolveOverride,
  type PRAttachment,
  type PRSnapshot,
} from './prReviewStore.svelte';
import { peekPRCI, refreshPRCI, type PRCILogState } from './prReviewCI.svelte';
import type { ForgeFailure } from '../utils/forgeFailure';
import { prCILogFollowPending, refreshPRCILogFollows, setPRCILogFollow } from './prReviewCIFollows.svelte';
import {
  ensurePRConflictFile,
  openPRConflicts,
  peekPRConflicts,
  permitPRConflictReconcile,
  type PRConflicts,
} from './prReviewConflicts.svelte';
import {
  collapsedByDefault,
  defaultBaseBranch,
  draftAnchorExists,
  EDITS_NEEDS_THREAD,
  EditFileMerge,
  editSelectionFromKey,
  editSelectionKey,
  loadPatch,
  reviewLineCommentForDraft,
  seedEditPatchSpans,
  supportsIgnoreWhitespace,
  type EditDiffEntryView,
  type EditSelection,
  type LoadedPatch,
} from './reviewPaneLoad';
import type { ReviewDiffRead } from './reviewDiffStream';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { persistScope, readPersistedScope } from './reviewPaneScope';
import { getSettings } from './settings.svelte';
import { getActiveTurn } from './threadStatuses.svelte';
import type { BranchCommit, WorkspaceRef } from '../types/git';
import type {
  CIJob,
  CIPipeline,
  DiffReviewComment,
  DiffReviewScope,
  PRDetail,
  ReviewThread,
  ReviewVerdict,
  SubmitPRReviewResult,
  Thread,
} from '../types/models';
import { PaintedSpans, type PatchScopeContext } from '../utils/diffSpanCache.svelte';
import { conflictPatchFile } from '../utils/conflictFile';
import { hunkExcerptForComment } from '../utils/prHunkExcerpt';
import {
  prKey,
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
  readHunkHeadings,
  type ContextExpansionState,
  type ExpandDirection,
} from '../utils/diffContextExpansion';
import type { DiffGap } from '../utils/patchFiles';
import { reviewFileFromPatchFile, whenResident, type PatchParser, type ReviewFile } from '../utils/patchStore';
import { anchorKey, type CommentAnchor } from '../utils/reviewRows';
import { sortFilesTreeOrder } from '../utils/reviewTree';
import type { ReviewSectionId } from './reviewSectionSizes.svelte';
import { REVIEW_OVERVIEW_ROW_KEY } from '../utils/reviewRows';

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
   *  Conversation/draft identity, ownership epoch and checkout. */
  readonly identity: string;
  /** Span-cache owner for this review's diff bodies (`ReviewSubject.rowId`). */
  readonly rowId: string;
  readonly scope: ReviewScope;
  /** The computer this review's PR and workspace reads are routed to. */
  readonly backend: BackendKey;
  readonly baseBranch: string | null;
  readonly prRef: PRRef | null;
  readonly prScopeLabel: string | null;
  readonly sourceKey: string;
  /** The loaded diff's files in tree order. A load into an empty pane
   * publishes them as they arrive; a reload keeps the previous files
   * until the new diff is complete. */
  readonly files: ReviewFile[];
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
  /** Diff row key to jump to (a card's file:line, the unresolved
   * stepper, the title bar's peek controls); consumed by the diff body. */
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
  /** The pump's kind of prUpdateError (rate limited, a login to fix,
   * ...); null when the error has no kind or there is none. */
  readonly prUpdateFailure: ForgeFailure | null;
  /** Scope fields for parse-priming span requests — the same triple
   * the diff-context expansion sends (`app_review_diffs.go` scopes). */
  readonly spanContext: PatchScopeContext;
  /** The PR moved since this pane loaded its diff: derived from the live
   * head against the head this pane loaded at, so a push seen by one pane
   * can never mark another pane's freshly-loaded diff stale. */
  readonly prStale: boolean;
  /** PR scope waiting on input rather than failed: the workspace's git
   * status (which names the PR) has not been observed yet, or the PR is
   * held but its poller has no snapshot (the forge failed the first
   * fetch; it is retrying). The diff loads on its own once either lands. */
  readonly awaitingPR: boolean;
  readonly refreshingPRData: boolean;
  readonly conflictView: boolean;
  readonly conflicts: PRConflicts | null;
  readonly conflictsLoading: boolean;
  readonly conflictsError: string | null;
  readonly conflictContentByPath: SvelteMap<string, string>;
  readonly conflictCollapsedPaths: SvelteSet<string>;
  readonly conflictFiles: ReviewFile[];
  /** The colors the diff surface and the conflict surface last painted,
   * served to their lines while exact spans are in flight. */
  readonly paintedSpans: PaintedSpans;
  readonly conflictPaintedSpans: PaintedSpans;
  readonly ciPipeline: CIPipeline | null;
  /** Subscribed and no pipeline or failure observed yet. */
  readonly ciLoading: boolean;
  /** A manual CI refresh is in flight. */
  readonly ciRefreshing: boolean;
  readonly ciError: string | null;
  /** The kind of ciError; null for a refresh's own failure or none. */
  readonly ciFailure: ForgeFailure | null;
  /** The open job log. Its job is the live pipeline's row for that id. */
  readonly ciLogView: CILogView | null;
  /** The followed log; null until the first state arrives. */
  readonly ciLog: PRCILogState | null;
  /** A follow call (open or refresh) is in flight. */
  readonly ciLogLoading: boolean;
  readonly ciLogError: string | null;
  /** The kind of ciLogError; null for a local failure or none. */
  readonly ciLogFailure: ForgeFailure | null;
  /** False while the forge cannot serve the log: it has not published
   * the log yet (GitHub until the job's log blob exists, running or
   * completed). */
  readonly ciLogAvailable: boolean;
  readonly ciLogSavedPath: string | null;
  /** The open log's expanded sections, as `<job id>/<section key>`.
   * Empty when a log opens. */
  readonly ciLogOpenSections: ReadonlySet<string>;
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
  // The PR overview: the row above the first file holding the
  // Description and Conversation sections. It scrolls with the diff and
  // is unmounted while far below the viewport, so everything a reader
  // would notice losing lives here: section open state, each section
  // body's scroll offset, the unresolved stepper's cursor.
  // ------------------------------------------------------------------
  readonly descriptionOpen: boolean;
  setDescriptionOpen(open: boolean): void;
  /** A section body's scroll offset, restored when the row remounts. */
  overviewSectionScrollTop(section: ReviewSectionId): number;
  setOverviewSectionScrollTop(section: ReviewSectionId, px: number): void;
  /** Scroll the diff body back to the overview, opening `section`. */
  jumpToOverview(section?: ReviewSectionId): void;
  /** Open the Conversation section at `prThreadId`'s card and bring the
   * overview row back on screen: the one call every "open in conversation"
   * affordance uses, so none can open the section where it cannot be seen. */
  jumpToConversationThread(prThreadId: string): void;
  /** Unresolved threads in feed order (newest first). */
  readonly unresolvedThreads: readonly ReviewThread[];
  /** The thread the unresolved stepper last landed on. */
  readonly unresolvedCursor: string | null;
  /** Step to the next/previous unresolved thread. With the overview on
   * screen the conversation scrolls to its card; otherwise the diff jumps
   * to its row (a thread outside the diff goes to its card either way). */
  stepUnresolvedThread(direction: 1 | -1, inOverview: boolean): void;
  // ------------------------------------------------------------------
  // The Conversation section: one chronological feed (newest first) of
  // thread cards, review verdicts, and commit pushes. Ordering is FROZEN
  // from the first time the section renders it: remote updates never
  // reorder or hide what the reader is looking at. Entries that arrive
  // after the freeze count into `conversationNewCount` and join only on
  // reveal.
  // ------------------------------------------------------------------
  readonly conversationOpen: boolean;
  /** The whole feed — thread cards, verdicts, commit pushes — newest
   * first: the frozen order once captured, the live order before (the
   * section freezes it on first render). Empty while closed. */
  readonly conversationFeed: readonly ConversationFeedItem[];
  /** Whether the order is captured. */
  readonly conversationFrozen: boolean;
  /** Capture the current live order (the section's first render). */
  freezeConversation(): void;
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
  /** Has the PR's pump poll the pipeline now. */
  refreshCI(): Promise<void>;
  openCIJobLog(stageName: string, job: CIJob): void;
  closeCILogView(): void;
  /** Fetches the open job's log again. */
  refreshCILog(): void;
  saveCILog(): Promise<string | null>;
  sendCILogToChat(): Promise<void>;
  toggleCILogSection(sectionKey: string): void;
  /** Expands or collapses every listed section of the open log. */
  setCILogSectionsOpen(sectionKeys: readonly string[], open: boolean): void;
  /** Puts one section of the open log in the source chat's composer:
   * inline up to CI_SECTION_INLINE_MAX_BYTES, otherwise saved to a file
   * the message names. */
  sendCILogSectionToChat(section: CILogSectionSend): Promise<void>;
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
  jobId: string;
  job: CIJob;
}

/** One section of a job log, as sent to chat. */
export interface CILogSectionSend {
  name: string;
  /** A CI status, or a section kind with no result (utils/ciLogSections). */
  status: string;
  text: string;
  /** The shown log starts partway through this section. */
  truncatedTop: boolean;
}

/** A section's text goes into the composer inline up to this size in
 * UTF-8 bytes; a longer one is saved to a file the message names. */
export const CI_SECTION_INLINE_MAX_BYTES = 64 * 1024;

const CI_RESULT_STATUSES = new Set(['success', 'failed', 'running', 'pending', 'canceled', 'skipped', 'manual']);

/** A fence longer than any backtick run in `text`. */
function codeFence(text: string): string {
  const longest = (text.match(/`+/g) ?? []).reduce((max, run) => Math.max(max, run.length), 0);
  return '`'.repeat(Math.max(3, longest + 1));
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
 * What a review pane is looking at. The values travel together because
 * they answer different questions and only agree by accident:
 * `identity` keys the registry (a draft placeholder has one without a row),
 * `threadId` is the REAL row and is null until the draft materializes,
 * `rowId` is the pane's thread row id (a draft placeholder's synthetic one)
 * and owns the diff span-cache entries, which thread switch, delete and
 * draft materialization address by row id, and `workspace` is the checkout
 * every workspace-scoped RPC addresses.
 */
export interface ReviewSubject {
  readonly identity: string;
  readonly threadId: string | null;
  readonly rowId: string;
  readonly workspace: WorkspaceRef;
}

/** The one place a review subject is built. Structural in the pane so both
 *  `ThreadPane` and the narrower `PanelContext` projection satisfy it. */
export function reviewSubjectForPane(pane: {
  threadId: string | null;
  thread: Thread | null;
  workspace: WorkspaceRef | null;
}): ReviewSubject | null {
  const workspace = pane.workspace;
  if (!pane.thread || !workspace) return null;
  return {
    identity: companionSubjectKey(pane),
    threadId: pane.threadId,
    rowId: pane.thread.id,
    workspace,
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

/**
 * Dispose `state` if it is still the pane's registered state. A state that
 * was already replaced was disposed by `reviewStateForPane`, and its
 * successor belongs to a newer mount.
 */
export function disposeReviewStateForPane(sourcePaneId: string, state: ReviewPaneState): void {
  if (statesBySourcePane.get(sourcePaneId) !== state) return;
  statesBySourcePane.delete(sourcePaneId);
  state.dispose();
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
  const { identity, threadId, rowId, workspace } = subject;
  const backend = threadMachine(threadId ?? '', workspace.projectId);
  function computerPRKey(ref: PRRef): string { return composeWorkspaceKey(backend, prKey(ref)); }
  // Scope persistence is keyed on a real row; a draft placeholder has no
  // history to restore and nothing to write back.
  const persisted = threadId === null ? null : readPersistedScope(threadId);
  // Derived, not probed-once: the PR is the workspace's, read from the live
  // git-status store, so it becomes selectable the moment status lands (or a
  // PR opens while the pane sits open) instead of only when something
  // re-enters pr scope.
  const prRef: PRRef | null = $derived(workspacePRRef());
  let scope: ReviewScope = $state(persisted?.scope ?? 'workspace');
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
  // pane is in pr scope. The subscription, poll pump and CI pipeline under
  // it are shared with every other pane on the PR, and the conflict tree
  // with every pane on the PR in this checkout.
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
  // The job log this pane opened, as captured at open; `ciLogView` derives
  // the live row from the pipeline. The log itself is the PR's (shared by
  // every pane following that job); this pane holds a follow on it under
  // its own token, against the PR key it was opened on.
  let ciLogOpen = $state<{ key: string; stageName: string; job: CIJob & { id: string } } | null>(null);
  const ciFollowToken = Symbol('review-ci-log');
  // Save and send failures; the log's own failures are on its state.
  let ciLogLocalError: string | null = $state(null);
  let ciLogSavedPath: string | null = $state(null);
  // Expanded log sections, `<job id>/<section key>`. Nothing opens on its
  // own, and each log open starts collapsed.
  const ciLogOpenSections = new SvelteSet<string>();
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
  let descriptionOpen = $state(false);
  const overviewScrollTops = new SvelteMap<ReviewSectionId, number>();
  let unresolvedCursor: string | null = $state(null);
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
  // The loaded diff's files, tree-sorted (edits: merged by path), and the
  // content key of the whole patch for the scopes that key comments by
  // content. The key is set once the patch is complete.
  let loadedFiles: ReviewFile[] = $state.raw([]);
  let patchKey = $state('');
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
  let navigationSeq = 0;
  // The shared PR entity this pane is looking at. Null outside pr scope:
  // the PR data survives a scope switch (another pane may still be on it),
  // but a pane that left is not reporting a PR's state as its own.
  const prEntityKey = $derived(scope === 'pr' && prRef ? computerPRKey(prRef) : null);
  const prSnapshot = $derived<PRSnapshot | null>(peekPRSnapshot(prEntityKey));
  // A failed poll/refresh is user-facing state, not a log line — and it is
  // deliberately NOT `error` (which owns the diff): the rendered diff is
  // still valid, only the live PR data behind it went stale.
  const prUpdateError = $derived(peekPRError(prEntityKey));
  const prUpdateFailure = $derived(peekPRFailure(prEntityKey));
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
  // The frozen order projected onto the live universe; the live order
  // itself until the section has rendered once and frozen it.
  const conversationFeed = $derived.by<readonly ConversationFeedItem[]>(() => {
    if (!conversationOpen) return EMPTY_CONVERSATION_FEED;
    if (conversationOrder.length === 0) {
      return conversationFeedSource.length === 0
        ? EMPTY_CONVERSATION_FEED
        : conversationFeedSource.map((entry) => entry.item);
    }
    const byId = new Map(conversationFeedSource.map((entry) => [entry.item.id, entry.item]));
    const out: ConversationFeedItem[] = [];
    for (const id of conversationOrder) {
      const item = byId.get(id);
      if (item) out.push(item);
    }
    return out;
  });
  const unresolvedThreads = $derived.by<readonly ReviewThread[]>(() => {
    const out: ReviewThread[] = [];
    for (const entry of conversationFeedSource) {
      if (entry.item.kind !== 'thread') continue;
      const thread = entry.item.thread;
      if (thread.isResolvable && !thread.isResolved && !thread.isOutdated) out.push(thread);
    }
    return out;
  });
  const conversationNewCount = $derived.by(() => {
    if (!conversationOpen || conversationOrder.length === 0) return 0;
    const known = new Set(conversationOrder);
    let count = 0;
    for (const entry of conversationFeedSource) {
      if (!known.has(entry.item.id)) count += 1;
    }
    return count;
  });
  const ciState = $derived(peekPRCI(prEntityKey));
  // The open job as the pipeline lists it now, so status, steps and
  // duration move while the log is watched. A job the pipeline no longer
  // lists (the head moved on) keeps the row captured at open.
  //
  // Both read the PR the log was opened on: a PR switch moves
  // `prEntityKey` before the reload that releases the old PR closes the
  // view, and in between this pane must not show one PR's job against
  // another's pipeline.
  const ciLogView = $derived.by<CILogView | null>(() => {
    const open = ciLogOpen;
    if (!open) return null;
    const live = findCIJob(peekPRCI(open.key).pipeline, open.job.id);
    return {
      stageName: live?.stageName ?? open.stageName,
      jobId: open.job.id,
      job: live?.job ?? open.job,
    };
  });
  const ciLog = $derived<PRCILogState | null>(
    ciLogOpen ? (peekPRCI(ciLogOpen.key).logs.get(ciLogOpen.job.id) ?? null) : null,
  );
  const conflictsState = $derived(peekPRConflicts(prEntityKey, workspace));
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
    return patchKey;
  });
  // One gap-suppressed copy per loaded file, so a file keeps its identity
  // across rebuilds of the overlay below.
  const suppressedFiles = new WeakMap<ReviewFile, ReviewFile>();
  function gapsSuppressed(file: ReviewFile): ReviewFile {
    let copy = suppressedFiles.get(file);
    if (!copy) {
      copy = { ...file, suppressGaps: true };
      suppressedFiles.set(file, copy);
    }
    return copy;
  }
  // Hunk-gap expansions overlay per file, keyed by the version counter.
  // Expansion clicks and edit gap gating re-run only this overlay, so
  // every unexpanded file keeps its identity and the per-file memos
  // downstream (display rows, span keys) stay warm.
  const files = $derived.by(() => {
    void contextExpansionVersion;
    let parsed = loadedFiles;
    if (scope === 'edits') {
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
      parsed = parsed.map((file) =>
        editExpandablePaths.has(file.path) && !unexpandableEditPaths.has(file.path)
          ? file
          : gapsSuppressed(file),
      );
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
        return reviewFileFromPatchFile(conflictPatchFile(path, content ?? '', {
          baseLabel,
          headLabel,
          notes: pathNotes,
          expandedFolds: conflictExpandedFolds.get(path),
        }));
      }
      return reviewFileFromPatchFile({ path, kind: 'conflict', additions: 0, deletions: 0, lines: [] });
    });
  });
  // The colors each surface last painted (see PaintedSpans), retained
  // for exactly the files that surface shows. Lines keep them while
  // their file's exact spans are in flight after an expansion, a reload
  // or a recompute.
  const paintedSpans = new PaintedSpans();
  const conflictPaintedSpans = new PaintedSpans();
  const disposePaintedRetention = $effect.root(() => {
    $effect(() => {
      paintedSpans.retain(files.map((file) => file.path));
    });
    $effect(() => {
      // A recompute after a push empties the tree until the new one
      // lands; the painted colors carry across that gap.
      if (!conflictView) conflictPaintedSpans.clear();
      else if (conflictsState.state) conflictPaintedSpans.retain(conflictsState.state.paths);
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

  // Set by dispose(); a load that resolves after disposal must not write
  // back into a dead state — or hold a PR reference nobody will release.
  let disposed = false;

  // The read whose files the pane shows. It can hold the diff's handle to
  // read evicted text again, so it is disposed when its files go.
  let shownRead: ReviewDiffRead | null = null;

  function showRead(read: ReviewDiffRead | null): void {
    if (read === shownRead) return;
    shownRead?.dispose();
    shownRead = read;
    read?.onLost((err) => {
      if (shownRead !== read || disposed) return;
      reportFrontendDiagnostic('review diff: evicted text could not be read again', err.message);
      void reload({ selectionOnly: true });
    });
  }

  // A pr-scope load that ran before `prRef` resolved is waiting on input,
  // not failed: a pane restored into persisted pr scope races the
  // git-status fetch at boot and used to stick on "No PR or MR is
  // available" until the user reloaded by hand. The flag is set by the
  // load that came up empty and consumed by the watcher below the moment
  // the derived ref lands.
  let awaitingPRRef = $state(false);
  // The same shape one step later: the PR is held but its poller has no
  // snapshot yet (the forge failed the first fetch). The load that found
  // no detail sets this; the watcher reloads the moment the pump's
  // recovery frame lands.
  let awaitingPRDetail = $state(false);
  // One step earlier still: pr scope before the workspace's git status
  // has answered which PR the branch has. A pane remounted on a thread,
  // or restored at boot, reaches reload before the status store has a
  // status for its workspace, or with the fast first status whose PR
  // lookup is still pending (`openPrLookupPending`); either is "not yet",
  // not "no PR".
  let awaitingPRStatus = $state(false);
  const awaitingPR = $derived(awaitingPRDetail || awaitingPRStatus);
  function workspacePRLookupSettled(): boolean {
    const key = workspaceKeyForThread(getPane(sourcePaneId)?.thread ?? null);
    const status = peekGitStatus(key);
    if (status !== null) return !status.openPrLookupPending;
    return peekGitStatusError(key) !== null;
  }
  const disposePRRefWatch = $effect.root(() => {
    $effect(() => {
      if (!awaitingPRRef || prRef === null) return;
      awaitingPRRef = false;
      void reload();
    });
    $effect(() => {
      // Status landed, with or without a PR: the load runs now, so a
      // workspace with no PR answers "no PR" instead of waiting forever.
      if (!awaitingPRStatus || !workspacePRLookupSettled()) return;
      awaitingPRStatus = false;
      void reload();
    });
    $effect(() => {
      if (!awaitingPRDetail || !prSnapshot?.detail) return;
      awaitingPRDetail = false;
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
    if (!threadHasScope('git:operate', subject.threadId, subject.workspace.projectId)) return null;
    const key = computerPRKey(ref);
    if (prAttachment?.key === key) return prAttachment;
    releasePR();
    prAttachment = attachPR(key, { ref });
    return prAttachment;
  }

  function releasePR(): void {
    // The log view belongs to the PR being released, and so does its follow.
    closeCILogView();
    prAttachment?.release();
    prAttachment = null;
    // The anchor `prStale` is measured from describes a diff of THAT PR.
    // Carrying it to the next one would compare two different PRs' heads
    // and raise the banner on a diff that was never loaded.
    loadedPRHead = null;
  }

  function dispose(): void {
    disposed = true;
    showRead(null);
    disposePRRefWatch();
    disposePaintedRetention();
    paintedSpans.clear();
    conflictPaintedSpans.clear();
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
    if (key) conflictReconcilePermit = permitPRConflictReconcile(key, workspace);
  }

  // Only the pane's VIEW of the conflicts resets on a scope switch: the
  // merged tree belongs to the PR in this checkout and may still be on
  // screen in another pane. It is released with this pane's reference.
  function resetConflictView(): void {
    setConflictView(false);
    conflictCollapsedPaths = new SvelteSet<string>();
    conflictExpandedFolds.clear();
  }

  async function setScope(nextScope: ReviewScope, opts?: { baseBranch?: string }): Promise<void> {
    const navigation = ++navigationSeq;
    const scopeChanged = nextScope !== scope;
    const nextBaseBranch = nextScope === 'branch'
      ? (opts?.baseBranch?.trim() || baseBranch || await defaultBaseBranch(workspace))
      : null;
    if (navigation !== navigationSeq || disposed) return;
    if (scopeChanged) {
      resetConflictView();
      closeCILogView();
      // The frozen ordering describes the previous scope's threads.
      resetConversationView();
    }
    if (scope === 'pr' && nextScope !== 'pr') releasePR();
    if (scopeChanged || nextBaseBranch !== baseBranch) retireDiffSubject();
    scope = nextScope;
    baseBranch = nextBaseBranch;
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
    navigationSeq++;
    retireDiffSubject();
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
    navigationSeq++;
    retireDiffSubject();
    selectedEdit = next;
    openEditors = [];
    draftBodies.clear();
    collapseOverrides.clear();
    await reload({ selectionOnly: true });
  }

  // A new subject cannot reuse the previous patch under its new scope,
  // anchors or span context. Same-subject refreshes intentionally retain it.
  function retireDiffSubject(): void {
    loadedFiles = [];
    showRead(null);
    patchKey = '';
    clearContextExpansions();
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
      // The diff needs the PR detail's base ref, so the shared snapshot is
      // awaited before the patch call. Attaching is what
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
        awaitingPRStatus = false;
        loadingPRRef = prRef;
        loadingPRKey = computerPRKey(loadingPRRef);
        const hold = holdPR(loadingPRRef);
        snapshot = hold ? await hold.ready() : null;
        if (seq !== loadSeq || disposed) return;
        if (hold && !snapshot?.detail) {
          // Held, but the poller has nothing yet: its failure banner says
          // why, and the watcher above reloads when the snapshot lands.
          awaitingPRDetail = true;
          return;
        }
        awaitingPRDetail = false;
      } else if (scope !== 'pr') {
        // Scope can change mid-load (the selector stays enabled while a PR
        // loads); a pane that is no longer on a PR holds no reference.
        releasePR();
        awaitingPRRef = false;
        awaitingPRStatus = false;
        awaitingPRDetail = false;
      } else {
        // pr scope with no resolvable ref. Before the workspace's status has
        // been observed there is nothing to say yet: wait, and the watcher
        // above reloads when it lands. Once it has, loadPatch surfaces the
        // user-facing error, and the ref watcher retries if a PR appears.
        awaitingPRRef = true;
        if (!workspacePRLookupSettled()) {
          awaitingPRStatus = true;
          return;
        }
        awaitingPRStatus = false;
      }
      const loaded = await loadPatch(
        { workspace, threadId, backend },
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
      if (seq !== loadSeq || disposed) {
        loaded.patch?.abandon();
        return;
      }
      const arrival: Arrival = { loaded, applied: false };
      const read = await readPatch(seq, arrival);
      if (!read) return;
      // Everything the complete diff decides lands in this synchronous
      // step, so no frame shows its files with another load's collapse
      // state, patch key or head.
      showFiles(arrival, read.files);
      showRead(read.read);
      patchKey = read.patchKey;
      // Fresh defaults for the new patch, with the user's explicit
      // collapse/expand choices layered back on top.
      const nextCollapsed = new SvelteSet<string>();
      for (const file of loadedFiles) {
        if (collapsedByDefault(file)) nextCollapsed.add(file.path);
      }
      for (const [path, collapsed] of collapseOverrides) {
        if (collapsed) nextCollapsed.add(path);
        else nextCollapsed.delete(path);
      }
      collapsedPaths = nextCollapsed;
      // Fire-and-forget: arrows appear when verification lands; the
      // diff itself renders immediately (gaps just aren't expandable
      // yet).
      if (scope === 'edits') void verifyEditExpandability(seq);
      if (scope === 'pr' && loadingPRKey && loadingPRRef) {
        // The anchor staleness is measured against: this diff describes
        // this head OF THIS PULL REQUEST, so the banner clears until the PR
        // moves again — for THIS pane, without touching what another pane
        // loaded, and without a switch to a different PR being compared
        // against a head that was never its own. The backend reports the
        // head it computed the diff at, which its fetch can move past the
        // snapshot's.
        loadedPRHead = { key: loadingPRKey, sha: read.headSha || (loaded.prHeadSHA ?? loadedPRHeadSHA) };
      }
      // The files, patch key and selection are already updated above, so
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
      loadedFiles = [];
      showRead(null);
      patchKey = '';
      openEditors = [];
      draftBodies.clear();
      collapsedPaths = new SvelteSet<string>();
      if (threadId !== null) setActiveDiffReviewSource(threadId, null);
      error = userFacingError(err);
    } finally {
      if (seq === loadSeq) loading = false;
    }
  }

  // Interval between publishes of a diff still arriving.
  const PROGRESS_PUBLISH_MS = 100;
  // One VerifyEditDiffs call: the files the backend verifies per call,
  // and patch characters that stay far under a transport frame.
  const VERIFY_BATCH_FILES = 200;
  const VERIFY_BATCH_CHARS = 8 * 1024 * 1024;

  /** One load's files on their way to the screen. */
  interface Arrival {
    loaded: LoadedPatch;
    /** Whether the load's selection state has been applied. */
    applied: boolean;
  }

  // The load's selection state is applied with its first files, so the
  // selectors never describe a diff that is not on screen.
  function showFiles(arrival: Arrival, next: readonly ReviewFile[]): void {
    if (!arrival.applied) {
      arrival.applied = true;
      const { loaded } = arrival;
      commits = loaded.commits ?? [];
      edits = loaded.edits ?? [];
      editTurnLabels = loaded.editTurnLabels ?? new Map();
      if (loaded.selectedEdit !== undefined) selectedEdit = loaded.selectedEdit;
      if (loaded.selectedCommitSHA !== undefined) selectedCommitSHA = loaded.selectedCommitSHA;
      clearContextExpansions();
    }
    loadedFiles = sortFilesTreeOrder(next);
  }

  /**
   * Reads a loaded patch. A pane with no files shows the diff as it
   * arrives; a pane showing a diff keeps it until the caller commits the
   * complete one. Returns null when a newer load or dispose cancelled it.
   */
  async function readPatch(
    seq: number,
    arrival: Arrival,
  ): Promise<{ files: readonly ReviewFile[]; read: ReviewDiffRead | null; patchKey: string; headSha: string } | null> {
    const { patch, mergesPaths } = arrival.loaded;
    if (patch === null) return { files: [], read: null, patchKey: '', headSha: '' };
    const merge = mergesPaths ? new EditFileMerge() : null;
    const view = (sections: readonly ReviewFile[]): readonly ReviewFile[] => merge?.files(sections) ?? sections;
    const cancelled = (): boolean => seq !== loadSeq || disposed;
    const progressive = loadedFiles.length === 0;
    let sectionsShown = 0;
    let filesShown = 0;
    let publishedAt = 0;
    // Files arriving in an empty pane take their default collapse state
    // as they land; the complete load recomputes it for every file.
    const onProgress = (parser: PatchParser): void => {
      if (!progressive || parser.files.length === sectionsShown) return;
      const now = performance.now();
      if (sectionsShown > 0 && now - publishedAt < PROGRESS_PUBLISH_MS) return;
      if (sectionsShown === 0) collapsedPaths = new SvelteSet<string>();
      const files = view(parser.files);
      for (let index = filesShown; index < files.length; index += 1) {
        const file = files[index];
        const collapsed = collapseOverrides.get(file.path) ?? collapsedByDefault(file);
        if (collapsed) collapsedPaths.add(file.path);
      }
      sectionsShown = parser.files.length;
      filesShown = files.length;
      publishedAt = now;
      showFiles(arrival, files);
    };
    const read = await patch.read({
      cancelled,
      onProgress,
      onOpened: (opened) => {
        if (threadId !== null && opened.payloadIds?.length) {
          void seedEditPatchSpans(threadId, patch.backend, opened.payloadIds, cancelled);
        }
      },
    });
    if (!read) return null;
    if (merge) {
      try {
        // The last sections join the merge here; a path whose text was
        // evicted merges once it is read again.
        merge.files(read.parser.files);
        await merge.settle();
      } catch (err) {
        read.dispose();
        throw err;
      }
      if (cancelled()) {
        read.dispose();
        return null;
      }
    }
    return { files: view(read.parser.files), read, patchKey: read.parser.sourceKey(), headSha: read.headSha };
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
  function filePatchText(file: ReviewFile): string {
    return file.body.patchText();
  }

  // The historical patch text of one edits-scope file, for the
  // backend's has-the-file-drifted verification. Empty for unknown
  // paths (the backend then refuses, which is the safe direction).
  async function editVerifyPatch(path: string): Promise<string> {
    if (scope !== 'edits') return '';
    const file = files.find((candidate) => candidate.path === path);
    if (!file) return '';
    return file.body.whenResident(() => filePatchText(file));
  }

  // Load-time expandability pass for the edits scope: one batch RPC
  // proves which files an expansion click would actually serve, and
  // only those get gap arrows (editExpandablePaths gates the files
  // derived). Candidates come from the unsuppressed merge
  // (`loadedFiles`), NOT the files derived — that one already
  // suppresses everything still unverified. Any failure (a session
  // without `files:read` included) just leaves paths unverified: no
  // arrows, no error banner, exactly what clicking would have found
  // out the hard way.
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
    if (scope !== 'edits' || threadId === null) return;
    // Added and deleted files are whole: no gaps to gate, so no reason to
    // resolve them.
    const candidates = loadedFiles.filter(
      (file) => !file.suppressGaps && file.kind !== 'added' && file.kind !== 'deleted' && !file.path.startsWith('/'),
    );
    const context = patchScopeContext();
    // Batches in turn, so one request's patches stay far under a frame
    // and within the files the backend verifies per call.
    for (let start = 0; start < candidates.length;) {
      let end = start + 1;
      let chars = candidates[start].body.textLength;
      while (end < candidates.length && end - start < VERIFY_BATCH_FILES
        && chars + candidates[end].body.textLength <= VERIFY_BATCH_CHARS) {
        chars += candidates[end].body.textLength;
        end += 1;
      }
      const batch = candidates.slice(start, end);
      start = end;
      try {
        const files = await whenResident(
          batch.flatMap((file) => file.body.segments),
          () => batch.map((file) => ({ path: file.path, verifyPatch: filePatchText(file) })),
        );
        if (seq !== loadSeq || disposed) return;
        const result = await VerifyEditDiffs(threadId, {
          editPayloadId: context.editPayloadId ?? '',
          editTurnIndex: context.editTurnIndex ?? -1,
          files,
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
        verifyPatch: await editVerifyPatch(path),
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
      // Builds of the expansion run in a derived, so the headings they
      // keep are read here, where evicted text can be read again.
      const source = loadedFiles.find((file) => file.path === path);
      const headings = contextExpansions.get(path)?.headings
        ?? (source ? await readHunkHeadings(source.body) : undefined);
      if (seq !== loadSeq || disposed) return;
      const state = contextExpansions.get(path)
        ?? { lines: new Map<number, string>(), eofLine: null, version: 0 };
      state.headings ??= headings;
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
      const thread = commentThreadId();
      const sendScope = scope;
      const key = sourceKey;
      const sending = drafts;
      const shown = files;
      // Excerpts can read evicted text again, so everything the send
      // addresses is taken before.
      const pr = sendScope === 'pr' && detail
        ? {
            number: detail.number,
            url: detail.url,
            comments: await Promise.all(sending.map(async (comment) => ({
              commentId: comment.id,
              hunkExcerpt: await hunkExcerptForComment(shown, comment),
            }))),
          }
        : undefined;
      await SendDiffReviewComments(thread, sendScope, key, sending.map((comment) => comment.id), { pr });
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
      const result = (await withBackendTarget(backend, () => SubmitPRReview(prReferenceWire(prRef), {
        verdict,
        body: summaryBody.trim(),
        comments: submitDrafts.map(reviewLineCommentForDraft).filter((comment) => comment !== null),
      }))) as SubmitPRReviewResult;
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
      applyPRThreads(computerPRKey(prRef), ((await withBackendTarget(backend, () => ListPRReviewThreads(prReferenceWire(prRef)))) ?? []) as ReviewThread[]);
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
    const key = computerPRKey(prRef);
    refreshingPRData = true;
    try {
      const pr = prReferenceWire(prRef);
      const [detail, threads] = await Promise.all([
        withBackendTarget(backend, () => GetPRDetail(pr)) as Promise<PRDetail>,
        withBackendTarget(backend, () => ListPRReviewThreads(pr)) as Promise<ReviewThread[] | null>,
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
      await withBackendTarget(backend, () => ReplyToPRThread(prReferenceWire(prRef), thread.id, first.databaseID, body));
      replyBodies.delete(thread.id);
      applyPRThreads(computerPRKey(prRef), ((await withBackendTarget(backend, () => ListPRReviewThreads(prReferenceWire(prRef)))) ?? []) as ReviewThread[]);
    } catch (err) {
      const message = userFacingError(err);
      replyErrors.set(thread.id, message);
      error = message;
      throw err;
    } finally {
      sendingReplyIds.delete(thread.id);
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
      await withBackendTarget(backend, () => SetPRThreadResolved(prReferenceWire(prRef), thread.id, resolved));
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

  // Forget the frozen view: a fresh visit is a fresh view, so the order
  // and the reply-fold defaults recompute and the previous visit's manual
  // choices go. The section's open state is the reader's and stays.
  function resetConversationView(): void {
    conversationOrder = [];
    conversationDefaultExpanded = new Set<string>();
    conversationExpandOverrides.clear();
    pendingConversationThreadId = null;
    unresolvedCursor = null;
  }

  function setConversationOpen(open: boolean): void {
    if (open === conversationOpen) return;
    conversationOpen = open;
    if (open) captureConversationOrder(false);
    else resetConversationView();
  }

  function conversationThreadExpanded(prThreadId: string): boolean {
    return conversationExpandOverrides.get(prThreadId) ?? conversationDefaultExpanded.has(prThreadId);
  }

  function openConversationAt(prThreadId: string): void {
    setConversationOpen(true);
    // The target may still be behind the "N new" chip (it just arrived on
    // a poll), or the order may not be frozen yet; capture so the jump
    // has somewhere to land.
    if (!conversationOrder.includes(`t:${prThreadId}`)) captureConversationOrder(true);
    if (!conversationThreadExpanded(prThreadId)) conversationExpandOverrides.set(prThreadId, true);
    pendingConversationThreadId = prThreadId;
  }

  // The overview row carries the sections; leave any replacement view
  // first, the way every diff-surface jump does.
  function jumpToOverview(section?: ReviewSectionId): void {
    closeCILogView();
    closeConflictView();
    if (section === 'description') descriptionOpen = true;
    if (section === 'conversation') setConversationOpen(true);
    pendingJumpRowKey = REVIEW_OVERVIEW_ROW_KEY;
  }

  function jumpToConversationThread(prThreadId: string): void {
    openConversationAt(prThreadId);
    jumpToOverview('conversation');
  }

  function jumpToDiffThread(thread: ReviewThread): void {
    if (!thread.path) return;
    closeCILogView();
    closeConflictView();
    collapsedPaths.delete(thread.path);
    expandedPRThreadIds.add(thread.id);
    pendingJumpRowKey = `pt:${thread.id}`;
  }

  function stepUnresolvedThread(direction: 1 | -1, inOverview: boolean): void {
    const list = unresolvedThreads;
    if (list.length === 0) return;
    const current = list.findIndex((thread) => thread.id === unresolvedCursor);
    const index = current < 0
      ? (direction > 0 ? 0 : list.length - 1)
      : (current + direction + list.length) % list.length;
    const thread = list[index];
    unresolvedCursor = thread.id;
    const inDiff = thread.path !== '' && files.some((file) => file.path === thread.path);
    if (inOverview) {
      openConversationAt(thread.id);
      return;
    }
    if (!inDiff) {
      jumpToConversationThread(thread.id);
      return;
    }
    jumpToDiffThread(thread);
  }

  // The merged tree and every conflicted file's content belong to the PR
  // in this checkout (one merge-tree run serves every pane there); what
  // this pane owns is whether the surface is showing and which files it
  // has collapsed.
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
    const conflicts = peekPRConflicts(key, workspace);
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
    await ensurePRConflictFile(key, workspace, path);
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

  async function refreshCI(): Promise<void> {
    const key = prEntityKey;
    if (!key) return;
    await refreshPRCI(key);
  }

  function openCIJobLog(stageName: string, job: CIJob): void {
    const key = prEntityKey;
    const jobId = job.id;
    if (!key || !job.logsAvailable || !jobId) return;
    // The log view and the conflict view both replace the diff body.
    setConflictView(false);
    if (ciLogOpen && ciLogOpen.key !== key) closeCILogView();
    ciLogOpen = { key, stageName, job: { ...job, id: jobId } };
    ciLogLocalError = null;
    ciLogSavedPath = null;
    ciLogOpenSections.clear();
    setPRCILogFollow(key, ciFollowToken, jobId);
  }

  // Re-sending the follow is the backend's refetch.
  function refreshCILog(): void {
    if (!ciLogOpen) return;
    ciLogLocalError = null;
    refreshPRCILogFollows(ciLogOpen.key);
  }

  function closeCILogView(): void {
    if (ciLogOpen) setPRCILogFollow(ciLogOpen.key, ciFollowToken, null);
    ciLogOpen = null;
    ciLogLocalError = null;
    ciLogSavedPath = null;
    ciLogOpenSections.clear();
  }

  function toggleCILogSection(sectionKey: string): void {
    if (!ciLogOpen) return;
    const key = `${ciLogOpen.job.id}/${sectionKey}`;
    if (ciLogOpenSections.has(key)) ciLogOpenSections.delete(key);
    else ciLogOpenSections.add(key);
  }

  function setCILogSectionsOpen(sectionKeys: readonly string[], open: boolean): void {
    if (!ciLogOpen) return;
    for (const sectionKey of sectionKeys) {
      const key = `${ciLogOpen.job.id}/${sectionKey}`;
      if (open) ciLogOpenSections.add(key);
      else ciLogOpenSections.delete(key);
    }
  }

  async function saveCILog(): Promise<string | null> {
    // The open record, not the derived view: a pipeline frame landing
    // during the save re-derives the view but leaves the same log open.
    const open = ciLogOpen;
    if (!prRef || !open) return null;
    const jobId = open.job.id;
    const jobName = ciLogView?.job.name ?? open.job.name;
    const ref = prReferenceWire(prRef);
    try {
      const path = String(await withBackendTarget(backend, () => SavePRCIJobLog(ref, jobId, jobName)));
      if (ciLogOpen === open) ciLogSavedPath = path;
      return path;
    } catch (err) {
      if (ciLogOpen === open) ciLogLocalError = userFacingError(err);
      return null;
    }
  }

  async function sendCILogToChat(): Promise<void> {
    const open = ciLogOpen;
    const opened = ciLogView;
    if (!prRef || !open || !opened) return;
    const path = await saveCILog();
    if (!path) return;
    // The job's status as of the send, while its log is still the one open.
    const view = (ciLogOpen === open ? ciLogView : null) ?? opened;
    const draft = getComposerDraftForPane(sourcePaneId);
    if (!draft) {
      ciLogLocalError = 'The source chat pane is not available.';
      return;
    }
    const message = [
      `Investigate CI job \`${view.job.name}\` (${view.stageName}) on PR #${prRef.number}, status: ${view.job.status}.`,
      `Full log saved at: ${path}`,
    ].join('\n');
    const existing = draft.content.trim();
    draft.setContent(existing ? `${existing}\n\n${message}` : message);
  }

  async function sendCILogSectionToChat(section: CILogSectionSend): Promise<void> {
    const open = ciLogOpen;
    const view = ciLogView;
    const ref = prRef;
    if (!ref || !open || !view) return;
    if (!getComposerDraftForPane(sourcePaneId)) {
      ciLogLocalError = 'The source chat pane is not available.';
      return;
    }
    const result = CI_RESULT_STATUSES.has(section.status) ? ` (${section.status})` : '';
    const lines = [
      `Investigate \`${section.name}\`${result} in CI job \`${view.job.name}\` (${view.stageName}) on PR #${ref.number}, status: ${view.job.status}.`,
    ];
    if (section.truncatedTop) {
      lines.push('The text starts partway through this section: the log view holds the end of the job log.');
    }
    if (new TextEncoder().encode(section.text).length <= CI_SECTION_INLINE_MAX_BYTES) {
      const fence = codeFence(section.text);
      lines.push(fence, section.text, fence);
    } else {
      let path: string;
      try {
        path = String(await withBackendTarget(backend, () =>
          SavePRCIJobLogSection(prReferenceWire(ref), open.job.id, view.job.name, section.name, section.text)));
      } catch (err) {
        if (ciLogOpen === open) ciLogLocalError = userFacingError(err);
        return;
      }
      lines.push(`Section log saved at: ${path}`);
    }
    // The draft as of now: the save may have outlived the pane.
    const draft = getComposerDraftForPane(sourcePaneId);
    if (!draft) {
      if (ciLogOpen === open) ciLogLocalError = 'The source chat pane is not available.';
      return;
    }
    const message = lines.join('\n');
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
    rowId,
    get scope() { return scope; },
    backend,
    get baseBranch() { return baseBranch; },
    get prRef() { return prRef; },
    get prScopeLabel() { return prRef ? prScopeLabel(prRef) : null; },
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
    get prUpdateFailure() { return prUpdateFailure; },
    get spanContext(): PatchScopeContext {
      return patchScopeContext();
    },
    get prStale() { return prStale; },
    get awaitingPR() { return awaitingPR; },
    get refreshingPRData() { return refreshingPRData; },
    get conflictView() { return conflictView; },
    get conflicts() { return conflictsState.state; },
    get conflictsLoading() { return conflictsState.loading; },
    get conflictsError() { return conflictsState.error; },
    get conflictContentByPath() { return conflictsState.contentByPath; },
    get conflictCollapsedPaths() { return conflictCollapsedPaths; },
    get conflictFiles() { return conflictFiles; },
    paintedSpans,
    conflictPaintedSpans,
    get ciPipeline() { return ciState.pipeline; },
    get ciLoading() { return ciState.loading; },
    get ciRefreshing() { return ciState.refreshing; },
    get ciError() { return ciState.error; },
    get ciFailure() { return ciState.failure; },
    get ciLogView() { return ciLogView; },
    get ciLog() { return ciLog; },
    get ciLogLoading() { return ciLogOpen !== null && prCILogFollowPending(ciLogOpen.key); },
    get ciLogError() { return ciLogLocalError ?? ciLog?.error ?? null; },
    get ciLogFailure() { return ciLogLocalError === null ? (ciLog?.failure ?? null) : null; },
    get ciLogAvailable() { return ciLog?.available ?? true; },
    get ciLogSavedPath() { return ciLogSavedPath; },
    get ciLogOpenSections() { return ciLogOpenSections; },
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
    jumpToDiffThread,
    get descriptionOpen() { return descriptionOpen; },
    setDescriptionOpen(open: boolean): void {
      descriptionOpen = open;
    },
    overviewSectionScrollTop(section: ReviewSectionId): number {
      return overviewScrollTops.get(section) ?? 0;
    },
    setOverviewSectionScrollTop(section: ReviewSectionId, px: number): void {
      overviewScrollTops.set(section, px);
    },
    jumpToOverview,
    jumpToConversationThread,
    get unresolvedThreads() { return unresolvedThreads; },
    get unresolvedCursor() { return unresolvedCursor; },
    stepUnresolvedThread,
    setConversationOpen,
    get conversationFrozen() { return conversationOrder.length > 0; },
    freezeConversation(): void {
      if (conversationOrder.length === 0) captureConversationOrder(false);
    },
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
    refreshCI,
    openCIJobLog,
    closeCILogView,
    refreshCILog,
    saveCILog,
    sendCILogToChat,
    toggleCILogSection,
    setCILogSectionsOpen,
    sendCILogSectionToChat,
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

function findCIJob(
  pipeline: CIPipeline | null,
  jobId: string,
): { stageName: string; job: CIJob } | null {
  for (const stage of pipeline?.stages ?? []) {
    const job = stage.jobs.find((candidate) => candidate.id === jobId);
    if (job) return { stageName: stage.name, job };
  }
  return null;
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
