import { threadMachine } from './attachedBackends.svelte';
import { companionSubjectKey } from './companionSubject';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { threadHasScope } from '../transport/entityScopes';
import { hasScope } from '../transport/scopes';
import {
  createPRDiffReviewComment,
  deletePRDiffReviewComment,
  getPRDiffReviewComments,
  markPRDiffReviewCommentsSent,
  prDraftsKey,
  refreshPRDiffReviewComments,
  resetPRDiffReviewCommentsForTest,
  sendPRDiffReviewComments,
  updatePRDiffReviewComment,
  type PRDraftsTarget,
} from './prDiffReviewComments.svelte';
import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import { isPaneOnScreen } from './layoutMode.svelte';
import { untrack } from 'svelte';
import {
  ApprovePR,
  CancelPRAutoMerge,
  GetDiffContextLines,
  GetEditDiffContextLines,
  GetPRDetail,
  MarkPRReady,
  MergePR,
  ListPRReviewThreads,
  OpenPRDiff,
  MarkDiffReviewCommentsSent,
  SetPRThreadResolved,
  SendDiffReviewComments,
  SubmitPRReview,
  GetPRReviewSubmitOutcome,
  UnapprovePR,
  UpdatePRBranch,
  VerifyEditDiffs,
} from './bindings';
import { addCompanionClosedObserver, openCompanion } from './companionPanes.svelte';
import { addThreadRemovedObserver } from './threads.svelte';
import { peekGitStatus, peekGitStatusError } from './gitStatusStore.svelte';
import { getTransportHelloFor, isTransportClassError } from './transportStatus.svelte';
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

  peekPRSeq,
  peekPRSnapshot,
  setPRThreadResolveOverride,
  type PRAttachment,
  type PRSnapshot,
} from './prReviewStore.svelte';
import type { PRCILogState } from './prReviewCI.svelte';
import { CI_SECTION_INLINE_MAX_BYTES, createCISubjectState, type CILogView } from './ciSubject.svelte';
import type { CIActionsView } from './ciSubjectActions.svelte';
import { addErrorToast, addToast } from './toast.svelte';
import { createPRManageState, type PRManageView } from './prManage.svelte';
import { TransportError } from '../transport/wsClient';
import type { ForgeFailure } from '../utils/forgeFailure';
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
import { ReviewDiffSource, type ReviewDiffRead } from './reviewDiffStream';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { persistScope, readPersistedScope } from './reviewPaneScope';
import { getSettings } from './settings.svelte';
import { getActiveTurn } from './threadStatuses.svelte';
import type { BranchCommit, WorkspaceRef } from '../types/git';
import type {
  CIJob,
  CILogLines,
  CILogSearch,
  CILogSection,
  CIPipeline,
  DiffReviewComment,
  DiffReviewCommentInput,
  DiffReviewScope,
  MergeMessage,
  MergeOutcome,
  PRDetail,
  ReviewEvent,
  ReviewThread,
  ReviewVerdict,
  SubmitPRReviewResult,
  PRReviewSubmitOutcome,
  Thread,
} from '../types/models';
import { randomId } from '../utils/randomId';
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
import { prRevision, type PRRevision } from '../utils/prRevision';
import {
  applyContextExpansion,
  expansionFetchRange,
  nextExpansionVersion,
  readHunkHeadings,
  type ContextExpansionState,
  type ExpandDirection,
} from '../utils/diffContextExpansion';
import type { DiffGap } from '../utils/patchFiles';
import { createReviewDiffMedia, type ReviewMediaHost } from './reviewDiffMedia.svelte';
import { DiffFilesHandle, type DiffFilesTarget } from './reviewDiffFiles';
import { keptText, PatchTextLost, reviewFileFromPatchFile, whenResident, type PatchParser, type ReviewFile } from '../utils/patchStore';
import { anchorRow, anchorRows, completeAnchor, namesOneRow } from '../utils/patchRows';
import { anchorKey, type CommentAnchor, type EditorRow, type EditorState } from '../utils/reviewRows';
import {
  EDITOR_QUOTE_MAX_BYTES,
  lineGuard,
  RELOCATION_BUDGET_BYTES,
  RELOCATION_BUDGET_ROWS,
  RelocationBudget,
  RelocationCancelled,
  relocateEditors,
  type DetachReason,
  type EditorProof,
  type LineGuard,
  type RelocationOutcome,
  type RelocationTarget,
} from '../utils/editorPlacement';
import { sortFilesTreeOrder } from '../utils/reviewTree';
import type { ReviewSectionId } from './reviewSectionSizes.svelte';
import { REVIEW_COMMIT_CARD_ROW_KEY, REVIEW_OVERVIEW_ROW_KEY } from '../utils/reviewRows';
import { createReviewCommentActions, type ReviewCommentActionsState } from './reviewCommentActions.svelte';
import { createReviewCommentExtras, type ReviewCommentExtrasState } from './reviewCommentExtras.svelte';
import { EDITOR_PREVIEW_KEY } from './reviewEditorPreview.svelte';
import { commentWriteKey } from './reviewCommentWrites.svelte';
import { RANGE_REFUSAL_TEXT, rangesByFile, spanRange, type LineRange } from '../utils/reviewRange';
import { commitSelectionFromHere, isCommitRange, resolveCommitSelection, sameCommitSelection, type CommitSelection } from '../utils/commitRange';
import { createReviewPRLineIndex, type PRLineIndexState } from './reviewPRLineIndex.svelte';
import { buildPRLineIndex, type PRLineIndex } from '../utils/prLineIndex';
import { lineCommentReason, lineCommentTarget, rangePROrphans, rangeSaveAnchor, rangeShownDrafts, type LineCommentTarget } from './reviewRangeComments';

export type ReviewScope = DiffReviewScope;

/** One row of the Conversation section's chronological feed: a review
 * thread's card, a verdict one-liner, a forge event one-liner (an
 * approval, a merge, a draft flip), or one contiguous run of pushed
 * commits by one author. Ids are prefixed per kind (`t:`/`v:`/`e:`/`c:`)
 * so the frozen-order capture can hold all four still at once. */
export type ConversationFeedItem =
  | { kind: 'thread'; id: string; thread: ReviewThread }
  | { kind: 'verdict'; id: string; verdict: ReviewVerdict }
  | { kind: 'event'; id: string; event: ReviewEvent }
  | { kind: 'commits'; id: string; author: string; commits: readonly BranchCommit[] };

/** A Merge section action in flight. */
export type MergeActionKind = 'approve' | 'unapprove' | 'merge' | 'auto_merge' | 'cancel_auto_merge' | 'update_branch' | 'mark_ready';

/** What the Merge menu sends: the method and toggles it shows. */
export interface MergePRChoice {
  method: string;
  deleteBranch: boolean;
  autoMerge: boolean;
  /** The edited commit message (prMergeMessage.svelte.ts); omitted, the forge's default. */
  commitMessage?: MergeMessage;
}

export interface ReviewPaneState extends ReviewCommentActionsState, ReviewCommentExtrasState {
  /** Subject this state was created for — the registry's staleness check.
   *  Conversation/draft identity, ownership epoch and checkout. */
  readonly identity: string;
  /** Span-cache owner for this review's diff bodies (`ReviewSubject.rowId`). */
  readonly rowId: string;
  readonly scope: ReviewScope;
  /** Pinned to one PR with no thread (`reviewSubjectForPR`): the scope
   * stays `pr`, there is no chat to send to, and drafts post as a PR
   * review. */
  readonly pinned: boolean;
  /** The session holds the grant the comment writes need on this
   * review's computer: `threads:operate` for a thread's drafts,
   * `git:operate` for a pinned review's forge submission. */
  readonly commentsGranted: boolean;
  /** Line comments have a home here: the thread row, or for a pinned
   * review the PR's full diff (a picked commit's line numbers would
   * misanchor against the head diff, and there is no agent to send them to). */
  readonly canDraftComments: boolean;
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
  /** The open new-comment editors of the diff on screen. */
  readonly openEditors: readonly ReviewEditor[];
  /** Commits the branch/PR carries (newest first); empty outside those
   * scopes and for non-git workspaces. Feeds the commit selector. */
  readonly commits: readonly BranchCommit[];
  /** How many commits the range holds: more than `commits` when the
   * backend listed only the newest, null when it could not count them
   * (utils/commitRange.ts). */
  readonly commitsTotal: number | null;
  /** Selected single commit, or null for the full-range diff. In pr
   * scope with a run of commits picked, the run's newer end. */
  readonly selectedCommitSHA: string | null;
  /** pr scope: the picked commit or run (`first` older), or null. */
  readonly selectedCommits: CommitSelection | null;
  /** A run of two or more commits ending at the PR's head is on screen:
   * comments here are the PR's (stores/reviewRangeComments.ts). */
  readonly headRange: boolean;
  /** A held commit or run named commits the PR no longer lists, so the
   * pane fell back to All commits; cleared by the next pick. */
  readonly commitSelectionLost: boolean;
  dismissCommitSelectionLost(): void;
  /** The whole PR's diff index a head-ending run reads its PR lines from. */
  readonly prDiffIndexState: PRLineIndexState;
  readonly prDiffIndexError: string | null;
  retryPRDiffIndex(): void;
  /** In a head-ending run, what a line takes and why (hover text); null
   * in every other view, where every line takes the view's comments. */
  lineComment(anchor: Pick<CommentAnchor, 'filePath' | 'newLine' | 'side'>): { target: LineCommentTarget; reason: string | null } | null;
  /** The drafts the diff shows: `drafts`, or in a head-ending run the
   * PR's drafts placed by head line. */
  readonly shownDrafts: readonly DiffReviewComment[];
  /** The diff on screen's binary files (sizes, images) and the text SVGs
   * shown rendered. */
  readonly mediaHost: ReviewMediaHost;
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
  readonly pendingJumpFilePath: string | null;
  /** Paths an explicit jump revealed: the file filter keeps them despite
   * the type filter and hide-tests (`filterReviewFiles`). Emptied when the
   * filter selection changes (`clearRevealedPaths`) and when the diff is
   * replaced by a different one. */
  readonly revealedPaths: ReadonlySet<string>;
  /** Jump to a file's header, revealing it if the filter hides it. */
  requestFileJump(path: string): void;
  clearRevealedPaths(): void;
  /** Diff row key to jump to (a card's file:line, the unresolved
   * stepper, the title bar's peek controls); consumed by the diff body. */
  readonly pendingJumpRowKey: string | null;
  readonly loading: boolean;
  readonly error: string | null;
  readonly sendingComments: boolean;
  /** Whether the send strip shows: a draft, a summary or verdict kept for
   * the next submit, or a submit in flight or unresolved. */
  readonly sendStripShown: boolean;
  /** Live PR detail, shared by every pane on this PR. */
  readonly prDetail: PRDetail | null;
  readonly prThreads: readonly ReviewThread[];
  /** The forge's activity rows for the PR (approvals, merges, …). */
  readonly prEvents: readonly ReviewEvent[];
  // ------------------------------------------------------------------
  // The Merge section: readiness and the merge actions, the one home for
  // approve, merge and auto-merge on every surface (docs/specs/pull-requests.md).
  // No optimistic state: a success asks the pump to poll and the pane
  // follows the forge's answer; the action stays pending until that
  // answer is on screen: the action answers with the pump's seq after its
  // poll, and the pane waits for its watermark to reach it.
  // ------------------------------------------------------------------
  readonly mergeOpen: boolean;
  setMergeOpen(open: boolean): void;
  /** The action in flight, or null. Every merge control disables on it. */
  readonly mergeAction: MergeActionKind | null;
  /** Why the merge actions are withheld (a stale diff, a failing pump),
   * or null when they may run. */
  readonly mergeWithheldReason: string | null;
  approvePR(): Promise<void>;
  unapprovePR(): Promise<void>;
  mergePR(choice: MergePRChoice): Promise<void>;
  cancelPRAutoMerge(): Promise<void>;
  updatePRBranch(): Promise<void>;
  markPRReady(): Promise<void>;
  /** Managing the PR: its editors, Details pickers, state changes and
   * reviews, under the merge actions' contract (prManage.svelte.ts). */
  readonly manage: PRManageView;
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
  /** The hidden lines of each collapsed fold of a conflict file, by fold
   * id, as `conflictFiles` renders it now; null with no content. */
  conflictFoldLines(path: string): ReadonlyMap<number, readonly string[]> | null;
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
  readonly ciLogSavedPath: string | null;
  /** The open log followed a retry of the job it was opened on. */
  readonly ciLogRetriedFrom: string | null;
  /** The PR's CI actions (ciSubjectActions.svelte.ts). */
  readonly ciActions: CIActionsView;
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
  setScope(scope: ReviewScope, opts?: { baseBranch?: string } & ReadIntent): Promise<void>;
  /** Switch the loaded diff to a single commit (null → full range). */
  selectCommit(sha: string | null, opts?: ReadIntent): Promise<void>;
  /** Switch to a commit or, in pr scope, a run of commits (null → the
   * whole diff). */
  selectCommits(selection: CommitSelection | null, opts?: ReadIntent): Promise<void>;
  /** The Conversation's `Changes from here`: the commit through the
   * newest, then the diff's top. */
  showChangesFrom(sha: string): Promise<void>;
  /** Switch the edits-scope diff (selector value encoding: `item:<id>`
   * or `turn:<n>`; null → default, the latest turn). */
  selectEdit(key: string | null, opts?: ReadIntent): Promise<void>;
  reload(opts?: ReadIntent): Promise<void>;
  consumePendingJumpFilePath(): void;
  consumePendingJumpRowKey(): void;
  /** Opens an editor on a line of the diff on screen, or focuses the one
   * already open on it; with an editor armed (moveEditor), moves that one
   * to the line instead. Returns its id; null when the diff on screen is
   * not the subject the controls name, has no such line, or is not the
   * one `generation` names (the diff the reader pressed in). `anchor`'s
   * `lineIndex` names the row among several with its coordinate. */
  openDraftEditor(anchor: EditorAnchor, generation?: number): string | null;
  /** Discards an open editor and its text; not while its comment is being
   * posted, so a failure has its editor and text to show in. */
  closeDraftEditor(editorId: string): void;
  /** Places a detached editor on its suggested row, as if it were opened
   * there now. False when that row is not on screen, or is not the one
   * `seen` names: the suggestion the reader saw, in the diff it was in. */
  useSuggestedLine(editorId: string, seen: Pick<EditorRow, 'generation' | 'suggested'>): boolean;
  /** Arms a detached editor, or one on a line number several rows share:
   * the next line openDraftEditor names takes it and its text. Arming one
   * disarms any other. */
  moveEditor(editorId: string): void;
  cancelEditorMove(): void;
  /** The editor a picked line will take, if any. */
  readonly armedEditorId: string | null;
  /** The diff on screen's generation (ShownDiff.generation); 0 before one. */
  readonly shownGeneration: number;
  /** Store-backed editor text — survives virtualizer row unmounts. */
  draftBodyFor(editorId: string): string;
  setDraftBody(editorId: string, body: string): void;
  /** True exactly once after openDraftEditor, for the mount autofocus. */
  consumeDraftEditorFocus(editorId: string): boolean;
  /** Saves an open editor's comment as a draft, or with `now` posts it to
   * the PR at once (the editor stays open, pending, until the forge's
   * answer is on screen). Rejects with ReviewCommentRefused when nothing
   * proves its line is the one it was opened on. */
  createComment(editorId: string, body: string, mode?: 'draft' | 'now'): Promise<void>;
  /** Shift+click on a line: spans the last-opened editor of the same file
   * from its line to `anchor`. Opens an editor on `anchor` when none is. */
  extendEditorRange(anchor: EditorAnchor, generation?: number): string | null;
  /** Drops a range editor back to the line it was opened on. */
  dropEditorRange(editorId: string): void;
  /** The ranges the diff tints in a file: open range editors, range
   * drafts, and the expanded forge threads that span lines. */
  rangeTintsFor(path: string): readonly LineRange[];
  /** The unsaved text of a draft open in its editor, or null when it is
   * not being edited; store-backed like `draftBodyFor`. */
  draftEditBodyFor(commentId: string): string | null;
  /** Opens (a body), changes or, with null, discards a draft's edit. */
  setDraftEditBody(commentId: string, body: string | null): void;
  /** Saves a draft's body; a success ends its edit. */
  updateComment(commentId: string, body: string): Promise<void>;
  deleteComment(commentId: string): Promise<void>;
  sendComments(): Promise<void>;
  /** A pinned review's drafts on the whole PR, handed to a thread as a
   * review message (SendPRDiffReviewComments). Answers the thread row. */
  sendDraftsToThread(threadId: string): Promise<Thread | null>;
  submitPRReview(): Promise<void>;
  setSubmitTarget(target: 'agent' | 'pr'): void;
  setVerdict(verdict: 'comment' | 'approve' | 'request-changes'): void;
  setSummaryBody(body: string): void;
  orphanedDraftIds(): SvelteSet<string>;
  togglePRThread(threadId: string): void;
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
  /** Conversation entries (threads, verdicts, events; pushes are context,
   * not conversation): the count the section header and the title bar show. */
  readonly conversationCount: number;
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
  /** A pane rendering this state is mounted in layout pane `paneId`;
   * returns the detach. The conflict view's reconcile permit is held only
   * while such a pane is on screen, so a state off screen reads no merges. */
  attachSurface(paneId: string): () => void;
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
  /** Reads a window of the open job's cached log, for its view. */
  readCILogLines(from: number, count: number): Promise<CILogLines>;
  /** Finds `query` in the open job's cached log, for the pane's find. */
  searchCILog(query: string, limit: number): Promise<CILogSearch>;
  /** Copies one section of the open log: its text up to
   * CI_SECTION_INLINE_MAX_BYTES, otherwise a file it is saved to, whose
   * path is copied. Resolves true when something was copied. */
  copyCILogSection(section: CILogSection): Promise<boolean>;
  /** Puts one section of the open log in the source chat's composer:
   * inline up to CI_SECTION_INLINE_MAX_BYTES, otherwise saved to a file
   * the message names. */
  sendCILogSectionToChat(section: CILogSection): Promise<void>;
  /** Fetches hidden hunk-gap context and merges it into the diff. */
  expandDiffContext(path: string, gap: DiffGap, dir: ExpandDirection): Promise<void>;
  toggleCollapsed(path: string): void;
  toggleCollapseAll(): Promise<void>;
  setViewMode(mode: 'stacked' | 'split'): void;
  setWordWrap(wrap: boolean): void;
  /** Flips `-w` and re-requests the diff (the patch itself changes, so
   * this is a full reload, not a view toggle). */
  setIgnoreWhitespace(ignore: boolean, opts?: ReadIntent): Promise<void>;
  dispose(): void;
}

/**
 * Whether a person asked for a load: a pr-scope read that may fetch from
 * origin may ask for git credentials, and its failure arms the PR pump's
 * retry, only then. Every entry point defaults to unattended, so a load
 * nobody asked for cannot prompt by omission; a user action says so.
 */
export interface ReadIntent {
  userInitiated?: boolean;
}

const CI_RESULT_STATUSES = new Set(['success', 'failed', 'running', 'pending', 'canceled', 'skipped', 'manual']);

/** A fence longer than any backtick run in `text`. */
function codeFence(text: string): string {
  const longest = (text.match(/`+/g) ?? []).reduce((max, run) => Math.max(max, run.length), 0);
  return '`'.repeat(Math.max(3, longest + 1));
}

const statesBySourcePane = new Map<string, ReviewPaneState>();
// The PR a pane in pr scope holds after the forge stops listing it, by
// thread row. A merge from the companion pane moves the thread to Base
// (the worktree cleanup), which replaces this state with one for the new
// checkout, and a thread switch hides the companion and reopens it later;
// the held PR outlives both so the pane stays on the merged PR's snapshot
// until closed (docs/specs/pull-requests.md#actions). It goes when the
// pane leaves pr scope, the review companion closes for good, or the
// thread is removed.
const heldPRByRow = new Map<string, PRRef>();
// Registered on the first state, not at module load: this module sits in
// an import cycle with the companion and thread stores, so at load one of
// them can still be initializing.
let heldPRObserversInstalled = false;
function installHeldPRObservers(): void {
  if (heldPRObserversInstalled) return;
  heldPRObserversInstalled = true;
  addCompanionClosedObserver((sourcePaneId, kind, threadId) => {
    if (kind !== 'review') return;
    const row = threadId ?? statesBySourcePane.get(sourcePaneId)?.rowId;
    if (row) heldPRByRow.delete(row);
  });
  // A deleted or archived thread takes its hidden companions with it
  // (companionStash), with no live companion left to close.
  addThreadRemovedObserver((threadId) => heldPRByRow.delete(threadId));
}

// Stable identity for "this pane is on no PR": ReviewDiffBody re-anchors
// the reader on a prThreads IDENTITY change, so a fresh [] per read would
// look like the review threads moved on every keystroke.
const EMPTY_PR_THREADS: readonly ReviewThread[] = Object.freeze([]);
const EMPTY_PR_EVENTS: readonly ReviewEvent[] = Object.freeze([]);
// The verb a failed action's toast leads with.
const MERGE_ACTION_LABEL: Record<MergeActionKind, string> = {
  approve: 'Approve',
  unapprove: 'Unapprove',
  merge: 'Merge',
  auto_merge: 'Auto-merge',
  cancel_auto_merge: 'Cancel auto-merge',
  update_branch: 'Update branch',
  mark_ready: 'Mark ready',
};
const EMPTY_CONVERSATION_FEED: readonly ConversationFeedItem[] = Object.freeze([]);

// Same stability rule for the comment list a workspace-only pane reads.
const EMPTY_COMMENTS: readonly DiffReviewComment[] = Object.freeze([]);
const NO_RANGES: readonly LineRange[] = Object.freeze([]);
// What a chat-bound call on a pinned review answers; the pane offers none.
const PINNED_NO_CHAT = 'This review has no chat to send to.';

/**
 * What a review pane is looking at. The values travel together because
 * they answer different questions and only agree by accident:
 * `identity` keys the registry (a draft placeholder has one without a row),
 * `threadId` is the REAL row and is null until the draft materializes,
 * `rowId` is the pane's thread row id (a draft placeholder's synthetic one)
 * and owns the diff span-cache entries, which thread switch, delete and
 * draft materialization address by row id, and `workspace` is the checkout
 * every workspace-scoped RPC addresses. `pinned` is set only by
 * `reviewSubjectForPR`: the review is of that PR alone, with no thread.
 */
export interface ReviewSubject {
  readonly identity: string;
  readonly threadId: string | null;
  readonly rowId: string;
  readonly workspace: WorkspaceRef;
  readonly pinned?: PinnedReviewPR;
}

/** The PR a review with no thread is pinned to, and the computer that
 * listed it: every PR and workspace call of the review routes there. */
export interface PinnedReviewPR {
  readonly pr: PRRef;
  readonly backend: BackendKey;
}

/** An open new-comment editor of the review (utils/editorPlacement.ts). */
export type ReviewEditor = EditorRow;

/** A line a gesture names: its anchor and, for a row whose coordinate
 * other rows share, the row's line in the file's body. */
export type EditorAnchor = CommentAnchor & { lineIndex?: number };

/** Why createComment did not save: the editor belongs to another diff,
 * has no proven line, was proven against a diff no longer on screen, is
 * on a line number several rows of its file share, or quotes a line
 * longer than a comment may. */
export class ReviewCommentRefused extends Error {
  constructor(readonly reason: 'subject' | 'detached' | 'stale' | 'repeated' | 'too-long', message: string) {
    super(message);
    this.name = 'ReviewCommentRefused';
  }
}

const REVIEW_EDITOR_DETACHED = 'The file changed under this comment.';
const REVIEW_EDITOR_GONE = 'This file is no longer in the diff.';
const REVIEW_EDITOR_OTHER_DIFF = 'This comment was opened on another diff.';
const REVIEW_EDITOR_CLOSED = 'This comment editor is closed.';
const REVIEW_QUOTE_TOO_LONG = 'This line is too long to comment on.';
export const REVIEW_EDITOR_REPEATED = 'This line appears more than once in this view, so a comment cannot be attached to it.';

/** An open editor, replaced (never changed in place) on every change, so
 * work across an await can tell whether it still stands. */
interface EditorRecord {
  /** Minted when it opens: keys its text, focus and row. */
  readonly id: string;
  /** The subject of the diff it was opened on (reviewSubjectKey). */
  readonly subject: string;
  readonly filePath: string;
  readonly side: CommentAnchor['side'];
  readonly state: EditorState;
  /** Placed: the line it saves on. */
  readonly anchor: CommentAnchor | null;
  /** Placed: which of the rows its coordinate names it is on, and
   * whether there are several (it cannot be saved then). */
  readonly occurrence: number;
  readonly repeated: boolean;
  /** Detached: the row it shows after; proves nothing. */
  readonly suggested: CommentAnchor | null;
  /** Its last line number on its side, for the suggested row. */
  readonly line: number;
  /** Why it is detached or gone. */
  readonly reason: DetachReason | null;
  /** The line's text, saved as the comment's quote; empty when it is
   * longer than a comment may quote (quoteTooLong). */
  readonly selectedText: string;
  readonly quoteTooLong: boolean;
  /** Null for a file comment. */
  readonly guard: LineGuard | null;
  /** Detached: what last proved its line. Placed: derived when needed. */
  readonly proof: EditorProof;
  /** Placed: the shown file and the loaded file its line was found in. */
  readonly shownFile: ReviewFile | null;
  readonly baseFile: ReviewFile | null;
  /** Placed on a range: its first line (`anchor` is its last), the line
   * the editor was opened on, and the span the diff tints. */
  readonly rangeStart: CommentAnchor | null;
  readonly rangeOrigin: CommentAnchor | null;
  readonly range: LineRange | null;
  /** Why the last shift+click made no range. */
  readonly rangeRefusal: string | null;
}

const NO_RANGE = { rangeStart: null, rangeOrigin: null, range: null, rangeRefusal: null } as const;

function anchorSideLine(anchor: CommentAnchor): number {
  return (anchor.side === 'old' ? anchor.oldLine : anchor.newLine) ?? 0;
}

/** What a diff is about: everything that sends a comment somewhere else.
 * The workspace and thread are the review state's own. */
function reviewSubjectKey(parts: { scope: ReviewScope; baseBranch: string | null; commit: string | null; edit: string | null; pr: string }): string {
  return [parts.scope, parts.baseBranch ?? '', parts.commit ?? '', parts.edit ?? '', parts.pr].join('\u0000');
}

/** The subject segment of a commit pick: the commit, or a run's ends. */
function commitSubject(selection: CommitSelection | null): string | null {
  if (!selection) return null;
  return selection.first === selection.last ? selection.last : `${selection.first}..${selection.last}`;
}

/** The diff on screen, committed with its files: comments open and save
 * against it, never against the controls' subject while another loads. */
interface ShownDiff {
  /** Counts commits: an action is valid only in the diff it was taken in. */
  readonly generation: number;
  readonly subject: string;
  readonly scope: ReviewScope;
  readonly sourceKey: string;
  readonly commitSha: string | undefined;
  /** The `-w` it was read with. */
  readonly ignoreWhitespace: boolean;
  /** A run of commits ending at the PR's head (its comments are the PR's). */
  readonly headRange: boolean;
}

function utf8Bytes(text: string): number {
  if (text.length > EDITOR_QUOTE_MAX_BYTES) return text.length;
  return new TextEncoder().encode(text).length;
}

/** Relocation's allowance; tests narrow it. */
const relocationBudget = { rows: RELOCATION_BUDGET_ROWS, bytes: RELOCATION_BUDGET_BYTES };
export function __setRelocationBudgetForTest(rows: number, bytes = RELOCATION_BUDGET_BYTES): () => void {
  const before = { ...relocationBudget };
  relocationBudget.rows = rows;
  relocationBudget.bytes = bytes;
  return () => Object.assign(relocationBudget, before);
}

// The backend's SubmitPRReview posts named drafts and marks them itself
// (internal/app/app_pr_review_submit.go).
const REVIEW_DRAFT_SUBMIT = 'pr-review-drafts.v1';
function reviewDraftSubmit(backend: BackendKey): boolean {
  return getTransportHelloFor(backend)?.capabilities.includes(REVIEW_DRAFT_SUBMIT) ?? false;
}
// One press of Send to a PR review, from its call until what it did is
// known: its answer, or after a lost answer the backend's record of it
// (GetPRReviewSubmitOutcome). It carries everything it sent and where its
// drafts live, so it settles the same whatever the pane shows by then.
interface SubmitAttempt {
  /** Fresh per press; empty on a backend without the draft submit. */
  id: string;
  /** The backend process the submit went to (its hello's launchId). */
  launchId: string | null;
  ref: PRRef;
  scope: ReviewScope;
  source: string;
  /** The thread whose drafts it sent; null for the PR's own drafts. */
  threadId: string | null;
  ids: readonly string[];
  body: string;
  verdict: 'comment' | 'approve' | 'request-changes';
}

/** What an attempt did, as the backend answered or recorded it. Only a
 *  proof says not-posted; without one it is unconfirmed: the review may
 *  be on the PR. */
type SubmitSettlement =
  | { kind: 'posted'; result: SubmitPRReviewResult }
  | { kind: 'not-posted'; message: string }
  | { kind: 'unconfirmed'; message: string };

// The sentence for a submit the backend answered with an error: whatever
// the code, the review was not posted.
function notPostedMessage(code: string | undefined, message: string): string {
  if (code === 'review_drafts_not_drafts') return 'Some of these comments were already sent, so this review and its summary were not posted.';
  if (code === 'review_drafts_unmarked') return 'These comments were posted by an earlier review and are being marked sent, so this review was not posted.';
  return message;
}

const REVIEW_NEVER_ARRIVED = 'The connection dropped before the review reached the computer, so nothing was posted.';
const REVIEW_ANSWER_LOST = 'The connection dropped before the forge answered, so the review may already be on the PR. Check the PR before you submit again.';
const REVIEW_UNCONFIRMED = 'The forge did not confirm the review, so it may already be on the PR. Check the PR before you submit again.';
const REVIEW_OUTCOME_FORGOTTEN = 'The computer no longer has this review\'s outcome, so it may already be on the PR. Check the PR before you submit again.';

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
  opts: { deferInitialLoad?: boolean } & ReadIntent = {},
): ReviewPaneState {
  // A pinned review belongs to its page's retention, not a companion slot.
  if (subject.pinned) throw new Error('A pull request review is held by reviewStateForPR.');
  const existing = statesBySourcePane.get(sourcePaneId);
  // Subject mismatch replaces rather than reuses: the CompanionPane {#key}
  // remount usually disposes the old state first, but correctness must not
  // depend on Svelte's destroy-before-create ordering.
  if (existing && existing.identity === subject.identity) return existing;
  // The replaced state may own a live PR-update subscription; drop it or
  // the Go-side poll pump outlives the state that could unsubscribe it.
  existing?.dispose();
  const state = createReviewPaneState(sourcePaneId, subject, opts.deferInitialLoad ?? false, opts.userInitiated ?? false);
  statesBySourcePane.set(sourcePaneId, state);
  return state;
}

/**
 * Dispose `state` if it is still the pane's registered state. A state that
 * was already replaced was disposed by `reviewStateForPane`, and its
 * successor belongs to a newer mount. The held PR is the companion's, not
 * the state's: it outlives a remount and a hidden companion.
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
  } & ReadIntent = {},
): Promise<ReviewPaneState | null> {
  const companion = openCompanion(sourcePaneId, 'review');
  if (!companion) return null;
  const hasExplicitSelection = opts.scope !== undefined || opts.editItemId !== undefined;
  const userInitiated = opts.userInitiated ?? false;
  const state = reviewStateForPane(sourcePaneId, subject, { deferInitialLoad: hasExplicitSelection, userInitiated });
  if (opts.editItemId) {
    state.pendingEditItemID = opts.editItemId;
  }
  if (opts.scope) {
    await state.setScope(opts.scope, { userInitiated });
  } else if (opts.editItemId) {
    // A pinned edit implies edits scope; setScope reloads even when the
    // pane already sits there, which is what consumes the pending pin.
    await state.setScope('edits', { userInitiated });
  }
  // After the scope switch, which retires the previous diff and the reveals
  // that came with it.
  if (opts.filePath) state.requestFileJump(opts.filePath);
  return state;
}

/**
 * Builds a review state. Only the registries call it: `reviewStateForPane`
 * for a thread's companion, `reviewPanePinned.svelte#reviewStateForPR` for
 * a review pinned to one PR, each owning the state's disposal.
 */
export function createReviewPaneState(
  sourcePaneId: string,
  subject: ReviewSubject,
  deferInitialLoad: boolean,
  // Whether a person opened the pane, which the first load carries.
  initialUserInitiated = false,
): ReviewPaneState {
  installHeldPRObservers();
  const { identity, threadId, rowId, workspace } = subject;
  // A pinned review's PR and computer are the ones it was opened on; it
  // never reads the owner pane's git status, thread or composer.
  const pinned = subject.pinned ?? null;
  const backend = pinned?.backend ?? threadMachine(threadId ?? '', workspace.projectId);
  function computerPRKey(ref: PRRef): string { return composeWorkspaceKey(backend, prKey(ref)); }
  // Scope persistence is keyed on a real row; a draft placeholder has no
  // history to restore and nothing to write back, and a pinned review's
  // scope is always pr.
  const persisted = pinned ? { scope: 'pr' as const, baseBranch: null } : threadId === null ? null : readPersistedScope(threadId);
  // Derived, not probed-once: the PR is the workspace's, read from the live
  // git-status store, so it becomes selectable the moment status lands (or a
  // PR opens while the pane sits open) instead of only when something
  // re-enters pr scope.
  // In pr scope the PR stays on screen after the forge stops listing it
  // (a merge, from here or elsewhere): the last open PR is held until the
  // scope changes, so the merged snapshot does not vanish under the
  // reader when git status catches up.
  const initialScope: ReviewScope = persisted?.scope ?? 'workspace';
  let scope: ReviewScope = $state(initialScope);
  let heldPRRef = $state<PRRef | null>(initialScope === 'pr' ? (heldPRByRow.get(rowId) ?? null) : null);
  const prRef: PRRef | null = $derived(pinned ? pinned.pr : (workspacePRRef() ?? (scope === 'pr' ? heldPRRef : null)));
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
  let loadedPRHead = $state<{ key: string; revision: PRRevision } | null>(null);
  let refreshingPRData = $state(false);
  // This pane's reference on the shared PR entity — held exactly while the
  // pane is in pr scope. The subscription, poll pump and CI pipeline under
  // it are shared with every other pane on the PR, and the conflict tree
  // with every pane on the PR in this checkout.
  let prAttachment: PRAttachment | null = null;
  let conflictView = $state(false);
  // The conflict view's reconcile permit, held for exactly as long as the
  // view is open AND a mounted pane that renders this state is on screen
  // (`isPaneOnScreen`: every mounted pane on the desktop, the one compact
  // shows): a head move recomputes the merged tree only for a view
  // somebody is looking at (see prReviewConflicts.svelte.ts). A retained
  // state off screen (a pinned review the page keeps for a step back, a
  // companion compact navigated away from) holds none.
  let conflictReconcilePermit: { key: string; release: () => void } | null = null;
  // Mounted surfaces by the layout pane that hosts them.
  const surfacePanes = new SvelteMap<string, number>();
  let conflictCollapsedPaths: SvelteSet<string> = $state(new SvelteSet<string>());
  // Expanded fold ids per path. Entries are replaced wholesale on expand
  // so the SvelteMap write re-derives conflictFiles.
  const conflictExpandedFolds = new SvelteMap<string, ReadonlySet<number>>();
  // The CI of the PR on screen, and the job log this pane opened on it
  // (ciSubject.svelte.ts). The log is the PR's, shared by every pane
  // following that job; this pane holds its own follow on it.
  const ci = createCISubjectState(() => (scope === 'pr' && prRef ? { kind: 'pr', backend, pr: prRef } : null));
  let submitTarget: 'agent' | 'pr' = $state('agent');
  let verdict: 'comment' | 'approve' | 'request-changes' = $state('comment');
  let summaryBody = $state('');
  let submitError: string | null = $state(null);
  const resolvingThreadIds: SvelteSet<string> = $state(new SvelteSet<string>());
  const resolveErrors = new SvelteMap<string, string>();
  // The PR header's Conversation section. The order and the
  // expanded-by-default set are CAPTURED, not derived: they must hold
  // still while the reader is in the section, whatever the poll pump
  // replaces underneath (see the interface comment).
  let conversationOpen = $state(false);
  let descriptionOpen = $state(false);
  let mergeOpen = $state(false);
  // The merge action in flight, then (mergeAwait) the one whose success
  // is waiting for the pump's answer: it reads as pending while the PR's
  // applied seq is below the seq the action answered with (the poll it
  // asked for stamped that seq, or stamped nothing when unchanged). The
  // seq is the backend process's (its hello's launchId): a restarted
  // backend's pump numbers from one again and its fresh snapshot is the
  // forge's answer, so the wait ends with the launch; a re-subscribe to
  // the same process (gap recovery) keeps it.
  let mergeAction = $state<MergeActionKind | null>(null);
  let mergeAwait = $state<{ kind: MergeActionKind; key: string; seq: number; launch: string } | null>(null);
  const backendLaunch = $derived(getTransportHelloFor(backend)?.launchId ?? '');
  const overviewScrollTops = new SvelteMap<ReviewSectionId, number>();
  let unresolvedCursor: string | null = $state(null);
  let conversationOrder: readonly string[] = $state([]);
  let conversationDefaultExpanded: ReadonlySet<string> = $state(new Set<string>());
  const conversationExpandOverrides = new SvelteMap<string, boolean>();
  let pendingConversationThreadId: string | null = $state(null);
  let expandedPRThreadIds: SvelteSet<string> = $state(new SvelteSet<string>());
  let commits: BranchCommit[] = $state([]);
  let commitsTotal: number | null = $state(0);
  let selectedCommitSHA: string | null = $state(null);
  // A run's older end, with `selectedCommitSHA` its newer; null for one
  // commit. pr scope only.
  let rangeFirstSHA: string | null = $state(null);
  const selectedCommits = $derived<CommitSelection | null>(
    selectedCommitSHA ? { first: rangeFirstSHA ?? selectedCommitSHA, last: selectedCommitSHA } : null,
  );
  // The head of the revision the commit list was read at.
  let commitsHead = $state('');
  let commitSelectionLost = $state(false);
  const headRange = $derived(scope === 'pr' && isCommitRange(selectedCommits) && selectedCommits?.last === commitsHead);
  const prLineIndex = createReviewPRLineIndex();
  // The diff on screen's binary files; targeted at each load's commit.
  const media = createReviewDiffMedia();
  // Text SVGs shown as images, by path; a new subject clears them.
  const renderedSvgPaths = new SvelteSet<string>();
  const mediaHost: ReviewMediaHost = {
    media,
    get newSideOnly() { return shown?.scope === 'edits'; },
    renderedPaths: renderedSvgPaths,
    toggleRendered(path) {
      if (renderedSvgPaths.has(path)) renderedSvgPaths.delete(path);
      else renderedSvgPaths.add(path);
    },
    reloadDiff() {
      void reload({ userInitiated: true });
    },
  };
  let edits: EditDiffEntryView[] = $state([]);
  let editTurnLabels: ReadonlyMap<number, string> = $state(new Map());
  let selectedEdit: EditSelection | null = $state(null);
  let pendingEditItemID: string | null = null;
  // A pinned review has no agent: its drafts only ever post to the PR.
  const effectiveSubmitTarget = $derived<'agent' | 'pr'>(
    pinned ? 'pr' : scope === 'pr' && (!selectedCommitSHA || headRange) ? submitTarget : 'agent',
  );
  let pendingJumpFilePath: string | null = $state(null);
  let pendingJumpRowKey: string | null = $state(null);
  const revealedPaths = new SvelteSet<string>();

  // Every jump that targets a file in the diff comes through here, so a
  // file the filter hides is shown before the row is requested: the diff
  // body consumes a request for a row it does not render.
  function revealFile(path: string): void {
    if (path !== '') revealedPaths.add(path);
  }

  function requestFileJump(path: string): void {
    revealFile(path);
    pendingJumpFilePath = path;
  }
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
  // The PR review submit whose answer was lost and whose outcome is not
  // known yet (resolveLostSubmit); a press of Send asks again instead of
  // posting.
  let openSubmit: SubmitAttempt | null = $state.raw(null);
  let openEditors: EditorRecord[] = $state.raw([]);
  // Draft-editor text lives HERE, not in the row component: editor rows
  // are virtualized, so scrolling one out of the render window unmounts
  // it — row-local state would silently drop the user's typed text. By
  // editor id.
  const draftBodies = new SvelteMap<string, string>();
  let shown: ShownDiff | null = $state.raw(null);
  // The editor a picked line takes ("Move").
  let armedEditorId: string | null = $state(null);
  // The editor last opened or focused by a pick: a shift+click spans it.
  let lastEditorId: string | null = null;
  // The same for a saved draft open in its editor, by comment id, under
  // the comment source the draft belongs to. Anchors do not key it, so a
  // new view of the same subject (whitespace, a commit, an edit) keeps
  // it; it goes with its draft: saved, sent, deleted or missing from a
  // read of its source (pruneDraftEdits), and with the subject (setScope).
  const draftEditBodies = new SvelteMap<string, { sourceKey: string; body: string }>();
  function pruneDraftEdits(source: string, read: readonly DiffReviewComment[]): void {
    const live = new Set(read.filter((comment) => comment.status === 'draft').map((comment) => comment.id));
    for (const [id, edit] of draftEditBodies) {
      if (edit.sourceKey === source && !live.has(id)) draftEditBodies.delete(id);
    }
  }
  // One-shot focus request, set on user-initiated open and consumed by
  // the editor's mount effect. Without it, an editor row remounting as
  // it re-enters the render buffer would steal focus mid-scroll.
  let pendingEditorFocusId: string | null = null;
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
  // The `-w` the files on screen were loaded with. A refresh that fails
  // keeps those files, so the toggle goes back to describing them.
  let shownIgnoreWhitespace = false;
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
  const prEvents = $derived<readonly ReviewEvent[]>(prSnapshot?.events ?? EMPTY_PR_EVENTS);
  // A poll that failed after the action is the pump's answer too: the
  // banner says why the forge's state is unknown, and the button is free.
  const mergeActionShown = $derived<MergeActionKind | null>(
    mergeAction ?? (mergeAwait !== null && mergeAwait.key === prEntityKey && backendLaunch === mergeAwait.launch && peekPRSeq(prEntityKey) < mergeAwait.seq && prUpdateError === null ? mergeAwait.kind : null),
  );
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
    const verdicts = prSnapshot?.detail?.latestReviews ?? [];
    for (const verdict of verdicts) {
      const parsed = Date.parse(verdict.submittedAt);
      out.push({
        timeMs: Number.isFinite(parsed) ? parsed : 0,
        item: { kind: 'verdict', id: `v:${verdict.authorLogin}:${verdict.submittedAt}`, verdict },
      });
    }
    // A forge event; an approval the verdict row above already shows (same
    // reviewer, same minute) is not repeated.
    for (const event of prEvents) {
      const parsed = Date.parse(event.at);
      const timeMs = Number.isFinite(parsed) ? parsed : 0;
      if (event.kind === 'approved' && verdicts.some((verdict) => verdict.authorLogin === event.actorLogin
        && verdict.state.toUpperCase() === 'APPROVED' && Math.abs(Date.parse(verdict.submittedAt) - timeMs) < 60_000)) continue;
      out.push({ timeMs, item: { kind: 'event', id: `e:${event.id}`, event } });
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
  const conversationCount = $derived(conversationFeedSource.reduce((count, entry) => count + (entry.item.kind === 'commits' ? 0 : 1), 0));
  const unresolvedThreads = $derived.by<readonly ReviewThread[]>(() => {
    const out: ReviewThread[] = [];
    for (const entry of conversationFeedSource) {
      if (entry.item.kind !== 'thread') continue;
      const thread = entry.item.thread;
      if (thread.isResolvable && !thread.isResolved) out.push(thread);
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
  const conflictsState = $derived(peekPRConflicts(prEntityKey, workspace));
  // The loaded head, but only while it still describes the PR on screen.
  // Everything anchored to the diff — the stale banner, span context,
  // draft commitSha, sent-marks — reads this rather than the raw stamp, so
  // none of them can quote a head that belongs to another pull request.
  const loadedPRRevision = $derived(
    loadedPRHead !== null && loadedPRHead.key === prEntityKey ? loadedPRHead.revision : null,
  );
  const loadedPRHeadSHA = $derived(loadedPRRevision?.headSHA ?? '');
  const prStale = $derived.by(() => {
    const live = prSnapshot?.headSHA ?? '';
    return live !== '' && loadedPRHeadSHA !== '' && live !== loadedPRHeadSHA;
  });
  // Why the merge actions are withheld: this PR's diff is not on screen
  // yet (loading, or its load failed), the diff on screen is not the PR
  // the forge holds, or the forge's state is unknown. Null when they may
  // run. The actions act on what the reader saw, so none runs on a diff
  // that has not loaded for this PR.
  const mergeWithheldReason = $derived<string | null>(
    loadedPRRevision === null ? (loading ? 'The diff is still loading.' : 'The diff has not loaded for this pull request. Reload it to act on it.')
      : prStale ? 'The pull request changed since you loaded it. Reload to act on it.'
        : prUpdateError !== null ? 'Updates from the forge are failing, so its state is unknown.'
          : null,
  );
  // Head-pinned actions name the head the DIFF was loaded at, so a push
  // the reader has not seen is refused by the forge, not merged blind.
  // Never the live head: the actions are withheld until a diff is loaded.
  const mergeHeadSHA = $derived(loadedPRHeadSHA);
  const manage = createPRManageState({
    get prRef() { return prRef; },
    get identity() { return prRef ? computerPRKey(prRef) : null; },
    get mergeMethods() { return prSnapshot?.detail?.merge?.methods ?? null; },
    get entityKey() { return prEntityKey; },
    get appliedSeq() { return peekPRSeq(prEntityKey); },
    backend,
    get launch() { return backendLaunch; },
    get withheldReason() { return mergeWithheldReason; },
    get updateFailing() { return prUpdateError !== null; },
    refresh: () => { void refreshPRThreads(); },
  });
  const sourceKey = $derived.by(() => sourceKeyFor(prRef));
  // The comment source of the selection under the PR `pr`: the controls'
  // (`sourceKey`) or, at a load's commit, the PR that load was for.
  function sourceKeyFor(pr: PRRef | null): string {
    if (scope === 'edits') {
      // An edit payload is immutable, so its id is the stable identity;
      // a whole-turn view keys by the turn (its edit set only grows
      // while the turn is still streaming).
      if (!selectedEdit) return '';
      return selectedEdit.kind === 'item'
        ? `edit:${selectedEdit.payloadId}`
        : `edit-turn:${selectedEdit.turnIndex}`;
    }
    if (selectedCommitSHA && !headRange) {
      // A single commit's content is immutable — the SHA itself is the
      // stable identity, in both branch and pr scope; a run's is its ends.
      // A run ending at the head shows the head's lines: its comments are
      // the PR's, below.
      return rangeFirstSHA ? `range:${rangeFirstSHA}..${selectedCommitSHA}` : `commit:${selectedCommitSHA}`;
    }
    if (scope === 'pr' && pr) {
      // Stable across PR head movement: drafts must survive pushes; each
      // draft's commitSha records the head SHA it was anchored to.
      return prSourceKey(pr);
    }
    return patchKey;
  }
  // The subject the controls name; a load commits the subject it resolved.
  const requestedSubject = $derived(reviewSubjectKey({
    scope,
    baseBranch: scope === 'branch' ? baseBranch : null,
    commit: commitSubject(selectedCommits),
    edit: scope === 'edits' ? editSelectionKey(selectedEdit) : null,
    pr: scope === 'pr' && prRef ? computerPRKey(prRef) : '',
  }));
  // The open editors of the diff on screen, as the rows place them.
  const shownEditors: readonly ReviewEditor[] = $derived(shown === null ? [] : openEditors
    .filter((editor) => editor.subject === shown?.subject)
    .map(({ id, filePath, side, state, anchor, suggested, occurrence, repeated, range, rangeRefusal }) => ({
      id, filePath, side, state, anchor, suggested, occurrence, repeated, armed: id === armedEditorId, generation: shown?.generation ?? 0,
      range: range ?? undefined, rangeRefusal: rangeRefusal ?? undefined,
    })));
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
  // The conflict view's files and, per file with content, the hidden lines
  // of each fold it renders: find reads the folds this one build made.
  const conflictProjection = $derived.by(() => {
    const folds = new Map<string, Map<number, readonly string[]>>();
    const conflicts = conflictsState.state;
    if (!conflicts) return { files: [] as ReviewFile[], folds };
    const { baseLabel, headLabel, notes } = conflicts;
    const files = conflicts.paths.map((path) => {
      const content = conflictsState.contentByPath.get(path);
      const pathNotes = notes[path];
      // A structural conflict's content can be unfetchable (the path may
      // not exist in the merged tree at all) — its notes still render.
      if (content !== undefined || pathNotes?.length) {
        const fileFolds = new Map<number, readonly string[]>();
        if (content !== undefined) folds.set(path, fileFolds);
        return reviewFileFromPatchFile(conflictPatchFile(path, content ?? '', {
          baseLabel,
          headLabel,
          notes: pathNotes,
          expandedFolds: conflictExpandedFolds.get(path),
          onFold: (id, lines) => fileFolds.set(id, lines),
        }));
      }
      return reviewFileFromPatchFile({ path, kind: 'conflict', additions: 0, deletions: 0, lines: [] });
    });
    return { files, folds };
  });
  const conflictFiles = $derived(conflictProjection.files);
  function conflictFoldLines(path: string): ReadonlyMap<number, readonly string[]> | null {
    return conflictProjection.folds.get(path) ?? null;
  }
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
      const live = workspacePRRef();
      if (live) {
        heldPRRef = live;
        heldPRByRow.set(rowId, live);
      } else if (scope !== 'pr') {
        heldPRRef = null;
        heldPRByRow.delete(rowId);
      }
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
    !conflictView && ci.logView === null && supportsIgnoreWhitespace(scope, selectedCommitSHA),
  );
  // Review comments and turn state belong to a THREAD's history; a draft
  // placeholder has neither, so the comment affordances stay dark rather
  // than pointing at a row that does not exist yet.
  // A pinned review has no thread row: its drafts are the PR's
  // (prDiffReviewComments.svelte.ts), on the computer it was opened on,
  // and post as a PR review.
  const pinnedDrafts: PRDraftsTarget | null = pinned ? { backend, pr: pinned.pr } : null;
  const pinnedDraftKey = pinnedDrafts ? prDraftsKey(pinnedDrafts) : null;
  const comments = $derived.by((): readonly DiffReviewComment[] => {
    if (pinnedDraftKey !== null) return getPRDiffReviewComments(pinnedDraftKey, scope, sourceKey);
    return threadId === null ? EMPTY_COMMENTS : getDiffReviewComments(threadId, scope, sourceKey);
  });
  const commentsGranted = $derived(
    pinned ? hasScope('git:operate', backend) : threadHasScope('threads:operate', threadId, workspace.projectId),
  );
  const canDraftComments = $derived(pinned ? scope === 'pr' && (selectedCommitSHA === null || headRange) : threadId !== null);
  const drafts = $derived(comments.filter((comment) => comment.status === 'draft'));
  const shownDrafts = $derived.by(() => (shown?.headRange ? rangeShownDrafts(drafts) : drafts));
  // A summary or verdict kept for the next PR review needs the strip to
  // send it, drafts or not.
  const sendStripShown = $derived(
    drafts.length > 0 || sendingComments || openSubmit !== null
      || (effectiveSubmitTarget === 'pr' && (summaryBody.trim() !== '' || verdict !== 'comment')),
  );
  const isTurnActive = $derived(threadId !== null && getActiveTurn(threadId) !== null);

  // Send now or hold, and the reply composers (reviewCommentActions.svelte.ts).
  const commentActions = createReviewCommentActions({
    backend,
    scope: () => scope,
    // A head-ending run posts to the PR as All commits does, line by line.
    selectedCommitSHA: () => (headRange ? null : selectedCommitSHA),
    ref: () => prRef,
    entityKey: () => prEntityKey,
    launch: () => backendLaunch,
    updateFailing: () => prUpdateError !== null,
    forgeGranted: () => (pinned ? hasScope('git:operate', backend) : threadHasScope('git:operate', threadId, workspace.projectId)),
    prWithheld: () => mergeWithheldReason,
    canDraft: () => canDraftComments && commentsGranted,
    drafts: () => drafts,
    threads: () => prThreads,
    threadsLoaded: () => prSnapshot !== null,
    sourceKey: () => sourceKey,
    loadedHeadSHA: () => loadedPRHeadSHA,
    saveDraft: (input) => saveDraft(input),
    editDraft(draft) {
      if (draftEditBodies.get(draft.id) === undefined) draftEditBodies.set(draft.id, { sourceKey, body: draft.body });
    },
    refreshThreads: () => refreshPRThreads(),
    showPostedComment: (commentId) => joinPostedComment(commentId),
  });

  // Suggestions, reactions and the editors' Preview
  // (reviewCommentExtras.svelte.ts). A suggestion's replaced text is read
  // only from the PR's whole diff at its live head.
  const openEditorKeys = $derived.by(() => {
    const out = new Set<string>([EDITOR_PREVIEW_KEY.summary]);
    for (const editor of openEditors) out.add(commentWriteKey.editor(editor.id));
    for (const id of draftEditBodies.keys()) out.add(EDITOR_PREVIEW_KEY.draftEdit(id));
    for (const thread of prThreads) {
      if (commentActions.state.replyOpen(thread.id, 'diff') || commentActions.state.replyOpen(thread.id, 'conversation')) out.add(commentWriteKey.reply(thread.id));
    }
    for (const key of commentActions.writes.editing) out.add(`edit:${key}`);
    if (commentActions.writes.prCommentOpen) out.add(commentWriteKey.prComment);
    if (manage.descriptionDraft !== null) out.add(EDITOR_PREVIEW_KEY.description);
    return out;
  });
  const extras = createReviewCommentExtras({
    backend,
    ref: () => (scope === 'pr' ? prRef : null),
    identity: () => (prRef ? computerPRKey(prRef) : null),
    appliedSeq: () => peekPRSeq(prEntityKey),
    launch: () => backendLaunch,
    updateFailing: () => prUpdateError !== null,
    refreshThreads: () => refreshPRThreads(),
    onPR: () => scope === 'pr' && prRef !== null,
    withheld: () => commentActions.state.nowWithheldReason,
    loadedHeadSHA: () => loadedPRHeadSHA,
    diffIsCurrentHead: () => scope === 'pr' && selectedCommitSHA === null && loadedPRRevision !== null && !prStale,
    files: () => loadedFiles,
    openEditorKeys: () => openEditorKeys,
    reactionsUnavailable: () => prSnapshot?.reactionsUnavailable ?? null,
  });

  // The workspace's CURRENT open PR, read live from the shared git-status
  // store. Not memoized: a PR opened while this pane sat open must become
  // selectable, and one that merged must stop being offered.
  function workspacePRRef(): PRRef | null {
    // The owner pane of a pinned review may hold a thread later; its git
    // status names that thread's PR, not this one.
    if (pinned) return null;
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
  // The pr-scope load that failed, with the snapshot it ran against and
  // that snapshot's head: the watcher below retries when a newer snapshot
  // frame arrives.
  let failedPRLoad = $state<{ snapshot: PRSnapshot; headSHA: string } | null>(null);
  const awaitingPR = $derived(awaitingPRDetail || awaitingPRStatus);
  // The intent of the load that is waiting on the status or the snapshot:
  // no PR read has run for it yet, so the load the wait ends is still the
  // one a person may have asked for. Every load resets it, so a PR that
  // appears after the status answered "none" reads unattended.
  let awaitedUserInitiated = false;
  function workspacePRLookupSettled(): boolean {
    if (pinned) return true;
    const key = workspaceKeyForThread(getPane(sourcePaneId)?.thread ?? null);
    const status = peekGitStatus(key);
    if (status !== null) return !status.openPrLookupPending;
    return peekGitStatusError(key) !== null;
  }
  const disposePRRefWatch = $effect.root(() => {
    // The screen moving (compact navigation) takes or releases the permit.
    $effect(() => syncConflictPermit());
    $effect(() => {
      const key = shownHeadRangeKey;
      untrack(() => {
        const ref = prRef;
        const revision = loadedPRRevision;
        if (key === null || ref === null || revision === null) prLineIndex.close();
        else prLineIndex.open(key, readWholePRDiff(ref, revision, shown?.ignoreWhitespace ?? false));
      });
    });
    $effect(() => {
      if (!awaitingPRRef || prRef === null) return;
      awaitingPRRef = false;
      void reload({ userInitiated: awaitedUserInitiated });
    });
    $effect(() => {
      // Status landed, with or without a PR: the load runs now, so a
      // workspace with no PR answers "no PR" instead of waiting forever.
      // A status that names a PR resolves the ref too, and the watcher
      // above, which runs first, carries the waiting load's intent.
      if (!awaitingPRStatus || !workspacePRLookupSettled()) return;
      awaitingPRStatus = false;
      void reload();
    });
    $effect(() => {
      if (!awaitingPRDetail || !prRevision(prSnapshot?.detail, prSnapshot?.headSHA)) return;
      awaitingPRDetail = false;
      void reload({ userInitiated: awaitedUserInitiated });
    });
    $effect(() => {
      // A failed pr-scope load retries on the next frame the pump delivers:
      // the backend has the pump emit its next successful poll after a
      // failed read, so this is "once the forge answers again", not a
      // timer. The retry reads unattended, so its failure arms no further
      // emit. A diff still on screen of a PR whose head has since moved is
      // the stale banner's to reload (it measures the head alone).
      const failed = failedPRLoad;
      const live = prSnapshot;
      if (failed === null || live === null || live === failed.snapshot) return;
      if (live.headSHA !== failed.headSHA && untrack(() => loadedFiles.length > 0)) return;
      failedPRLoad = null;
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
    // A pinned review asks the computer every one of its calls routes to.
    const granted = pinned
      ? hasScope('git:operate', backend)
      : threadHasScope('git:operate', subject.threadId, subject.workspace.projectId);
    if (!granted) return null;
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
    prLineIndex.close();
    media.setTarget(null);
    commentActions.dispose();
    extras.dispose();
    showRead(null);
    disposePRRefWatch();
    disposePaintedRetention();
    paintedSpans.clear();
    conflictPaintedSpans.clear();
    resetConflictView();
    ci.dispose();
    manage.dispose();
    releasePR();
  }

  // The one writer of `conflictView`, so the reconcile permit cannot drift
  // out of step with what is on screen: an unreleased permit keeps
  // recomputing merge trees for a surface nobody closed, and a missing one
  // leaves an open view rendering the previous head's merge.
  function setConflictView(open: boolean): void {
    conflictView = open;
    untrack(syncConflictPermit);
  }

  // The one writer of the permit.
  function syncConflictPermit(): void {
    // Tracked only by the watcher in the effect root (the screen moving
    // re-decides); every other caller runs it untracked, so a component
    // effect that attaches a surface never depends on what it writes.
    const shown = conflictView && [...surfacePanes.keys()].some(isPaneOnScreen);
    untrack(() => {
      const key = shown && !disposed ? prEntityKey : null;
      if (conflictReconcilePermit?.key === key) return;
      conflictReconcilePermit?.release();
      conflictReconcilePermit = null;
      if (key) conflictReconcilePermit = { key, release: permitPRConflictReconcile(key, workspace) };
    });
  }

  function attachSurface(paneId: string): () => void {
    untrack(() => {
      surfacePanes.set(paneId, (surfacePanes.get(paneId) ?? 0) + 1);
      syncConflictPermit();
    });
    let attached = true;
    return () => {
      if (!attached) return;
      attached = false;
      untrack(() => {
        const remaining = (surfacePanes.get(paneId) ?? 1) - 1;
        if (remaining > 0) surfacePanes.set(paneId, remaining);
        else surfacePanes.delete(paneId);
        syncConflictPermit();
      });
    };
  }

  // Only the pane's VIEW of the conflicts resets on a scope switch: the
  // merged tree belongs to the PR in this checkout and may still be on
  // screen in another pane. It is released with this pane's reference.
  function resetConflictView(): void {
    setConflictView(false);
    conflictCollapsedPaths = new SvelteSet<string>();
    conflictExpandedFolds.clear();
  }

  async function setScope(nextScope: ReviewScope, opts?: { baseBranch?: string } & ReadIntent): Promise<void> {
    if (pinned && nextScope !== 'pr') {
      throw new Error('A pull request review shows only its pull request.');
    }
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
    // Re-entering the subject on screen (the PR badge, the selected scope
    // picked again, the selected edit opened again) reloads it and keeps
    // every open editor: draft edits name their drafts, and the reload
    // moves new-comment editors with their lines (relocateOpenEditors).
    const subjectChanged = scopeChanged || nextBaseBranch !== baseBranch;
    if (subjectChanged) retireDiffSubject();
    scope = nextScope;
    baseBranch = nextBaseBranch;
    // Back to "latest" on scope entry — the previous selection belongs
    // to another commit range. A base-branch change within branch scope
    // keeps it; reload's validation drops a selection that left the new
    // range.
    if (scopeChanged) {
      selectedCommitSHA = null;
      rangeFirstSHA = null;
      commitSelectionLost = false;
      selectedEdit = null;
    }
    if (subjectChanged) draftEditBodies.clear();
    keepEditorsOf(requestedSubject);
    collapseOverrides.clear();
    if (threadId !== null) persistScope(threadId, scope, baseBranch);
    await reload({ userInitiated: opts?.userInitiated });
  }

  async function selectCommit(sha: string | null, opts?: ReadIntent): Promise<void> {
    await selectCommits(sha ? { first: sha, last: sha } : null, opts);
  }

  async function selectCommits(selection: CommitSelection | null, opts?: ReadIntent): Promise<void> {
    // A run is a pr-scope pick; a run equal to the whole list is the
    // whole diff (commitSelectionBetween), never sent.
    const asked = selection && (scope === 'pr' || selection.first === selection.last) ? selection : null;
    const next = scope === 'pr' ? resolveCommitSelection(asked, commits, commitsTotal).selection : asked;
    commitSelectionLost = false;
    if (sameCommitSelection(next, selectedCommits)) return;
    navigationSeq++;
    seedPRLineIndex();
    retireDiffSubject();
    selectedCommitSHA = next?.last ?? null;
    rangeFirstSHA = next && next.first !== next.last ? next.first : null;
    keepEditorsOf(requestedSubject);
    collapseOverrides.clear();
    await reload({ selectionOnly: true, userInitiated: opts?.userInitiated });
  }

  async function showChangesFrom(sha: string): Promise<void> {
    const selection = commitSelectionFromHere(commits, commitsTotal, sha);
    await selectCommits(selection, { userInitiated: true });
    if (disposed || !sameCommitSelection(selectedCommits, selection)) return;
    // To the diff: a run's card leads it; the whole PR's diff starts at
    // its first file (the overview above it is where the press was).
    if (selection) pendingJumpRowKey = REVIEW_COMMIT_CARD_ROW_KEY;
    else if (files.length > 0) requestFileJump(files[0].path);
  }

  // The whole PR's diff index of a head-ending run (reviewPRLineIndex):
  // keyed by the PR, the revision and `-w` the run was read at, open
  // exactly while such a run is on screen.
  function prLineIndexKey(revision: PRRevision, whitespace: boolean): string {
    return [prEntityKey ?? '', revision.headSHA, revision.baseSHA, whitespace ? 'w' : ''].join('\u0000');
  }
  // Leaving the whole diff for a run: its files are the index, so a run
  // to the head needs no second read of them.
  function seedPRLineIndex(): void {
    if (shown?.scope !== 'pr' || selectedCommitSHA !== null || loadedPRRevision === null || loadedFiles.length === 0) return;
    prLineIndex.seed(prLineIndexKey(loadedPRRevision, shown.ignoreWhitespace), loadedFiles);
  }
  const shownHeadRangeKey = $derived.by(() => {
    const bundle = shown;
    return bundle?.headRange && loadedPRRevision !== null ? prLineIndexKey(loadedPRRevision, bundle.ignoreWhitespace) : null;
  });
  function readWholePRDiff(ref: PRRef, revision: PRRevision, whitespace: boolean): () => Promise<PRLineIndex> {
    return async () => {
      const source = new ReviewDiffSource(backend, () => OpenPRDiff(workspace, prReferenceWire(ref), revision, whitespace, false));
      const read = await source.read({ cancelled: () => disposed });
      if (!read) throw new Error('The review closed.');
      // Kinds and hunk starts are all the index keeps; the text goes.
      try {
        return buildPRLineIndex(read.parser.files);
      } finally {
        read.dispose();
      }
    };
  }

  function lineComment(anchor: Pick<CommentAnchor, 'filePath' | 'newLine' | 'side'>): { target: LineCommentTarget; reason: string | null } | null {
    if (!headRange) return null;
    const target = lineCommentTarget(prLineIndex, anchor, !pinned);
    return { target, reason: lineCommentReason(prLineIndex, target, anchor) };
  }

  async function setIgnoreWhitespace(ignore: boolean, opts?: ReadIntent): Promise<void> {
    if (ignore === ignoreWhitespace) return;
    ignoreWhitespace = ignore;
    // THIS IS A FULL DIFF RE-REQUEST, NOT A RENDER TOGGLE, and that cost
    // is accepted deliberately. `-w` changes the patch git emits — hunks
    // narrow, whitespace-only files vanish — so there is no projection of
    // the `-w` view out of the patch already in hand. Everything derived
    // from the patch text has to be rebuilt: parsed files, the px-pinned
    // row geometry, and the highlight-span cache (keyed by line content).
    // `selectionOnly` keeps it to the diff call — the commit/edit lists
    // and the PR subscription describe the same subject either way. Open
    // editors stay: the reload places each on its line again.
    // Collapse overrides deliberately SURVIVE, unlike selectCommit/selectEdit:
    // those switch which change is under review, whereas this shows the same
    // change with less noise. A file the user opened stays open.
    await reload({ selectionOnly: true, userInitiated: opts?.userInitiated });
  }

  async function selectEdit(key: string | null, opts?: ReadIntent): Promise<void> {
    const next = editSelectionFromKey(key, edits);
    if (editSelectionKey(next) === editSelectionKey(selectedEdit)) return;
    navigationSeq++;
    retireDiffSubject();
    selectedEdit = next;
    keepEditorsOf(requestedSubject);
    collapseOverrides.clear();
    await reload({ selectionOnly: true, userInitiated: opts?.userInitiated });
  }

  // A new subject cannot reuse the previous patch under its new scope,
  // anchors or span context. Same-subject refreshes intentionally retain it.
  function retireDiffSubject(): void {
    revealedPaths.clear();
    renderedSvgPaths.clear();
    media.setTarget(null);
    loadedFiles = [];
    shown = null;
    armedEditorId = null;
    showRead(null);
    patchKey = '';
    clearContextExpansions();
  }

  async function reload(opts?: { selectionOnly?: boolean } & ReadIntent): Promise<void> {
    const userInitiated = opts?.userInitiated ?? false;
    awaitedUserInitiated = false;
    // A commit/edit selection changes only which diff is shown — the
    // selector list and the PR snapshot are still valid, so reuse them and
    // fetch just the diff. Only when a previous full load actually
    // populated them; otherwise fall through to a full load.
    const selectionOnly = opts?.selectionOnly === true
      && (scope === 'edits'
        ? edits.length > 0
        : commits.length > 0 && (scope !== 'pr' || (prAttachment !== null && loadedPRRevision !== null)));
    // The inline-diff affordance pins an edit for the NEXT load;
    // consumed here so a later manual reload doesn't re-pin it.
    const pinnedEditItemID = pendingEditItemID;
    pendingEditItemID = null;
    const seq = loadSeq + 1;
    loadSeq = seq;
    loading = true;
    error = null;
    failedPRLoad = null;
    // Files on screen when the load starts belong to this subject (a new
    // subject retires them first): a failed refresh leaves them up.
    const keepsShownFiles = loadedFiles.length > 0;
    const requestedIgnoreWhitespace = ignoreWhitespace;
    // The diff is computed at the shared snapshot's revision, so the
    // snapshot is awaited before the patch call. Attaching is what starts
    // the poll pump — and re-attaching for a PR this pane already holds
    // costs nothing.
    let snapshot: PRSnapshot | null = null;
    try {
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
        if (hold && !prRevision(snapshot?.detail, snapshot?.headSHA)) {
          // Held, but the poller has no revision yet: its failure banner
          // says why (or GitLab is still preparing the merge request's
          // diff), and the watcher above reloads when one lands.
          awaitingPRDetail = true;
          awaitedUserInitiated = userInitiated;
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
          awaitedUserInitiated = userInitiated;
          return;
        }
        awaitingPRStatus = false;
      }
      const loaded = await loadPatch(
        { workspace, threadId, backend },
        scope,
        baseBranch,
        selectedCommits,
        loadingPRRef ?? prRef,
        snapshot,
        { pinnedItemId: pinnedEditItemID, current: selectedEdit },
        // Gate on support, not just the flag: a scope switch can leave the
        // toggle on while the new source can't honor it, and passing it
        // anyway would make the button's state a lie.
        ignoreWhitespace && canIgnoreWhitespace,
        selectionOnly ? { commits, commitsTotal, edits, editTurnLabels, prRevision: loadedPRRevision } : undefined,
        userInitiated,
      );
      if (seq !== loadSeq || disposed) {
        loaded.patch?.abandon();
        return;
      }
      const arrival: Arrival = { loaded, applied: false };
      const read = await readPatch(seq, arrival);
      if (!read) return;
      // The subject this diff is, by the selection the load resolved.
      const committed = committedSubject(loaded, loadingPRKey);
      const placements = await placeOpenEditors(seq, committed, read.files);
      if (seq !== loadSeq || disposed) {
        if (read.read !== shownRead) read.read?.dispose();
        return;
      }
      // Everything the complete diff decides lands in this synchronous
      // step, so no frame shows its files with another load's collapse
      // state, patch key, head, subject or editor lines.
      showFiles(arrival, read.files);
      showRead(read.read);
      patchKey = read.patchKey;
      shownIgnoreWhitespace = requestedIgnoreWhitespace;
      shown = {
        generation: (shown?.generation ?? 0) + 1,
        subject: committed,
        scope,
        sourceKey: sourceKeyFor(loadingPRRef),
        commitSha: (headRange ? undefined : selectedCommitSHA) ?? (scope === 'pr' ? loaded.prRevision?.headSHA : undefined),
        ignoreWhitespace: requestedIgnoreWhitespace,
        headRange,
      };
      media.setTarget(read.target);
      applyPlacements(committed, placements);
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
      if (scope === 'pr' && loadingPRKey && loadingPRRef && loaded.prRevision) {
        // The anchor staleness is measured against: this diff describes
        // this head OF THIS PULL REQUEST, so the banner clears until the PR
        // moves again — for THIS pane, without touching what another pane
        // loaded, and without a switch to a different PR being compared
        // against a head that was never its own.
        loadedPRHead = { key: loadingPRKey, revision: loaded.prRevision };
      }
      // The files, patch key and selection are already updated above, so
      // the derived reflects this load — no need to re-derive by hand.
      const nextSourceKey = sourceKey;
      if (pinnedDrafts !== null && nextSourceKey) {
        try {
          const read = await refreshPRDiffReviewComments(pinnedDrafts, scope, nextSourceKey);
          pruneDraftEdits(nextSourceKey, read);
          if (seq !== loadSeq) return;
          error = null;
        } catch (err) {
          if (seq !== loadSeq) return;
          error = userFacingError(err);
        }
        return;
      }
      if (threadId === null) {
        // A draft placeholder owns no comment store to sync.
        error = null;
        return;
      }
      if (!nextSourceKey) {
        setActiveDiffReviewSource(threadId, null);
        error = null;
        return;
      }
      try {
        pruneDraftEdits(nextSourceKey, await refreshDiffReviewComments(threadId, scope, nextSourceKey));
        if (seq !== loadSeq) return;
        setActiveDiffReviewSource(threadId, scope, nextSourceKey);
        error = null;
      } catch (err) {
        if (seq !== loadSeq) return;
        error = userFacingError(err);
      }
    } catch (err) {
      if (seq !== loadSeq || disposed) return;
      error = userFacingError(err);
      // Only a load that ran against a held snapshot: without one (no
      // git:operate scope) there is no frame of its own to retry on.
      if (scope === 'pr' && snapshot !== null) {
        failedPRLoad = { snapshot, headSHA: snapshot.headSHA };
      }
      if (keepsShownFiles) {
        // A failed refresh of the diff on screen changes nothing about it:
        // its files, comment source and editors stay, under the error.
        ignoreWhitespace = shownIgnoreWhitespace;
        return;
      }
      clearContextExpansions();
      revealedPaths.clear();
      loadedFiles = [];
      showRead(null);
      patchKey = '';
      collapsedPaths = new SvelteSet<string>();
      if (threadId !== null) setActiveDiffReviewSource(threadId, null);
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
      commitsTotal = loaded.commits ? (loaded.commitsTotal ?? null) : 0;
      edits = loaded.edits ?? [];
      editTurnLabels = loaded.editTurnLabels ?? new Map();
      if (loaded.selectedEdit !== undefined) selectedEdit = loaded.selectedEdit;
      if (loaded.selectedCommitSHA !== undefined) {
        selectedCommitSHA = loaded.selectedCommitSHA;
        const run = loaded.selectedCommits;
        rangeFirstSHA = run && run.first !== run.last ? run.first : null;
      }
      if (loaded.commitSelectionLost) commitSelectionLost = true;
      commitsHead = loaded.prRevision?.headSHA ?? '';
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
  ): Promise<{ files: readonly ReviewFile[]; read: ReviewDiffRead | null; patchKey: string; target: DiffFilesTarget | null } | null> {
    const { patch, mergesPaths } = arrival.loaded;
    if (patch === null) return { files: [], read: null, patchKey: '', target: null };
    const merge = mergesPaths ? new EditFileMerge() : null;
    const view = (sections: readonly ReviewFile[]): readonly ReviewFile[] => merge?.files(sections) ?? sections;
    const cancelled = (): boolean => seq !== loadSeq || disposed;
    const progressive = loadedFiles.length === 0;
    // The open whose files this diff's rows read. Rows published while it
    // streams (an empty pane) read under it from the first one; otherwise
    // the rows on screen are another diff's, and it is attached with this
    // diff's files when the read completes.
    let target: DiffFilesTarget | null = null;
    const attach = (next: DiffFilesTarget | null): void => {
      target = next;
      if (progressive && !cancelled()) media.setTarget(next);
    };
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
        // Each open, a retry's included, is its own target: a git diff's
        // by the files handle it answers, an edits diff's by an id minted
        // here.
        attach(arrival.loaded.selectedEdit ? editsTarget(arrival.loaded) : gitTarget(patch, opened.filesHandle ?? ''));
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
    return { files: view(read.parser.files), read, patchKey: read.parser.sourceKey(), target };
  }

  if (!deferInitialLoad) void reload({ userInitiated: initialUserInitiated });

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

  // The open a load's rows read files through: a git diff's files handle
  // (reopened when the server forgets it only for an open of immutable
  // revisions, ReviewDiffSource.revisionPinned), or the edit it replayed.
  function gitTarget(patch: ReviewDiffSource, handle: string): DiffFilesTarget {
    return {
      kind: 'git',
      backend,
      workspace,
      files: new DiffFilesHandle(handle, patch.revisionPinned ? () => patch.reopenFiles() : null),
    };
  }

  function editsTarget(loaded: LoadedPatch): DiffFilesTarget | null {
    const edit = loaded.selectedEdit;
    if (!edit || threadId === null) return null;
    return {
      kind: 'edits',
      backend,
      openId: randomId(),
      threadId,
      editPayloadId: edit.kind === 'item' ? edit.payloadId : '',
      editTurnIndex: edit.kind === 'turn' ? edit.turnIndex : -1,
    };
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
    if (pinned) throw new Error(PINNED_NO_CHAT);
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

  // The subject of a diff a load is committing, by the selection the load
  // resolved (it can replace the one the controls asked for).
  function committedSubject(loaded: LoadedPatch, prKeyOfLoad: string | null): string {
    const commit = loaded.selectedCommits !== undefined
      ? commitSubject(loaded.selectedCommits)
      : loaded.selectedCommitSHA !== undefined ? loaded.selectedCommitSHA : commitSubject(selectedCommits);
    const edit = loaded.selectedEdit !== undefined ? loaded.selectedEdit : selectedEdit;
    return reviewSubjectKey({
      scope,
      baseBranch: scope === 'branch' ? baseBranch : null,
      commit,
      edit: scope === 'edits' ? editSelectionKey(edit) : null,
      pr: scope === 'pr' ? prKeyOfLoad ?? '' : '',
    });
  }

  // Drops the editors of every other subject, with their text.
  function keepEditorsOf(subject: string): void {
    const kept = openEditors.filter((editor) => editor.subject === subject);
    if (kept.length === openEditors.length) return;
    const live = new Set(kept.map((editor) => editor.id));
    for (const id of draftBodies.keys()) {
      if (!live.has(id)) draftBodies.delete(id);
    }
    if (pendingEditorFocusId !== null && !live.has(pendingEditorFocusId)) pendingEditorFocusId = null;
    if (armedEditorId !== null && !live.has(armedEditorId)) armedEditorId = null;
    openEditors = kept;
  }

  function replaceEditor(from: EditorRecord, to: EditorRecord): void {
    if (openEditors.includes(from)) openEditors = openEditors.map((editor) => (editor === from ? to : editor));
  }

  // The editor with no proven line: detached in a file of the diff, gone
  // when the file is not in it.
  function detached(editor: EditorRecord, reason: DetachReason, suggested: CommentAnchor | null = null): EditorRecord {
    return { ...editor, state: reason === 'file-gone' ? 'gone' : 'detached', anchor: null, occurrence: 0, repeated: false, suggested, reason, shownFile: null, baseFile: null, ...NO_RANGE };
  }

  // Places the open editors of `subject` in `next`, the diff a load is
  // about to commit, while the diff on screen can still be read. Runs only
  // when such editors exist; never fails the load.
  async function placeOpenEditors(seq: number, subject: string, next: readonly ReviewFile[]): Promise<Map<EditorRecord, RelocationOutcome> | null> {
    const editors = openEditors.filter((editor) => editor.subject === subject);
    if (editors.length === 0) return null;
    const targets = new Map<EditorRecord, RelocationTarget>();
    for (const editor of editors) {
      targets.set(editor, {
        filePath: editor.filePath,
        side: editor.side,
        selectedText: editor.selectedText,
        guard: editor.guard,
        source: editor.anchor !== null && editor.baseFile !== null ? { file: editor.baseFile, anchor: editor.anchor } : null,
        proof: editor.proof,
        line: editor.line,
      });
    }
    try {
      return await relocateEditors(targets, next, new RelocationBudget(relocationBudget.rows, relocationBudget.bytes), () => seq !== loadSeq || disposed);
    } catch (err) {
      if (err instanceof RelocationCancelled) return null;
      if (!(err instanceof PatchTextLost)) reportFrontendDiagnostic('review diff: open comment editors could not be placed', String(err));
      const paths = new Set(next.map((file) => file.path));
      return new Map(editors.map((editor) => [editor, {
        filePath: editor.filePath, anchor: null, proof: editor.proof, reason: paths.has(editor.filePath) ? 'line-changed' : 'file-gone', suggested: null,
      }]));
    }
  }

  // Applies placements in the commit step, which also disarms a Move. An
  // editor of the subject with no placement was opened on the previous
  // diff while they were worked out: nothing proves its line in this one.
  // Whether an editor is in a file of the diff decides where it renders
  // (its file or the group), whatever placed it.
  function applyPlacements(subject: string, placements: Map<EditorRecord, RelocationOutcome> | null): void {
    armedEditorId = null;
    keepEditorsOf(subject);
    if (openEditors.length === 0) return;
    const inDiff = (path: string): boolean => files.some((file) => file.path === path);
    openEditors = openEditors.map((editor) => {
      const outcome = placements?.get(editor);
      if (!outcome) {
        if (editor.state === 'detached' && inDiff(editor.filePath)) return editor;
        return detached(editor, inDiff(editor.filePath) ? 'line-changed' : 'file-gone', editor.state === 'detached' ? editor.suggested : null);
      }
      const moved = { ...editor, filePath: outcome.filePath, proof: outcome.proof };
      if (!inDiff(outcome.filePath)) return detached(moved, 'file-gone');
      const shownFile = outcome.anchor ? files.find((file) => file.path === outcome.filePath) ?? null : null;
      const baseFile = outcome.anchor ? loadedFiles.find((file) => file.path === outcome.filePath) ?? null : null;
      if (!outcome.anchor || !shownFile || !baseFile) {
        return detached(moved, outcome.reason === 'file-gone' || !outcome.reason ? 'line-changed' : outcome.reason, outcome.suggested);
      }
      // A range stays only while its last line is the line it was proven on.
      const keepsRange = editor.anchor !== null && anchorKey(editor.anchor) === anchorKey(outcome.anchor);
      return {
        ...moved, state: 'placed', anchor: outcome.anchor, occurrence: 0, repeated: false, suggested: null, reason: null,
        line: anchorSideLine(outcome.anchor), shownFile, baseFile, ...(keepsRange ? { rangeRefusal: null } : NO_RANGE),
      };
    });
  }

  // A new editor on `at` of the diff on screen, or null when that diff is
  // not the subject the controls name or has no such line. An unchanged
  // line is saved with both numbers, whichever half was used; a line
  // number several rows share keeps the row the reader opened it on
  // (`lineIndex`) and cannot be saved.
  function newEditor(at: EditorAnchor, id: string = randomId()): EditorRecord | null {
    const bundle = shown;
    if (bundle === null || bundle.subject !== requestedSubject) return null;
    const shownFile = files.find((file) => file.path === at.filePath);
    const baseFile = loadedFiles.find((file) => file.path === at.filePath);
    if (!shownFile || !baseFile) return null;
    const { lineIndex, ...given } = at;
    const anchor = completeAnchor(shownFile, given);
    const rows = anchorRows(shownFile, anchor, lineIndex);
    if (anchor.side !== 'file' && rows.count === 0) return null;
    // The record outlives the chunk its quote may have been read from.
    const quote = keptText.own(anchor.selectedText ?? '');
    const quoteTooLong = utf8Bytes(quote) > EDITOR_QUOTE_MAX_BYTES;
    return {
      id,
      subject: bundle.subject,
      filePath: anchor.filePath,
      side: anchor.side,
      state: 'placed',
      anchor: { ...anchor, selectedText: quoteTooLong ? '' : quote },
      occurrence: rows.occurrence,
      repeated: anchor.side !== 'file' && !namesOneRow(rows.count),
      suggested: null,
      line: anchorSideLine(anchor),
      reason: null,
      selectedText: quoteTooLong ? '' : quote,
      quoteTooLong,
      guard: anchor.side === 'file' ? null : lineGuard(quote),
      proof: { kind: 'none' },
      shownFile,
      baseFile,
      ...NO_RANGE,
    };
  }

  function placedEditorOn(at: EditorRecord): EditorRecord | undefined {
    const key = anchorKey(at.anchor!);
    return openEditors.find((editor) => editor.subject === shown?.subject && editor.anchor !== null && anchorKey(editor.anchor) === key && editor.occurrence === at.occurrence);
  }

  // Puts `editor` on `anchor` with its id and text, as if opened there now.
  function attachEditor(editor: EditorRecord, anchor: EditorAnchor): boolean {
    const placed = newEditor(anchor, editor.id);
    if (!placed) return false;
    replaceEditor(editor, placed);
    if (armedEditorId === editor.id) armedEditorId = null;
    return true;
  }

  function openDraftEditor(anchor: EditorAnchor, generation?: number): string | null {
    if (generation !== undefined && generation !== shown?.generation) return null;
    const armed = armedEditorId === null ? undefined : openEditors.find((editor) => editor.id === armedEditorId);
    if (armed) {
      if (!attachEditor(armed, anchor)) return null;
      pendingEditorFocusId = armed.id;
      return armed.id;
    }
    const editor = newEditor(anchor);
    if (!editor) return null;
    const existing = placedEditorOn(editor);
    if (existing) {
      pendingEditorFocusId = existing.id;
      lastEditorId = existing.id;
      return existing.id;
    }
    pendingEditorFocusId = editor.id;
    lastEditorId = editor.id;
    openEditors = [...openEditors, editor];
    return editor.id;
  }

  // The editor a shift+click spans: the one last opened, when it is placed
  // in the file clicked. Its first line and last are checked against the
  // file as loaded (utils/reviewRange.ts); a refusal keeps the editor as
  // it was and says why on it.
  function extendEditorRange(anchor: EditorAnchor, generation?: number): string | null {
    if (generation !== undefined && generation !== shown?.generation) return null;
    const editor = openEditors.find((candidate) => candidate.id === lastEditorId);
    if (!editor || editor.state !== 'placed' || editor.anchor === null || editor.subject !== shown?.subject
      || editor.filePath !== anchor.filePath || armedEditorId !== null) {
      return openDraftEditor(anchor, generation);
    }
    const base = loadedFiles.find((file) => file.path === anchor.filePath);
    const shownFile = files.find((file) => file.path === anchor.filePath);
    if (!base || !shownFile) return null;
    const { lineIndex: _lineIndex, ...given } = anchor;
    const origin = editor.rangeOrigin ?? editor.anchor;
    const span = spanRange(base, origin, completeAnchor(shownFile, given));
    if (span.kind === 'refused') {
      replaceEditor(editor, { ...editor, rangeRefusal: RANGE_REFUSAL_TEXT[span.reason] });
      return editor.id;
    }
    const placed = newEditor(span.kind === 'single' ? origin : span.end, editor.id);
    if (!placed) return null;
    replaceEditor(editor, span.kind === 'single' ? placed : { ...placed, rangeStart: span.start, rangeOrigin: origin, range: span.range });
    pendingEditorFocusId = editor.id;
    return editor.id;
  }

  function dropEditorRange(editorId: string): void {
    const editor = openEditors.find((candidate) => candidate.id === editorId);
    if (!editor?.rangeOrigin) return;
    const placed = newEditor(editor.rangeOrigin, editor.id);
    if (placed) replaceEditor(editor, placed);
  }

  function useSuggestedLine(editorId: string, seen: Pick<EditorRow, 'generation' | 'suggested'>): boolean {
    const editor = openEditors.find((candidate) => candidate.id === editorId);
    if (!editor || editor.state !== 'detached' || !editor.suggested || !seen.suggested) return false;
    if (seen.generation !== shown?.generation || anchorKey(seen.suggested) !== anchorKey(editor.suggested)) return false;
    return attachEditor(editor, editor.suggested);
  }

  function moveEditor(editorId: string): void {
    const editor = openEditors.find((candidate) => candidate.id === editorId);
    if (!editor || editor.subject !== shown?.subject) return;
    if (editor.state === 'detached' || (editor.state === 'placed' && editor.repeated)) armedEditorId = editorId;
  }

  function cancelEditorMove(): void {
    armedEditorId = null;
  }

  function closeDraftEditor(editorId: string): void {
    if (commentActions.writes.pending(commentWriteKey.editor(editorId))) return;
    openEditors = openEditors.filter((editor) => editor.id !== editorId);
    draftBodies.delete(editorId);
    if (pendingEditorFocusId === editorId) pendingEditorFocusId = null;
    if (armedEditorId === editorId) armedEditorId = null;
  }

  // The line `editor` saves on, checked against the diff on screen with
  // no await: the editor belongs to it, its line is proven in it, and the
  // row is there. A file shown since through an expansion of the same
  // loaded file is checked again; anything else detaches the editor.
  function saveableAnchor(editor: EditorRecord | null, bundle: ShownDiff): CommentAnchor {
    if (!editor) throw new ReviewCommentRefused('detached', REVIEW_EDITOR_CLOSED);
    if (editor.subject !== bundle.subject) throw new ReviewCommentRefused('subject', REVIEW_EDITOR_OTHER_DIFF);
    if (editor.quoteTooLong) throw new ReviewCommentRefused('too-long', REVIEW_QUOTE_TOO_LONG);
    if (editor.state !== 'placed' || editor.anchor === null) {
      throw new ReviewCommentRefused('detached', editor.state === 'gone' ? REVIEW_EDITOR_GONE : REVIEW_EDITOR_DETACHED);
    }
    const file = files.find((candidate) => candidate.path === editor.filePath);
    if (!file) {
      replaceEditor(editor, detached(editor, 'file-gone'));
      throw new ReviewCommentRefused('detached', REVIEW_EDITOR_GONE);
    }
    if (editor.side === 'file') return editor.anchor;
    const sameLoad = loadedFiles.find((candidate) => candidate.path === editor.filePath) === editor.baseFile;
    const rows = anchorRows(file, editor.anchor);
    if ((file !== editor.shownFile && !sameLoad) || rows.count === 0) {
      replaceEditor(editor, detached(editor, 'line-changed'));
      throw new ReviewCommentRefused('stale', REVIEW_EDITOR_DETACHED);
    }
    // The saved line names only a coordinate: several rows have this one.
    if (!namesOneRow(rows.count)) {
      if (!editor.repeated) replaceEditor(editor, { ...editor, repeated: true });
      throw new ReviewCommentRefused('repeated', REVIEW_EDITOR_REPEATED);
    }
    if (file !== editor.shownFile) replaceEditor(editor, { ...editor, shownFile: file });
    return editor.anchor;
  }

  async function createComment(editorId: string, body: string, mode: 'draft' | 'now' = 'draft'): Promise<void> {
    const trimmed = body.trim();
    const bundle = shown;
    if (!bundle?.sourceKey || !trimmed) return;
    const editor = openEditors.find((candidate) => candidate.id === editorId) ?? null;
    const start = editor?.rangeStart ?? null;
    try {
      let anchor = saveableAnchor(editor, bundle);
      if (bundle.headRange) {
        // A head-ending run saves in the whole diff's numbers, or by head
        // line for the agent (stores/reviewRangeComments.ts); never a
        // line range.
        const saved = rangeSaveAnchor(prLineIndex, anchor, !pinned);
        if (!saved || start) throw new Error(lineCommentReason(prLineIndex, 'none', anchor) ?? 'This line takes no comment here.');
        if (mode === 'now' && saved.target !== 'pr') throw new Error(lineCommentReason(prLineIndex, saved.target, anchor) ?? 'This line takes no comment here.');
        anchor = saved.anchor;
      }
      // Posted from the diff on screen, at the head it was loaded at.
      if (mode === 'now') return await commentActions.postLineComment(editorId, anchor, start, trimmed, () => closeDraftEditor(editorId));
      if (pinnedDrafts !== null && !canDraftComments) throw new Error('Comment on the whole pull request: pick All commits.');
      const { filePath, oldLine, newLine, side } = anchor;
      await saveDraft({
        scope: bundle.scope, sourceKey: bundle.sourceKey, commitSha: bundle.commitSha, filePath, oldLine, newLine, side,
        startOldLine: start?.oldLine, startNewLine: start?.newLine, selectedText: editor?.selectedText ?? '', body: trimmed,
      });
      closeDraftEditor(editorId);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  // A new draft in the pane's comment home: the PR's for a pinned review,
  // else the thread's, whose active comment source it becomes.
  async function saveDraft(input: DiffReviewCommentInput): Promise<void> {
    if (pinnedDrafts !== null) return void await createPRDiffReviewComment(pinnedDrafts, input);
    const owner = commentThreadId();
    await createDiffReviewComment(owner, input);
    setActiveDiffReviewSource(owner, input.scope, input.sourceKey);
  }

  async function updateComment(commentId: string, body: string): Promise<void> {
    const trimmed = body.trim();
    if (!sourceKey || !trimmed) return;
    try {
      if (pinnedDrafts !== null) await updatePRDiffReviewComment(pinnedDrafts, scope, sourceKey, commentId, { body: trimmed });
      else await updateDiffReviewComment(commentThreadId(), scope, sourceKey, commentId, { body: trimmed });
      draftEditBodies.delete(commentId);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  async function deleteComment(commentId: string): Promise<void> {
    if (!sourceKey) return;
    try {
      if (pinnedDrafts !== null) await deletePRDiffReviewComment(pinnedDrafts, scope, sourceKey, commentId);
      else await deleteDiffReviewComment(commentThreadId(), scope, sourceKey, commentId);
      draftEditBodies.delete(commentId);
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    }
  }

  // A sent draft is no longer editable; its open edit goes with it.
  function forgetDraftEdits(sent: readonly Pick<DiffReviewComment, 'id'>[]): void {
    for (const comment of sent) draftEditBodies.delete(comment.id);
  }

  async function sendComments(): Promise<void> {
    if (!sourceKey || drafts.length === 0 || sendingComments || isTurnActive) return;
    sendingComments = true;
    try {
      const thread = commentThreadId();
      const sendScope = scope;
      const key = sourceKey;
      const sending = drafts;
      const pr = await prSendContext(sendScope, sending);
      await SendDiffReviewComments(thread, sendScope, key, sending.map((comment) => comment.id), { pr });
      forgetDraftEdits(sending);
      pruneDraftEdits(sourceKey, await refreshDiffReviewComments(commentThreadId(), scope, sourceKey));
      error = null;
    } catch (err) {
      error = userFacingError(err);
      throw err;
    } finally {
      sendingComments = false;
    }
  }

  // The PR context a send of drafts carries: each comment's hunk. Excerpts
  // can read evicted text again, so everything the send addresses is taken
  // before the first await.
  async function prSendContext(sendScope: ReviewScope, sending: readonly DiffReviewComment[]) {
    const detail = prSnapshot?.detail;
    const shown = files;
    if (sendScope !== 'pr' || !detail) return undefined;
    const threads = prThreads;
    return {
      number: detail.number,
      url: detail.url,
      comments: await Promise.all(sending.map(async (comment) => {
        // A held reply quotes the thread it answers, as the pane shows it.
        if (comment.side === 'reply') return { commentId: comment.id, hunkExcerpt: '', thread: commentActions.replyContext(comment, threads) };
        return { commentId: comment.id, hunkExcerpt: await hunkExcerptForComment(shown, comment) };
      })),
    };
  }

  async function sendDraftsToThread(target: string): Promise<Thread | null> {
    if (!pinnedDrafts) throw new Error('Only a review pinned to a pull request sends its drafts to a thread.');
    if (scope !== 'pr' || (selectedCommitSHA && !headRange)) throw new Error('Send the whole pull request\'s comments: pick All commits.');
    if (!sourceKey || drafts.length === 0 || sendingComments) return null;
    sendingComments = true;
    try {
      const key = sourceKey;
      const sending = drafts;
      const pr = await prSendContext('pr', sending);
      const thread = await sendPRDiffReviewComments(pinnedDrafts, target, 'pr', key, sending.map((comment) => comment.id), { pr });
      forgetDraftEdits(sending);
      error = null;
      return thread;
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
    if (scope !== 'pr' || (selectedCommitSHA && !headRange) || !prRef || !sourceKey || sendingComments) return;
    if (openSubmit !== null) {
      const lost = openSubmit;
      sendingComments = true;
      try {
        await resolveLostSubmit(lost);
      } finally {
        sendingComments = false;
      }
      return;
    }
    // From a head-ending run, the drafts the whole diff has no row for
    // (agent-only ones) stay out, as orphans do from All commits.
    if (headRange && prLineIndex.state !== 'ready') {
      submitError = 'The pull request\'s diff is still loading. Send the review once it has loaded.';
      return;
    }
    const orphaned = headRange ? rangePROrphans(prLineIndex, drafts) : orphanedDraftIds();
    const submitDrafts = drafts.filter((comment) => !orphaned.has(comment.id));
    // A bare Approve is a valid review; comment and request-changes need
    // content (GitHub's API rejects those events without a body).
    if (submitDrafts.length === 0 && !summaryBody.trim() && verdict !== 'approve') {
      submitError = 'No non-orphaned PR comments to submit.';
      return;
    }
    const withDrafts = reviewDraftSubmit(backend);
    const attempt: SubmitAttempt = {
      id: withDrafts ? randomId() : '',
      launchId: getTransportHelloFor(backend)?.launchId ?? null,
      ref: prRef,
      scope,
      source: sourceKey,
      threadId: pinnedDrafts !== null ? null : commentThreadId(),
      ids: submitDrafts.map((comment) => comment.id),
      body: summaryBody.trim(),
      verdict,
    };
    sendingComments = true;
    submitError = null;
    let failure: unknown = null;
    try {
      const result = withDrafts
        ? await submitReviewWithDrafts(attempt)
        : await submitReviewThenMark(attempt, submitDrafts);
      await settleSubmit(attempt, result.unconfirmed ? { kind: 'unconfirmed', message: REVIEW_UNCONFIRMED } : { kind: 'posted', result });
    } catch (err) {
      failure = err;
      if (isTransportClassError(err)) {
        openSubmit = attempt;
        await resolveLostSubmit(attempt);
      } else {
        const code = err instanceof TransportError ? err.code : undefined;
        await settleSubmit(attempt, { kind: 'not-posted', message: notPostedMessage(code, userFacingError(err)) });
      }
    } finally {
      sendingComments = false;
    }
    if (failure !== null) throw failure;
  }

  // A backend with the draft submit posts the drafts it holds and marks
  // the ones the forge received, in one call (SubmitPRReview).
  async function submitReviewWithDrafts(attempt: SubmitAttempt): Promise<SubmitPRReviewResult> {
    return (await withBackendTarget(backend, () => SubmitPRReview(prReferenceWire(attempt.ref), {
      verdict: attempt.verdict,
      body: attempt.body,
      comments: [],
      drafts: { threadId: attempt.threadId ?? undefined, ids: [...attempt.ids], headSha: loadedPRHeadSHA },
      attemptId: attempt.id,
    }))) as SubmitPRReviewResult;
  }

  // A backend without it (an older build on a connected computer) posts
  // the comments it is given; the pane marks the drafts that posted. The
  // review is on the PR once SubmitPRReview answers, so a failed mark is a
  // posted review with a partial failure, never a failed submit.
  async function submitReviewThenMark(attempt: SubmitAttempt, submitDrafts: readonly DiffReviewComment[]): Promise<SubmitPRReviewResult> {
    const result = (await withBackendTarget(backend, () => SubmitPRReview(prReferenceWire(attempt.ref), {
      verdict: attempt.verdict,
      body: attempt.body,
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
    }
    forgetDraftEdits(sent);
    if (sent.length > 0) {
      const ids = sent.map((comment) => comment.id);
      try {
        if (pinnedDrafts !== null) await markPRDiffReviewCommentsSent(pinnedDrafts, attempt.scope, attempt.source, ids);
        else if (attempt.threadId !== null) await MarkDiffReviewCommentsSent(attempt.threadId, attempt.scope, attempt.source, ids, `pr:${loadedPRHeadSHA}`);
      } catch (err) {
        const unmarked = `Its comments were not marked sent and still show as drafts; delete them instead of sending them again. ${userFacingError(err)}`;
        return {
          ...result,
          posted: true,
          partialFailure: result.partialFailure ? `${result.partialFailure} ${unmarked}` : unmarked,
          partialFailureKind: result.partialFailure ? result.partialFailureKind : 'unmarked',
        };
      }
    }
    // An answered submit without drafts posted what it was given.
    return { ...result, posted: true };
  }

  // A lost answer is settled by asking the backend what the attempt did.
  // Unknown to the process the submit went to, it never arrived; unknown
  // to a restarted one, or forgotten by the same one, nothing says. A
  // backend without the draft submit keeps no record, so there is nothing
  // to ask. A failed ask keeps the attempt open: the next press of Send
  // asks again.
  async function resolveLostSubmit(attempt: SubmitAttempt): Promise<void> {
    if (!attempt.id) {
      await settleSubmit(attempt, { kind: 'unconfirmed', message: REVIEW_ANSWER_LOST });
      return;
    }
    let outcome: PRReviewSubmitOutcome;
    try {
      outcome = (await withBackendTarget(backend, () => GetPRReviewSubmitOutcome(prReferenceWire(attempt.ref), attempt.id))) as PRReviewSubmitOutcome;
    } catch (err) {
      submitError = `${REVIEW_ANSWER_LOST} The computer could not be asked what happened, so Send asks again: ${userFacingError(err)}`;
      return;
    }
    if (outcome.state === 'posted' && outcome.result) {
      await settleSubmit(attempt, { kind: 'posted', result: outcome.result });
    } else if (outcome.state === 'unconfirmed') {
      await settleSubmit(attempt, { kind: 'unconfirmed', message: REVIEW_UNCONFIRMED });
    } else if (outcome.state === 'failed') {
      await settleSubmit(attempt, { kind: 'not-posted', message: notPostedMessage(outcome.code, outcome.message ?? 'The review was not posted.') });
    } else if (outcome.state === 'forgotten') {
      await settleSubmit(attempt, { kind: 'unconfirmed', message: REVIEW_OUTCOME_FORGOTTEN });
    } else if (outcome.state === 'unknown' && attempt.launchId !== null && getTransportHelloFor(backend)?.launchId === attempt.launchId) {
      await settleSubmit(attempt, { kind: 'not-posted', message: REVIEW_NEVER_ARRIVED });
    } else {
      await settleSubmit(attempt, { kind: 'unconfirmed', message: REVIEW_ANSWER_LOST });
    }
  }

  // The summary a submit posted leaves the box, unless it was changed
  // since the submit took it; so does its verdict.
  function clearSubmittedSummary(attempt: SubmitAttempt): void {
    if (summaryBody.trim() === attempt.body) summaryBody = '';
  }

  // Ends an attempt with what it did. Only a posted review clears the
  // summary; a review that was not posted, or whose outcome nothing can
  // say, keeps the summary and verdict for the person to decide. The
  // drafts and threads are read again only to show the backend's state.
  async function settleSubmit(attempt: SubmitAttempt, settlement: SubmitSettlement): Promise<void> {
    if (openSubmit === attempt) openSubmit = null;
    if (settlement.kind === 'posted') {
      const result = settlement.result;
      if (result.partialFailurePath) {
        submitError = `The review posted, but its comment on ${result.partialFailurePath} did not. ${result.partialFailure ?? 'Unknown error.'}`;
      } else if (result.partialFailureThreadId) {
        // The reply stays a draft at its thread's foot.
        submitError = `The review posted, but a held reply did not. ${result.partialFailure ?? 'Unknown error.'}`;
      } else if (result.partialFailure) {
        submitError = `The review posted, but a step after it did not. ${result.partialFailure}`;
      } else if (result.editedDraftIds?.length) {
        submitError = `The review posted. ${result.editedDraftIds.length === 1 ? 'A comment was' : `${result.editedDraftIds.length} comments were`} edited while it posted and ${result.editedDraftIds.length === 1 ? 'stays a draft' : 'stay drafts'}; the review has the text from before the edit.`;
      } else {
        submitError = null;
      }
      if (result.posted && (!result.partialFailure || result.partialFailureKind === 'unmarked')) {
        clearSubmittedSummary(attempt);
        if (verdict === attempt.verdict) verdict = 'comment';
      }
      forgetDraftEdits((result.sentDraftIds ?? []).map((id) => ({ id })));
      error = null;
    } else {
      submitError = settlement.message;
      error = submitError;
    }
    try {
      const read = attempt.threadId === null
        ? await refreshPRDiffReviewComments(pinnedDrafts!, attempt.scope, attempt.source)
        : await refreshDiffReviewComments(attempt.threadId, attempt.scope, attempt.source);
      pruneDraftEdits(attempt.source, read);
      // Through the store: a posted review is now part of the PR, so
      // every pane looking at it shows the new threads.
      if (settlement.kind === 'posted') {
        applyPRThreads(computerPRKey(attempt.ref), ((await withBackendTarget(backend, () => ListPRReviewThreads(prReferenceWire(attempt.ref)))) ?? []) as ReviewThread[]);
      }
    } catch (err) {
      error = userFacingError(err);
    }
  }

  // Comments-only refresh: PR detail + review threads, WITHOUT touching
  // the diff (they are fetched by separate calls, so this never reloads
  // or re-renders the patch). Applied through the store, so a head that
  // moved raises the stale banner on every pane whose diff predates it —
  // and on none whose diff doesn't. The diff never swaps mid-read.
  // A refresh asked for while one runs is owed, not dropped: the running
  // read may predate what the later ask wants to see (a merge failure
  // during a manual refresh). One read at a time; the owed one runs next.
  let refreshPRDataOwed = false;
  async function refreshPRThreads(): Promise<void> {
    if (scope !== 'pr' || !prRef) return;
    if (refreshingPRData) {
      refreshPRDataOwed = true;
      return;
    }
    refreshingPRData = true;
    try {
      do {
        refreshPRDataOwed = false;
        await readPRThreads(prRef);
      } while (refreshPRDataOwed && !disposed && scope === 'pr' && prRef !== null);
    } finally {
      refreshPRDataOwed = false;
      refreshingPRData = false;
    }
  }

  async function readPRThreads(ref: PRRef): Promise<void> {
    const key = computerPRKey(ref);
    try {
      const pr = prReferenceWire(ref);
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
  // Merge section actions
  // ------------------------------------------------------------------

  // Every merge action is one shape: refuse while one is pending (in
  // flight or waiting for the pump's answer), call the forge through this
  // PR's computer, and on failure toast the one sentence the backend
  // reviewed (the forge's own text is in Details) and re-read the PR so a
  // head that moved raises the stale banner. On success the backend has
  // already had the pump poll and answers with the pump's seq after it;
  // the action stays pending until this PR's watermark reaches that seq,
  // so the button never flips back to its old label before the forge's
  // answer is on screen. A poll that found the snapshot unchanged stamped
  // nothing, so the watermark is already there and the old label is the
  // forge's answer.
  async function runMergeAction(kind: MergeActionKind, call: () => Promise<{ seq: number }>): Promise<void> {
    if (scope !== 'pr' || !prRef || mergeActionShown !== null || mergeWithheldReason !== null) return;
    const ref = prRef;
    const key = prEntityKey;
    // The seq the answer names is this backend process's; a restart (a
    // new launch) starts a pump that numbers from one, whose snapshot is
    // the forge's answer.
    const launch = backendLaunch;
    mergeAction = kind;
    mergeAwait = null;
    let result: { seq: number };
    try {
      result = await withBackendTarget(backend, call);
    } catch (err) {
      if (disposed) return;
      mergeAction = null;
      const action = mergeFailureAction(err);
      // A failure with a fix to press stays until pressed or dismissed.
      addErrorToast(`${MERGE_ACTION_LABEL[kind]} failed: ${userFacingError(err)}`, err, action ? 0 : undefined, action);
      if (prRef === ref) void refreshPRThreads();
      return;
    }
    if (disposed) return;
    mergeAction = null;
    if (key === null || backendLaunch !== launch || peekPRSeq(key) >= result.seq) return;
    mergeAwait = { kind, key, seq: result.seq, launch };
  }

  // A head that moved under the action is fixed by reloading the diff;
  // the backend names that case by its code (git.ForgeErrorStaleHead).
  function mergeFailureAction(err: unknown): { label: string; run: () => void } | undefined {
    if (err instanceof TransportError && err.code === 'forge_stale_head') {
      return { label: 'Reload', run: () => { void reload({ userInitiated: true }); } };
    }
    return undefined;
  }

  function approvePR(): Promise<void> {
    const ref = prRef;
    const head = mergeHeadSHA;
    if (!ref) return Promise.resolve();
    return runMergeAction('approve', () => ApprovePR(prReferenceWire(ref), head));
  }

  function unapprovePR(): Promise<void> {
    const ref = prRef;
    if (!ref) return Promise.resolve();
    return runMergeAction('unapprove', () => UnapprovePR(prReferenceWire(ref)));
  }

  function mergePR(choice: MergePRChoice): Promise<void> {
    const ref = prRef;
    const head = mergeHeadSHA;
    // GitHub's auto-merge has no branch delete; the repository's own
    // setting decides, so the toggle is not sent with it.
    const deleteBranch = choice.autoMerge && ref?.forge === 'github' ? false : choice.deleteBranch;
    if (!ref) return Promise.resolve();
    return runMergeAction(choice.autoMerge ? 'auto_merge' : 'merge', async () => {
      const result = await MergePR(prReferenceWire(ref), {
        method: choice.method,
        headSHA: head,
        deleteBranch,
        autoMerge: choice.autoMerge,
        ...(choice.commitMessage ? { commitMessage: choice.commitMessage } : {}),
      });
      const outcome = result.outcome as MergeOutcome;
      // The forge took the merge but has not finished it (GitLab merges
      // asynchronously): say so, since the pane shows it open until the
      // frame with `merged` lands.
      if (!choice.autoMerge && !outcome.merged && !outcome.autoMerge) addToast('info', 'Merging…');
      return result;
    });
  }

  function cancelPRAutoMerge(): Promise<void> {
    const ref = prRef;
    if (!ref) return Promise.resolve();
    return runMergeAction('cancel_auto_merge', () => CancelPRAutoMerge(prReferenceWire(ref)));
  }

  function updatePRBranch(): Promise<void> {
    const ref = prRef;
    const head = mergeHeadSHA;
    if (!ref) return Promise.resolve();
    return runMergeAction('update_branch', () => UpdatePRBranch(prReferenceWire(ref), head));
  }

  function markPRReady(): Promise<void> {
    const ref = prRef;
    if (!ref) return Promise.resolve();
    return runMergeAction('mark_ready', () => MarkPRReady(prReferenceWire(ref)));
  }

  // ------------------------------------------------------------------
  // Conversation section
  // ------------------------------------------------------------------

  function threadSettled(thread: ReviewThread): boolean {
    return thread.isResolvable && thread.isResolved;
  }

  // Captures the feed order (chronological, newest first) and the
  // replies-unfolded-by-default set from the entries in hand.
  // `preserveExpanded` keeps folds that were already open open — a reveal
  // must not fold a thread's replies away because it was remotely
  // resolved while the reader had them open.
  // The viewer's own PR comment joins a frozen feed at its end, as it was
  // posted from there; nothing else that arrived meanwhile does. It is the
  // PR-level entry whose comment is the one the post's answer named. A
  // feed not frozen yet shows it in its live order.
  function joinPostedComment(commentId: string): void {
    if (conversationOrder.length === 0) return;
    const known = new Set(conversationOrder);
    const mine = conversationFeedSource.find(({ item }) => item.kind === 'thread' && !known.has(item.id)
      && !item.thread.isResolvable && item.thread.comments[0]?.id === commentId);
    if (mine) conversationOrder = [...conversationOrder, mine.item.id];
  }

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
    // The jump decides where the section is scrolled. A remembered
    // reading offset is restored a tick after the overview row remounts
    // (ReviewResizableBody), which would land on top of the jump's scroll.
    overviewScrollTops.delete('conversation');
    pendingConversationThreadId = prThreadId;
  }

  // The overview row carries the sections; leave any replacement view
  // first, the way every diff-surface jump does.
  function jumpToOverview(section?: ReviewSectionId): void {
    closeCILogView();
    closeConflictView();
    if (section === 'merge') mergeOpen = true;
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
    revealFile(thread.path);
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
  // in this checkout (every pane there shows the latest read, and each
  // open reads again against the base branch's live tip); what this pane
  // owns is whether the surface is showing and which files it has
  // collapsed.
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
    // The open is recorded before the permit: a permit wakes the runner,
    // which would otherwise start this read unattended for the person
    // asking for it.
    const opening = openPRConflicts(key, workspace, prRef, detail);
    setConflictView(true);
    conflictExpandedFolds.clear();
    await opening;
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
    await ci.refresh();
  }

  function openCIJobLog(stageName: string, job: CIJob): void {
    if (!prEntityKey || !job.logsAvailable || !job.id) return;
    // The log view and the conflict view both replace the diff body.
    setConflictView(false);
    ci.openJobLog(stageName, job);
  }

  function closeCILogView(): void {
    ci.closeLogView();
  }

  async function sendCILogToChat(): Promise<void> {
    const open = ci.opened;
    const opened = ci.logView;
    if (!prRef || !open || !opened) return;
    // The owner pane of a pinned review is not its chat, whatever it holds.
    if (pinned) {
      ci.setLogError(PINNED_NO_CHAT);
      return;
    }
    const path = await ci.saveLog();
    if (!path) return;
    // The job's status as of the send, while its log is still the one open.
    const view = (ci.opened === open ? ci.logView : null) ?? opened;
    const draft = getComposerDraftForPane(sourcePaneId);
    if (!draft) {
      ci.setLogError('The source chat pane is not available.');
      return;
    }
    const message = [
      `Investigate CI job \`${view.job.name}\` (${view.stageName}) on PR #${prRef.number}, status: ${view.job.status}.`,
      `Full log saved at: ${path}`,
    ].join('\n');
    const existing = draft.content.trim();
    draft.setContent(existing ? `${existing}\n\n${message}` : message);
  }

  async function sendCILogSectionToChat(section: CILogSection): Promise<void> {
    const open = ci.opened;
    const view = ci.logView;
    const ref = prRef;
    if (!ref || !open || !view) return;
    if (pinned) {
      ci.setLogError(PINNED_NO_CHAT);
      return;
    }
    if (!getComposerDraftForPane(sourcePaneId)) {
      ci.setLogError('The source chat pane is not available.');
      return;
    }
    const result = CI_RESULT_STATUSES.has(section.status) ? ` (${section.status})` : '';
    const lines = [
      `Investigate \`${section.name}\`${result} in CI job \`${view.job.name}\` (${view.stageName}) on PR #${ref.number}, status: ${view.job.status}.`,
    ];
    if (section.bytes <= CI_SECTION_INLINE_MAX_BYTES) {
      let text: string;
      try {
        text = await ci.sectionText(section);
      } catch (err) {
        ci.setLogError(userFacingError(err), open);
        return;
      }
      const fence = codeFence(text);
      lines.push(fence, text, fence);
    } else {
      const path = await ci.saveSection(section);
      if (!path) return;
      lines.push(`Section log saved at: ${path}`);
    }
    // The draft as of now: the read or save may have outlived the pane.
    const draft = getComposerDraftForPane(sourcePaneId);
    if (!draft) {
      ci.setLogError('The source chat pane is not available.', open);
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
    // A head-ending run places the PR's drafts by head line, loosely.
    for (const comment of shownDrafts) {
      if (comment.side === 'reply') continue;
      const anchor = { side: comment.side, oldLine: comment.oldLine, newLine: comment.newLine };
      if (headRange ? !files.some((file) => file.path === comment.filePath && anchorRow(file, anchor, true) >= 0) : !draftAnchorExists(files, comment)) out.add(comment.id);
    }
    for (const comment of commentActions.state.orphanedReplyDrafts) out.add(comment.id);
    return out;
  });

  const rangeTints = $derived(rangesByFile(shownEditors, shownDrafts, prThreads, (id) => expandedPRThreadIds.has(id), files));

  function orphanedDraftIds(): SvelteSet<string> {
    return orphanedIds;
  }

  const own = {
    identity,
    rowId,
    get scope() { return scope; },
    pinned: pinned !== null,
    get commentsGranted() { return commentsGranted; },
    get canDraftComments() { return canDraftComments; },
    backend,
    get baseBranch() { return baseBranch; },
    get prRef() { return prRef; },
    get prScopeLabel() { return prRef ? prScopeLabel(prRef) : null; },
    get sourceKey() { return sourceKey; },
    get files() { return files; },
    get comments() { return comments; },
    get drafts() { return drafts; },
    get openEditors() { return shownEditors; },
    get armedEditorId() { return armedEditorId; },
    get shownGeneration() { return shown?.generation ?? 0; },
    get commits() { return commits; },
    get commitsTotal() { return commitsTotal; },
    get selectedCommitSHA() { return selectedCommitSHA; },
    get selectedCommits() { return selectedCommits; },
    get headRange() { return headRange; },
    get commitSelectionLost() { return commitSelectionLost; },
    dismissCommitSelectionLost(): void { commitSelectionLost = false; },
    get prDiffIndexState() { return prLineIndex.state; },
    mediaHost,
    get prDiffIndexError() { return prLineIndex.error; },
    retryPRDiffIndex: () => prLineIndex.retry(),
    lineComment,
    get shownDrafts() { return shownDrafts; },
    get edits() { return edits; },
    get editTurnLabels() { return editTurnLabels; },
    get selectedEditKey() { return editSelectionKey(selectedEdit); },
    get pendingEditItemID() { return pendingEditItemID; },
    set pendingEditItemID(value: string | null) { pendingEditItemID = value; },
    get pendingJumpFilePath() { return pendingJumpFilePath; },
    get revealedPaths() { return revealedPaths; },
    requestFileJump,
    clearRevealedPaths(): void { revealedPaths.clear(); },
    get pendingJumpRowKey() { return pendingJumpRowKey; },
    get loading() { return loading; },
    get error() { return error; },
    get sendingComments() { return sendingComments; },
    get sendStripShown() { return sendStripShown; },
    get prDetail() { return prSnapshot?.detail ?? null; },
    get prThreads() { return prThreads; },
    get prEvents() { return prEvents; },
    get mergeOpen() { return mergeOpen; },
    setMergeOpen(open: boolean): void { mergeOpen = open; },
    get mergeAction() { return mergeActionShown; },
    get mergeWithheldReason() { return mergeWithheldReason; },
    approvePR,
    unapprovePR,
    mergePR,
    cancelPRAutoMerge,
    updatePRBranch,
    markPRReady,
    manage,
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
    conflictFoldLines,
    paintedSpans,
    conflictPaintedSpans,
    get ciPipeline() { return ci.pipeline; },
    get ciLoading() { return ci.loading; },
    get ciRefreshing() { return ci.refreshing; },
    get ciError() { return ci.error; },
    get ciFailure() { return ci.failure; },
    get ciLogView() { return ci.logView; },
    get ciLog() { return ci.log; },
    get ciLogLoading() { return ci.logLoading; },
    get ciLogError() { return ci.logError; },
    get ciLogFailure() { return ci.logFailure; },
    get ciLogSavedPath() { return ci.logSavedPath; },
    get ciLogRetriedFrom() { return ci.logRetriedFrom; },
    get ciActions() { return ci.actions; },
    get ciLogOpenSections() { return ci.logOpenSections; },
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
    selectCommits,
    showChangesFrom,
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
    useSuggestedLine,
    moveEditor,
    cancelEditorMove,
    draftBodyFor(editorId: string): string {
      return draftBodies.get(editorId) ?? '';
    },
    setDraftBody(editorId: string, body: string): void {
      if (openEditors.some((editor) => editor.id === editorId)) draftBodies.set(editorId, body);
    },
    consumeDraftEditorFocus(editorId: string): boolean {
      if (pendingEditorFocusId !== editorId) return false;
      pendingEditorFocusId = null;
      return true;
    },
    createComment,
    draftEditBodyFor(commentId: string): string | null {
      return draftEditBodies.get(commentId)?.body ?? null;
    },
    setDraftEditBody(commentId: string, body: string | null): void {
      if (body === null) draftEditBodies.delete(commentId);
      else draftEditBodies.set(commentId, { sourceKey, body });
    },
    updateComment,
    deleteComment,
    sendComments,
    sendDraftsToThread,
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
    refreshPRThreads,
    extendEditorRange,
    dropEditorRange,
    rangeTintsFor(path: string): readonly LineRange[] {
      return rangeTints.get(path) ?? NO_RANGES;
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
    get conversationCount() { return conversationCount; },
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
    attachSurface,
    toggleConflictCollapsed,
    expandConflictFold,
    refreshCI,
    openCIJobLog,
    closeCILogView,
    refreshCILog: () => ci.refreshLog(),
    saveCILog: () => ci.saveLog(),
    sendCILogToChat,
    toggleCILogSection: (sectionKey: string) => ci.toggleSection(sectionKey),
    setCILogSectionsOpen: (sectionKeys: readonly string[], open: boolean) => ci.setSectionsOpen(sectionKeys, open),
    readCILogLines: (from: number, count: number) => ci.readLines(from, count),
    searchCILog: (query: string, limit: number) => ci.searchLog(query, limit),
    copyCILogSection: (section: CILogSection) => ci.copySection(section),
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
  } satisfies Omit<ReviewPaneState, keyof ReviewCommentActionsState | keyof ReviewCommentExtrasState>;
  // Own properties, getters intact: a spread would read each getter once.
  return Object.defineProperties({} as ReviewPaneState, {
    ...Object.getOwnPropertyDescriptors(own),
    ...Object.getOwnPropertyDescriptors(commentActions.state),
    ...Object.getOwnPropertyDescriptors(extras.state),
  });
}

function userFacingError(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'Review diff failed.';
}

export function __resetReviewPaneStateForTest(): void {
  for (const state of statesBySourcePane.values()) state.dispose();
  statesBySourcePane.clear();
  heldPRByRow.clear();
  resetPRDiffReviewCommentsForTest();
}

export type { CommentAnchor };
export type { DiffReviewComment };
