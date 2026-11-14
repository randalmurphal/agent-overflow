<script lang="ts">
  import { onDestroy, onMount, setContext, untrack } from 'svelte';
  import { companionForSource } from '../../stores/companionPanes.svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import ReviewCILogView from './ReviewCILogView.svelte';
  import ReviewConflictHeader from './ReviewConflictHeader.svelte';
  import ReviewPaneBanners from './ReviewPaneBanners.svelte';
  import ReviewPaneFind, { type ReviewFindView } from './ReviewPaneFind.svelte';
  import ReviewCommentThread from './ReviewCommentThread.svelte';
  import ReviewCommitCard from './ReviewCommitCard.svelte';
  import ReviewRangeCard from './ReviewRangeCard.svelte';
  import ReviewLineMenu from './ReviewLineMenu.svelte';
  import ReviewDiffBody from './ReviewDiffBody.svelte';
  import ReviewDraftEditor from './ReviewDraftEditor.svelte';
  import ReviewOverview from './ReviewOverview.svelte';
  import ReviewPRThreadRow from './ReviewPRThreadRow.svelte';
  import ReviewPRTitleBar from './ReviewPRTitleBar.svelte';
  import ReviewRail from './ReviewRail.svelte';
  import ReviewSendStrip from './ReviewSendStrip.svelte';
  import ReviewToolbar from './ReviewToolbar.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    disposeReviewStateForPane,
    reviewStateForPane,
    reviewSubjectForPane,
    type ReviewPaneState,
    type ReviewSubject,
  } from '../../stores/reviewPane.svelte';
  import { reviewStateForPR, reviewSubjectForPR } from '../../stores/reviewPanePinned.svelte';
  import { GitListBranches } from '../../stores/bindings';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { registerPaneBackStep } from '../../stores/paneBackSteps';
  import { getPane } from '../../stores/panes.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import type { GitBranch } from '../../types/git';
  import type { DiffReviewComment } from '../../types/models';
  import type { PRRef } from '../../utils/prReference';
  import { countReviewComments, type CommentCounts } from '../../utils/reviewComments';
  import { commitOrdinal, commitSelectionIndexes } from '../../utils/commitRange';
  import type { EditorRow, ReviewLeadRow } from '../../utils/reviewRows';
  import { filterReviewFiles } from '../../utils/reviewTree';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from '../chat/markdown/forgeAttachmentContext';
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import { commentWriteKey } from '../../stores/reviewCommentWrites.svelte';
  import { EDITOR_PREVIEW_KEY } from '../../stores/reviewEditorPreview.svelte';
  import type { SuggestionSource } from './ReviewCommentMarkdown.svelte';
  import { draftNewSideSpan, editorNewSideSpan, readNewSideLines, type NewSideSpan } from '../../utils/suggestionLines';

  interface Props {
    /** A review companion beside a thread pane. */
    ctx?: PanelContext;
    /** A review pinned to one PR with no thread (the Pull requests page).
     * The owner keys this component on the PR and calls
     * `disposePRReviewStates(ownerPaneId)` when its page goes; the states
     * of the PR on screen and the one before it outlive this component. */
    pr?: { ownerPaneId: string; backend: BackendKey; projectId: string; pr: PRRef };
    /** Pinned: whether a person opened this PR, which a new state's first
     * read carries (a restored page's does not). */
    userInitiated?: boolean;
    /** The key of this review's find (state, provider, bar, highlights).
     * A companion defaults to its layout pane id; a pinned review has no
     * find unless its host passes one. */
    findKey?: string;
  }

  let { ctx, pr, userInitiated = false, findKey: findKeyProp }: Props = $props();
  // File-filter state (type chips, hide tests) is owned here, not in the
  // tree, so it narrows the diff body too, and the selection survives the
  // rail hiding. The dropdown's "Apply filter to diff" item is the switch
  // that turns the diff narrowing off.
  const activeExtensions = new SvelteSet<string>();
  let hideTests = $state(false);
  let filterDiff = $state(true);
  // Captured at init, NOT $derived: the subject is fixed for this instance
  // (CompanionPane keys the body on conversation, ownership epoch and checkout,
  // so a move, workspace change or draft materialization remounts it; the
  // Pull requests page keys it on the PR), and the state lookups are
  // side-effectful: a subject mismatch or the pinned retention bound
  // disposes a state, which writes $state and is therefore illegal inside
  // a derived (state_unsafe_mutation). As a derived, a source-pane thread
  // switch could re-evaluate it on the OLD instance before the {#key}
  // teardown and crash the render flush.
  function initialSubject(): { subject: ReviewSubject | null; error: string | null } {
    if (!pr) return { subject: ctx ? reviewSubjectForPane(ctx) : null, error: null };
    try {
      return { subject: reviewSubjectForPR(pr), error: null };
    } catch (err) {
      return { subject: null, error: err instanceof Error ? err.message : String(err) };
    }
  }
  const { subject, error: subjectError } = initialSubject();
  // svelte-ignore state_referenced_locally
  const sourcePaneId = pr?.ownerPaneId ?? ctx?.paneId ?? '';
  function initialState(): ReviewPaneState | null {
    if (!subject) return null;
    return subject.pinned ? reviewStateForPR(sourcePaneId, subject, { userInitiated }) : reviewStateForPane(sourcePaneId, subject);
  }
  const review = initialState();
  // The conflict view's reconcile permit follows this mount, and whether
  // the layout pane hosting it is on screen, not how long the state is
  // retained. A pinned review lives in its owner's pane; a companion in
  // its own layout pane, fixed for its life.
  // svelte-ignore state_referenced_locally
  const companionPaneId = pr ? null : (untrack(() => companionForSource(sourcePaneId, 'review')?.paneId) ?? null);
  // svelte-ignore state_referenced_locally
  const hostPaneId = pr?.ownerPaneId ?? companionPaneId ?? sourcePaneId;
  $effect(() => review?.attachSurface(hostPaneId));
  // Back steps out of a view open inside the review before the review
  // goes: the CI log, the conflict view, then on compact the open rail,
  // which covers the diff.
  let rail: ReviewRail | undefined = $state();
  onDestroy(registerPaneBackStep(hostPaneId, () => {
    if (!review) return false;
    if (review.armedEditorId !== null) review.cancelEditorMove();
    else if (review.ciLogView) review.closeCILogView();
    else if (review.conflictView) review.closeConflictView();
    else if (rail && treeVisible && isCompactLayout()) toggleTree();
    else return false;
    return true;
  }));
  const findKey = $derived(findKeyProp ?? companionPaneId);
  // Forge attachments in every ChatMarkdown under this pane — the PR body,
  // the conversation, each thread's comments, the comments list. A reader
  // rather than a value: the PR reference and its web URL arrive after
  // mount, and context is set once. Non-PR scopes answer null, so a
  // workspace diff's markdown behaves exactly as it always has.
  setContext(FORGE_ATTACHMENT_SOURCE_CONTEXT, () =>
    review && review.scope === 'pr' && review.prRef
      ? { pr: review.prRef, backend: review.backend, webBase: review.prDetail?.url ?? '' }
      : null,
  );
  // The diff body keys scroll memory by `review.identity` (conversation,
  // owner epoch and checkout) and files its span-cache entries under
  // `review.rowId`, the row id that thread eviction and draft adoption use.
  const workspace = subject?.workspace ?? null;
  // A pinned review has no source pane: no thread, no git status to watch.
  const threadId = $derived(subject?.pinned ? null : (ctx?.threadId ?? null));
  const gitStatus = $derived(subject?.pinned || !ctx ? null : (getPane(ctx.paneId)?.gitStatus.status ?? null));
  let branches: GitBranch[] = $state([]);
  let branchesError: string | null = $state(null);
  const storedTreeVisible = readTreeVisiblePref();
  let rootEl: HTMLElement | undefined = $state();
  let treeVisible = $state(storedTreeVisible ?? true);
  let jumpFilePath: string | null = $state(null);
  let diffBody: ReviewDiffBody | undefined = $state();
  let ciLogView: ReviewCILogView | undefined = $state();
  const findView = $derived<ReviewFindView>(!review ? 'none' : review.ciLogView ? 'log' : review.conflictView ? 'conflicts' : 'diff');
  let topFileIndex = $state(-1);
  // The files the diff body renders: the tree's filtered subset (type
  // chips, hide tests; never the search box), or the full set once the
  // dropdown's apply-to-diff item is turned off.
  const diffFiles = $derived.by(() => {
    const files = review?.files ?? [];
    if (!filterDiff || (activeExtensions.size === 0 && !hideTests)) return files;
    return filterReviewFiles(files, '', activeExtensions, hideTests, review?.revealedPaths).files;
  });
  // The tree highlights by index into the FULL file list; the diff body
  // reports indexes into diffFiles — map through the path.
  const treeActiveFileIndex = $derived.by(() => {
    if (topFileIndex < 0) return -1;
    const path = diffFiles[topFileIndex]?.path;
    if (path === undefined) return -1;
    return (review?.files ?? []).findIndex((file) => file.path === path);
  });
  // Open comment editors keep the diff list mounted through an empty,
  // failed or loading diff: their text is the user's.
  const editorsKept = $derived((review?.openEditors.length ?? 0) > 0);
  const diffStats = $derived({
    files: diffFiles.length,
    additions: diffFiles.reduce((sum, file) => sum + file.additions, 0),
    deletions: diffFiles.reduce((sum, file) => sum + file.deletions, 0),
  });
  const collapsibleFileCount = $derived(
    review
      ? (review.conflictView ? (review.conflicts?.paths.length ?? 0) : review.files.length)
      : 0,
  );
  const EMPTY_COUNTS: CommentCounts = { byFile: new Map(), tally: { unresolved: 0, drafts: 0, total: 0 } };
  const commentCounts = $derived(
    review ? countReviewComments({ prThreads: review.prThreads, drafts: review.drafts }) : EMPTY_COUNTS,
  );
  // The picked commit or run, when the list holds it (branch and pr scope).
  const selectedAt = $derived(commitSelectionIndexes(review?.commits ?? [], review?.selectedCommits ?? null));
  const selectedCommit = $derived(selectedAt && selectedAt.newer === selectedAt.older ? (review?.commits[selectedAt.newer] ?? null) : null);
  const selectedRun = $derived(selectedAt && selectedAt.newer !== selectedAt.older ? (review?.commits.slice(selectedAt.newer, selectedAt.older + 1) ?? null) : null);

  // The diff list's first row: the picked commit's card (its subject,
  // author and message live there, not in the toolbar); else, in PR
  // scope, the overview (Merge + Description + Conversation): the Merge
  // section always has something to say about a PR, so the row exists
  // for every one; else nothing.
  const leadRow = $derived.by((): ReviewLeadRow | null => {
    if (selectedCommit || selectedRun) return 'commit';
    if (review?.scope === 'pr' && review.prDetail !== null) return 'overview';
    return null;
  });
  const overviewShown = $derived(leadRow === 'overview');
  // True once the overview row has scrolled off the top: the title bar
  // then shows peek buttons back to it.
  let overviewOff = $state(false);

  onMount(() => {
    if (storedTreeVisible !== null) return;
    const width = rootEl?.clientWidth ?? 0;
    if (width > 0 && width < 700) treeVisible = false;
  });

  $effect(() => {
    const scope = review?.scope;
    if (!workspace || scope !== 'branch') return;
    let cancelled = false;
    void GitListBranches(workspace)
      .then((next) => {
        if (cancelled) return;
        branches = ((next ?? []) as GitBranch[]).filter((branch) => branch.name);
        branchesError = null;
      })
      .catch((err) => {
        if (cancelled) return;
        branches = [];
        branchesError = err instanceof Error ? err.message : String(err);
      });
    return () => {
      cancelled = true;
    };
  });

  // A pinned state is the page's retention to dispose, not this mount's:
  // the companion registry never holds one, so this leaves it alone.
  onDestroy(() => {
    if (review) disposeReviewStateForPane(sourcePaneId, review);
  });

  // Escape anywhere in the pane cancels a Move waiting for its line.
  $effect(() => {
    const root = rootEl;
    if (!root) return;
    const onKeydown = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape' || event.defaultPrevented || review?.armedEditorId == null) return;
      event.preventDefault();
      review.cancelEditorMove();
    };
    root.addEventListener('keydown', onKeydown);
    return () => root.removeEventListener('keydown', onKeydown);
  });

  function toggleTree(): void {
    treeVisible = !treeVisible;
    appStorageSet('reviewTreeVisible', treeVisible ? 'true' : 'false');
  }

  // On the phone the open tree covers the diff, so picking a file is also
  // the way back to it.
  function jumpToFile(filePath: string): void {
    jumpFilePath = filePath;
    if (isCompactLayout() && treeVisible) toggleTree();
  }

  function onJumpConsumed(): void {
    jumpFilePath = null;
    review?.consumePendingJumpFilePath();
  }

  // Where a new comment's or a draft's suggestion reads the lines it
  // replaces: its new-side line or range in the PR's current diff.
  function spanSource(span: NewSideSpan | null): SuggestionSource | null {
    return span ? (review?.suggestions.lineSource(span.path, span.from, span.to) ?? null) : null;
  }
  function editorSource(editor: EditorRow): SuggestionSource | null {
    return spanSource(editorNewSideSpan(editor));
  }
  // `Suggest change` where the editor's lines are in the diff: it reads
  // their text when pressed.
  function suggestFor(editor: EditorRow): (() => Promise<string[] | null>) | null {
    const source = editorSource(editor);
    return source ? () => readNewSideLines(source.file, source.from, source.to) : null;
  }

  // In a head-ending run an editor posts now only on a PR line.
  function editorLineComment(editor: EditorRow): { target: string; reason: string | null } | null {
    return editor.anchor ? (review?.lineComment(editor.anchor) ?? null) : null;
  }
  function postFor(editor: EditorRow): { primary: 'now' | 'draft'; withheld: string | null } | null {
    if (!review?.canPostNow) return null;
    const line = editorLineComment(editor);
    if (line && line.target !== 'pr') return null;
    return { primary: review.primaryCommentAction, withheld: review.nowWithheldReason };
  }

  // Copy link to line: the PR's whole diff (All commits) only, the one
  // the forge's diff page shows; a picked commit's or run's numbers are
  // not that page's.
  let lineMenu: ReviewLineMenu | undefined = $state();
  const lineLinkTarget = $derived.by(() => {
    const forge = review?.prRef?.forge;
    const url = review?.prDetail?.url ?? '';
    if (review?.scope !== 'pr' || review.selectedCommitSHA || !url || (forge !== 'github' && forge !== 'gitlab')) return null;
    return { forge, url } as const;
  });
  function openLineMenu(event: MouseEvent, path: string, lineIndex: number): boolean {
    const file = diffFiles.find((candidate) => candidate.path === path);
    return file ? (lineMenu?.open(event, file, lineIndex) ?? false) : false;
  }

  function commentById(commentId: string): DiffReviewComment | null {
    return review?.comments.find((comment) => comment.id === commentId) ?? null;
  }

  function readTreeVisiblePref(): boolean | null {
    const raw = appStorageGet('reviewTreeVisible');
    if (raw === 'true') return true;
    if (raw === 'false') return false;
    return null;
  }
</script>

<section bind:this={rootEl} class="flex h-full min-h-0 flex-col bg-surface-1" data-testid="review-pane">
  <ReviewToolbar
    {review}
    {workspace}
    {threadId}
    {branches}
    {gitStatus}
    stats={diffStats}
    {collapsibleFileCount}
    onClose={ctx ? () => ctx?.close() : undefined}
  />
  {#if findKey && review}
    <ReviewPaneFind
      {findKey}
      title="Review"
      {review}
      view={findView}
      files={findView === 'conflicts' ? review.conflictFiles : diffFiles}
      root={rootEl}
      diff={() => diffBody}
      log={() => ciLogView}
    />
  {/if}

  {#if !review}
    <div class="p-3 text-sm text-fg-muted" data-testid="review-no-subject">{subjectError ?? 'No thread selected.'}</div>
  {:else}
    <!-- A render throw below this point (a keyed-each collision, a row
         model that cannot build) used to abort the flush mid-branch and
         leave whatever DOM the previous branch had — "Loading…" with a
         fully loaded store and nothing on screen to say why. -->
    <RenderBoundary label="The review pane" testId="review-render-error">
    <ReviewPaneBanners {review} {branchesError} />
    {#if review.scope === 'pr' && review.prDetail}
      <ReviewPRTitleBar
        detail={review.prDetail}
        hasWorkspace={workspace !== null}
        onViewConflicts={() => { void review?.openConflictView(); }}
        ciPipeline={review.ciPipeline}
        ciLoading={review.ciLoading}
        ciRefreshing={review.ciRefreshing}
        ciError={review.ciError}
        ciFailure={review.ciFailure}
        forge={review.prRef?.forge}
        onOpenCIJob={(stageName, job) => review?.openCIJobLog(stageName, job)}
        onRefreshCI={() => { void review?.refreshCI(); }}
        ciActions={review.ciActions}
        {review}
        overviewOff={overviewShown && overviewOff}
      />
    {/if}
    {#if review.ciLogView}
      <ReviewCILogView
        bind:this={ciLogView}
        view={review.ciLogView}
        log={review.ciLog}
        fetchLines={(from, count) => review?.readCILogLines(from, count) ?? Promise.reject(new Error('No CI job log is open.'))}
        loading={review.ciLogLoading}
        error={review.ciLogError}
        failure={review.ciLogFailure}
        forge={review.prRef?.forge}
        savedPath={review.ciLogSavedPath}
        openSections={review.ciLogOpenSections}
        onBack={() => review?.closeCILogView()}
        onRefresh={() => review?.refreshCILog()}
        onSave={() => { void review?.saveCILog(); }}
        onSend={review.pinned ? undefined : () => { void review?.sendCILogToChat(); }}
        onToggleSection={(sectionKey) => review?.toggleCILogSection(sectionKey)}
        onSetSectionsOpen={(sectionKeys, open) => review?.setCILogSectionsOpen(sectionKeys, open)}
        onCopySection={(section) => review?.copyCILogSection(section) ?? Promise.resolve(false)}
        onSendSection={review.pinned ? undefined : (section) => { void review?.sendCILogSectionToChat(section); }}
        actions={review.ciActions}
        retriedFrom={review.ciLogRetriedFrom}
      />
    {:else if review.conflictView}
      <ReviewConflictHeader {review} />
      {#if review.conflictsLoading && review.conflictFiles.length === 0}
        <div class="px-4 py-3 text-xs text-fg-muted">Loading…</div>
      {:else if review.conflicts && review.conflicts.paths.length === 0}
        <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-conflicts-empty">
          No conflicts against {review.conflicts.baseLabel} right now
        </div>
      {:else}
        <div class="flex min-h-0 flex-1">
          <!-- Conflict content is read-only by design: no comment anchors,
               draft editors, or PR thread rows. The tree is hidden here to
               keep the conflict surface scoped to the marker-bearing files. -->
          <ReviewDiffBody
            bind:this={diffBody}
            subjectId={review.identity}
            spanOwner={review.rowId}
            scope={`${review.scope}:conflicts`}
            files={review.conflictFiles}
            viewMode={review.viewMode}
            wordWrap={review.wordWrap}
            collapsedPaths={review.conflictCollapsedPaths}
            onToggleCollapsed={review.toggleConflictCollapsed}
            onExpandFold={review.expandConflictFold}
            painted={review.conflictPaintedSpans}
            drafts={[]}
            openEditors={[]}
            prThreads={[]}
            expandedPRThreadIds={new Set()}
            jumpToFilePath={null}
            onTopFileChange={(fileIndex) => { topFileIndex = fileIndex; }}
          />
        </div>
      {/if}
    {:else if review.loading && review.files.length === 0 && !editorsKept}
      <div class="px-4 py-3 text-xs text-fg-muted">Loading…</div>
    {:else if review.files.length === 0 && review.awaitingPR && !editorsKept}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-awaiting-pr">Waiting for the {forgeLabels(review.prRef?.forge).longSingular.toLowerCase()}…</div>
    {:else if review.files.length === 0 && review.error && !editorsKept}
      <!-- Nothing loaded: the banner above says why. An empty diff is not
           what the load found. -->
    {:else if review.files.length === 0 && !overviewShown && !editorsKept}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-empty">No changed files.</div>
    {:else}
      <!-- Relative: on the phone the open rail lays over the diff. The
           overview (Merge, Description, Conversation) leads the list for
           every PR, with no file rows under it when the diff is empty or
           the filter hides every file; the rail stays while there are
           files to unhide. -->
      <div class="relative flex min-h-0 flex-1">
        {#if review.files.length > 0}
          <ReviewRail
            bind:this={rail}
            files={review.files}
            activeFileIndex={treeActiveFileIndex}
            onSelectFile={jumpToFile}
            commentCounts={commentCounts.byFile}
            open={treeVisible}
            onToggleOpen={toggleTree}
            {activeExtensions}
            {hideTests}
            onHideTestsChange={(value) => { hideTests = value; }}
            {filterDiff}
            onFilterDiffChange={(value) => { filterDiff = value; }}
            revealedPaths={review.revealedPaths}
            onFilterSelectionChange={() => review?.clearRevealedPaths()}
          />
        {/if}
        {#snippet leadRowContent()}
          {#if selectedCommit}
            <ReviewCommitCard commit={selectedCommit} ordinal={commitOrdinal(selectedAt?.newer ?? 0, review?.commitsTotal ?? null)} total={review?.commitsTotal ?? null} />
          {:else if selectedRun}
            <ReviewRangeCard
              commits={selectedRun}
              olderIndex={selectedAt?.older ?? 0}
              total={review?.commitsTotal ?? null}
              onLeave={() => { void review?.selectCommits(null, { userInitiated: true }); }}
              indexError={review?.headRange && review.prDiffIndexState === 'failed' ? review.prDiffIndexError : null}
              onRetryIndex={() => review?.retryPRDiffIndex()}
            />
          {:else if review?.prDetail}
            <ReviewOverview {review} detail={review.prDetail} hasWorkspace={workspace !== null} />
          {/if}
        {/snippet}
        <ReviewDiffBody
          bind:this={diffBody}
          subjectId={review.identity}
          spanOwner={review.rowId}
          scope={review.scope}
          files={diffFiles}
          viewMode={review.viewMode}
          wordWrap={review.wordWrap}
          spanContext={review.spanContext}
          painted={review.paintedSpans}
          collapsedPaths={review.collapsedPaths}
          onToggleCollapsed={review.toggleCollapsed}
          drafts={review.shownDrafts}
          mediaHost={review.mediaHost}
          openEditors={review.openEditors}
          prThreads={review.shownPRThreads}
          expandedPRThreadIds={review.expandedPRThreadIds}
          generation={review.shownGeneration}
          onAddComment={review.canDraftComments && review.commentsGranted ? (anchor, generation) => review?.openDraftEditor(anchor, generation) : undefined}
          onExtendRange={review.canDraftComments && review.commentsGranted && review.canPostNow && !review.headRange ? (anchor, generation) => review?.extendEditorRange(anchor, generation) : undefined}
          lineComment={review.headRange ? (anchor) => review?.lineComment(anchor) ?? null : undefined}
          onLineMenu={lineLinkTarget ? openLineMenu : undefined}
          rangesFor={(path) => review?.rangeTintsFor(path) ?? []}
          onExpandGap={(path, gap, dir) => { void review?.expandDiffContext(path, gap, dir); }}
          jumpToFilePath={jumpFilePath ?? review.pendingJumpFilePath}
          onJumpConsumed={onJumpConsumed}
          jumpToRowKey={review.pendingJumpRowKey}
          onJumpRowConsumed={() => review?.consumePendingJumpRowKey()}
          onTopFileChange={(fileIndex) => { topFileIndex = fileIndex; }}
          overview={leadRow ? leadRowContent : undefined}
          overviewLead={leadRow ?? 'overview'}
          onOverviewOffChange={(off) => { overviewOff = off; }}
        >
          {#snippet empty()}
            {#if review?.files.length === 0}
              <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-empty">No changed files.</div>
            {:else}
              <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-filter-empty">No files match the filter.</div>
            {/if}
          {/snippet}
          {#snippet draftEditor(editor)}
            <ReviewDraftEditor
              {editor}
              body={review?.draftBodyFor(editor.id) ?? ''}
              onBodyChange={(editorId, body) => review?.setDraftBody(editorId, body)}
              onCancel={(editorId) => review?.closeDraftEditor(editorId)}
              onSubmit={(editorId, body, mode) => review?.createComment(editorId, body, mode)}
              post={postFor(editor)}
              agentOnlyReason={editorLineComment(editor)?.reason ?? null}
              pending={review?.commentWritePending(commentWriteKey.editor(editor.id)) ?? false}
              error={review?.commentWriteError(commentWriteKey.editor(editor.id)) ?? null}
              onDropRange={(editorId) => review?.dropEditorRange(editorId)}
              consumeFocus={(editorId) => review?.consumeDraftEditorFocus(editorId) ?? false}
              onUseLine={(editorId, seen) => review?.useSuggestedLine(editorId, seen) ?? false}
              onMove={(editorId) => review?.moveEditor(editorId)}
              onCancelMove={() => review?.cancelEditorMove()}
              preview={review?.editorPreviews.control(commentWriteKey.editor(editor.id)) ?? null}
              previewSource={editorSource(editor)}
              suggest={review?.canPostNow && review.suggestions.available ? suggestFor(editor) : null}
            />
          {/snippet}
          {#snippet commentThread(threadKey, _anchor, pulse)}
            {@const comment = commentById(threadKey)}
            {#if comment}
              <ReviewCommentThread
                {comment}
                {pulse}
                orphaned={review?.orphanedDraftIds().has(comment.id) ?? false}
                editBody={review?.draftEditBodyFor(comment.id) ?? null}
                onEditBodyChange={(commentId, body) => review?.setDraftEditBody(commentId, body)}
                onUpdate={(commentId, body) => review?.updateComment(commentId, body)}
                onDelete={(commentId) => review?.deleteComment(commentId)}
                preview={review?.editorPreviews.control(EDITOR_PREVIEW_KEY.draftEdit(comment.id)) ?? null}
                previewSource={spanSource(draftNewSideSpan(comment))}
              />
            {/if}
          {/snippet}
          {#snippet prThread(thread, anchor, collapsed, orphaned, pulse)}
            <ReviewPRThreadRow
              {thread}
              {anchor}
              {collapsed}
              {orphaned}
              {pulse}
              body={review?.replyBodyFor(thread.id) ?? ''}
              replying={review?.replyOpen(thread.id, 'diff') ?? false}
              error={review?.replyErrorFor(thread.id) ?? null}
              sending={review?.sendingReply(thread.id) ?? false}
              resolving={review?.resolvingThread(thread.id) ?? false}
              resolveError={review?.resolveErrorFor(thread.id) ?? null}
              onToggle={() => review?.togglePRThread(thread.id)}
              onBodyChange={(body) => review?.setReplyBody(thread.id, body)}
              onOpenReply={() => review?.openReply(thread.id, 'diff')}
              onCloseReply={() => review?.closeReply(thread.id, 'diff')}
              onSendReply={() => review?.sendPRThreadReply(thread)}
              onResolve={thread.isResolvable
                ? (resolved) => { void review?.setPRThreadResolved(thread, resolved); }
                : undefined}
              onJumpToConversation={overviewShown
                ? () => review?.jumpToConversationThread(thread.id)
                : undefined}
              {review}
            />
          {/snippet}
        </ReviewDiffBody>
      </div>
    {/if}
    {#if lineLinkTarget}
      <ReviewLineMenu bind:this={lineMenu} forge={lineLinkTarget.forge} prUrl={lineLinkTarget.url} />
    {/if}
    {#if !review.conflictView && review.sendStripShown}
      <ReviewSendStrip {review} />
    {/if}
    </RenderBoundary>
  {/if}
</section>
