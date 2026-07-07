import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import {
  GetBranchBaseDiff,
  GetGitStatus,
  GetMessageCheckpointDiff,
  GetMergeConflictFile,
  GetPRCIJobLog,
  GetPRCIJobs,
  GetPRDiff,
  GetPRMergeConflicts,
  GetThread,
  SavePRCIJobLog,
  GetSessionAgentDiff,
  GetWorkspaceCurrentDiff,
  GitListBranches,
  ListThreadCheckpoints,
  ListPRReviewThreads,
  MarkDiffReviewCommentsSent,
  ReplyToPRThread,
  SendMessage,
  SendDiffReviewComments,
  SubmitPRReview,
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
import type { Checkpoint } from '../types/checkpoint';
import type { GitBranch } from '../types/git';
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
import { conflictPatchFile } from '../utils/conflictFile';
import { hunkExcerptForComment } from '../utils/prHunkExcerpt';
import { prRefFromThread, prRefFromUrl, prScopeLabel, type PRRef } from '../utils/prReference';
import {
  buildPatchDisplayRows,
  parsePatchFilesCached,
  type PatchFile,
} from '../utils/patchFiles';
import { anchorKey, type CommentAnchor } from '../utils/reviewRows';
import type { CommentListItem } from '../utils/reviewComments';

export type ReviewScope = DiffReviewScope;

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
  readonly checkpoints: readonly Checkpoint[];
  selectedCheckpointUserItemId: string | null;
  pendingJumpFilePath: string | null;
  /** Diff row key to jump to (comments-list click); consumed by the diff body. */
  readonly pendingJumpRowKey: string | null;
  readonly loading: boolean;
  readonly error: string | null;
  readonly sendingComments: boolean;
  readonly prDetail: PRDetail | null;
  readonly prThreads: readonly ReviewThread[];
  readonly prHeadSHA: string;
  readonly prStale: boolean;
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
  selectCheckpoint(userItemId: string | null): Promise<void>;
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
}

function unregisterPRReviewState(subscriptionId: string): void {
  prStatesBySubscription.delete(subscriptionId);
}

export function applyPRUpdatedEvent(event: PRUpdatedEvent): void {
  prStatesBySubscription.get(event.subscriptionId)?.applyPRUpdate(event);
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
    checkpointUserItemId?: string | null;
    filePath?: string;
  } = {},
): Promise<ReviewPaneState | null> {
  const companion = openCompanion(sourcePaneId, 'review');
  if (!companion) return null;
  const state = reviewStateForPane(sourcePaneId, threadId);
  if (opts.filePath) {
    state.pendingJumpFilePath = opts.filePath;
  }
  if (opts.scope) {
    if (opts.scope === 'turn') {
      state.selectedCheckpointUserItemId = opts.checkpointUserItemId ?? null;
      await state.setScope('turn');
    } else {
      await state.setScope(opts.scope);
    }
  } else if (opts.checkpointUserItemId !== undefined) {
    await state.selectCheckpoint(opts.checkpointUserItemId);
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
  let selectedCheckpointUserItemId: string | null = $state(null);
  let checkpoints: Checkpoint[] = $state([]);
  let pendingJumpFilePath: string | null = $state(null);
  let pendingJumpRowKey: string | null = $state(null);
  let patchText = $state('');
  let loading = $state(false);
  let error: string | null = $state(null);
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
  let viewMode: 'stacked' | 'split' = $state('stacked');
  let wordWrap = $state(getSettings().diffWordWrap);
  let loadSeq = 0;
  const sourceKey = $derived.by(() => {
    if (scope === 'pr' && prRef) {
      // Stable across PR head movement: drafts must survive pushes; each
      // draft's commitSha records the head SHA it was anchored to.
      return `pr:${prRef.forge}:${prRef.namespace}/${prRef.repo}:${prRef.number}`;
    }
    return patchText ? diffSourceKey(patchText) : '';
  });
  const files = $derived(parsePatchFilesCached(patchText));
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
    if (nextScope !== scope) {
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
    if (nextScope !== 'turn') {
      selectedCheckpointUserItemId = null;
    }
    openEditors = [];
    draftBodies.clear();
    persistScope(threadId, scope, baseBranch);
    await reload();
  }

  async function selectCheckpoint(userItemId: string | null): Promise<void> {
    selectedCheckpointUserItemId = userItemId;
    resetConflictState();
    if (scope === 'pr') await unsubscribePR();
    if (scope !== 'turn') scope = 'turn';
    persistScope(threadId, scope, baseBranch);
    await reload();
  }

  async function reload(): Promise<void> {
    const seq = loadSeq + 1;
    loadSeq = seq;
    loading = true;
    error = null;
    // Unconditionally, not just in pr scope: scope can change mid-load
    // (the selector stays enabled while a PR loads), and an in-flight
    // pr load that resolved during setScope's awaits may have
    // registered a subscription after the scope flipped. No-op when
    // none is held.
    await unsubscribePR();
    try {
      if (scope === 'pr' && !prRef) {
        // Persisted 'pr' scope restores before the thread/git status is at
        // hand; resolve the reference here instead of failing the load.
        await ensurePRRef();
      } else {
        // Fire-and-forget: a PR opened after this pane mounted becomes
        // selectable on the next reload without blocking the diff load.
        probePRRef();
      }
      const loaded = await loadPatch(
        threadId,
        scope,
        baseBranch,
        selectedCheckpointUserItemId,
        prRef,
      );
      if (seq !== loadSeq || disposed) {
        // A newer load or dispose superseded this one — the subscription it
        // opened has no owner, so close it before dropping the result.
        if (loaded.subscriptionId) void UnsubscribePRUpdates(loaded.subscriptionId);
        return;
      }
      if (loaded.checkpoints) checkpoints = loaded.checkpoints;
      if (loaded.selectedCheckpointUserItemId !== undefined) {
        selectedCheckpointUserItemId = loaded.selectedCheckpointUserItemId;
      }
      if (loaded.prDetail) prDetail = loaded.prDetail;
      if (loaded.prThreads) prThreads = loaded.prThreads;
      if (loaded.prHeadSHA !== undefined) prHeadSHA = loaded.prHeadSHA;
      if (loaded.subscriptionId) {
        subscriptionId = loaded.subscriptionId;
        registerPRReviewState(subscriptionId, { applyPRUpdate });
      }
      patchText = loaded.patchText;
      collapsedPaths = defaultCollapsedPaths(parsePatchFilesCached(loaded.patchText));
      if (scope === 'pr') {
        prStale = false;
        void loadCIJobs();
      }
      const nextSourceKey = scope === 'pr'
        ? sourceKey
        : (loaded.patchText ? diffSourceKey(loaded.patchText) : '');
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
        commitSha: scope === 'pr' ? prHeadSHA : undefined,
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
    if (scope !== 'pr' || !prRef || !sourceKey || sendingComments) return;
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
    } else {
      for (const file of files) collapsedPaths.add(file.path);
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
    get checkpoints() { return checkpoints; },
    get selectedCheckpointUserItemId() { return selectedCheckpointUserItemId; },
    set selectedCheckpointUserItemId(value: string | null) { selectedCheckpointUserItemId = value; },
    get pendingJumpFilePath() { return pendingJumpFilePath; },
    set pendingJumpFilePath(value: string | null) { pendingJumpFilePath = value; },
    get pendingJumpRowKey() { return pendingJumpRowKey; },
    get loading() { return loading; },
    get error() { return error; },
    get sendingComments() { return sendingComments; },
    get prDetail() { return prDetail; },
    get prThreads() { return prThreads; },
    get prHeadSHA() { return prHeadSHA; },
    get prStale() { return prStale; },
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
    selectCheckpoint,
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
    toggleCollapsed(path: string): void {
      if (collapsedPaths.has(path)) collapsedPaths.delete(path);
      else collapsedPaths.add(path);
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
  checkpoints?: Checkpoint[];
  selectedCheckpointUserItemId?: string | null;
  prDetail?: PRDetail;
  prThreads?: ReviewThread[];
  prHeadSHA?: string;
  subscriptionId?: string;
}

async function loadPatch(
  threadId: string,
  scope: ReviewScope,
  baseBranch: string | null,
  selectedCheckpointUserItemId: string | null,
  prRef: PRRef | null,
): Promise<LoadedPatch> {
  switch (scope) {
    case 'pr': {
      if (!prRef) throw new Error('No PR or MR is available for this thread.');
      return loadPRPatch(threadId, prRef);
    }
    case 'turn': {
      const checkpoints = sortedCheckpoints(
        ((await ListThreadCheckpoints(threadId)) ?? []) as Checkpoint[],
      );
      const selected = selectedCheckpointUserItemId
        ? checkpoints.find((checkpoint) => checkpoint.userItemId === selectedCheckpointUserItemId) ?? null
        : null;
      const checkpoint = selected ?? latestCheckpoint(checkpoints);
      if (!checkpoint) {
        return { patchText: '', checkpoints, selectedCheckpointUserItemId: null };
      }
      return {
        patchText: ((await GetMessageCheckpointDiff(threadId, checkpoint.userItemId)) ?? '') as string,
        checkpoints,
        selectedCheckpointUserItemId: selected ? selected.userItemId : null,
      };
    }
    case 'session':
      return { patchText: ((await GetSessionAgentDiff(threadId)) ?? '') as string };
    case 'workspace':
      return { patchText: ((await GetWorkspaceCurrentDiff(threadId)) ?? '') as string };
    case 'branch': {
      const branch = baseBranch?.trim() || await defaultBaseBranch(threadId);
      return { patchText: ((await GetBranchBaseDiff(threadId, branch)) ?? '') as string };
    }
  }
}

async function loadPRPatch(threadId: string, ref: PRRef): Promise<LoadedPatch> {
  const pr = prReference(ref);
  // The subscription resolves the PR detail, whose baseRefName the diff
  // needs to compute a local three-dot diff (gh/glab's PR-diff API caps at
  // 20k lines; large PRs must go through the local-clone path). Sequenced,
  // not parallel — the base ref is only known once the detail lands.
  const subResult = await SubscribePRUpdates(threadId, pr);
  try {
    const detail = subResult.detail as PRDetail;
    const patchText = String((await GetPRDiff(threadId, pr, detail?.baseRefName ?? '')) ?? '');
    return {
      patchText,
      prDetail: detail,
      prThreads: (subResult.threads ?? []) as ReviewThread[],
      prHeadSHA: String(subResult.headSHA ?? detail?.headSHA ?? ''),
      subscriptionId: String(subResult.id),
    };
  } catch (err) {
    await UnsubscribePRUpdates(String(subResult.id ?? ''));
    throw err;
  }
}

function prReference(ref: PRRef): Parameters<typeof GetPRDiff>[1] {
  return {
    Forge: ref.forge,
    Namespace: ref.namespace,
    Repo: ref.repo,
    Number: ref.number,
  };
}

function sortedCheckpoints(checkpoints: readonly Checkpoint[]): Checkpoint[] {
  return [...checkpoints].sort((a, b) => a.turnIndex - b.turnIndex);
}

export function latestCheckpoint(checkpoints: readonly Checkpoint[]): Checkpoint | null {
  let latest: Checkpoint | null = null;
  for (const checkpoint of checkpoints) {
    if (!latest || checkpoint.turnIndex > latest.turnIndex) latest = checkpoint;
  }
  return latest;
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
    if (isLockfileish(file.path) || buildPatchDisplayRows(file.lines).length > 400) {
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
  const rows = buildPatchDisplayRows(file.lines);
  return rows.some((row) => {
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
  return value === 'turn' || value === 'session' || value === 'workspace' || value === 'branch' || value === 'pr';
}

function userFacingError(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'Review diff failed.';
}

export function __resetReviewPaneStateForTest(): void {
  statesBySourcePane.clear();
}

export type { CommentAnchor };
export type { DiffReviewComment };
