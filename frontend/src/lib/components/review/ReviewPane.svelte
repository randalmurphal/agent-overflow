<script lang="ts">
  import { onDestroy, onMount, setContext } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import ReviewCILogView from './ReviewCILogView.svelte';
  import ReviewCommentThread from './ReviewCommentThread.svelte';
  import ReviewCommitCard from './ReviewCommitCard.svelte';
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
  import { getPane } from '../../stores/panes.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import type { GitBranch } from '../../types/git';
  import type { DiffReviewComment } from '../../types/models';
  import type { PRRef } from '../../utils/prReference';
  import { countReviewComments, type CommentCounts } from '../../utils/reviewComments';
  import type { ReviewLeadRow } from '../../utils/reviewRows';
  import { filterReviewFiles } from '../../utils/reviewTree';
  import { rateLimitMessage } from '../../utils/forgeFailure';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from '../chat/markdown/forgeAttachmentContext';
  import RenderBoundary from '../shared/RenderBoundary.svelte';

  interface Props {
    /** A review companion beside a thread pane. */
    ctx?: PanelContext;
    /** A review pinned to one PR with no thread (the Pull requests page).
     * The owner keys this component on the PR and calls
     * `disposePRReviewStates(ownerPaneId)` when its page goes; the states
     * of the PR on screen and the one before it outlive this component. */
    pr?: { ownerPaneId: string; backend: BackendKey; projectId: string; pr: PRRef };
  }

  let { ctx, pr }: Props = $props();
  // File-filter state (type chips, hide tests) is owned here, not in the
  // tree, so the dropdown's "Apply filter to diff" toggle can also narrow
  // the diff body, and the selection survives the rail hiding.
  const activeExtensions = new SvelteSet<string>();
  let hideTests = $state(false);
  let filterDiff = $state(false);
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
    return subject.pinned ? reviewStateForPR(sourcePaneId, subject) : reviewStateForPane(sourcePaneId, subject);
  }
  const review = initialState();
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
  let topFileIndex = $state(-1);
  // The files the diff body renders: the full set, or the tree's filtered
  // subset (type chips, hide tests; never the search box) when the
  // dropdown's apply-to-diff toggle is on.
  const diffFiles = $derived.by(() => {
    const files = review?.files ?? [];
    if (!filterDiff || (activeExtensions.size === 0 && !hideTests)) return files;
    return filterReviewFiles(files, '', activeExtensions, hideTests).files;
  });
  // The tree highlights by index into the FULL file list; the diff body
  // reports indexes into diffFiles — map through the path.
  const treeActiveFileIndex = $derived.by(() => {
    if (topFileIndex < 0) return -1;
    const path = diffFiles[topFileIndex]?.path;
    if (path === undefined) return -1;
    return (review?.files ?? []).findIndex((file) => file.path === path);
  });
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
  // The picked commit, when the list holds it (branch and pr scope).
  const selectedCommitIndex = $derived(
    review?.selectedCommitSHA ? review.commits.findIndex((commit) => commit.sha === review.selectedCommitSHA) : -1,
  );
  const selectedCommit = $derived(selectedCommitIndex >= 0 ? review!.commits[selectedCommitIndex] : null);

  // The diff list's first row: the picked commit's card (its subject,
  // author and message live there, not in the toolbar); else, in PR
  // scope, the overview (Merge + Description + Conversation): the Merge
  // section always has something to say about a PR, so the row exists
  // for every one; else nothing.
  const leadRow = $derived.by((): ReviewLeadRow | null => {
    if (selectedCommit) return 'commit';
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

  {#if !review}
    <div class="p-3 text-sm text-fg-muted" data-testid="review-no-subject">{subjectError ?? 'No thread selected.'}</div>
  {:else}
    <!-- A render throw below this point (a keyed-each collision, a row
         model that cannot build) used to abort the flush mid-branch and
         leave whatever DOM the previous branch had — "Loading…" with a
         fully loaded store and nothing on screen to say why. -->
    <RenderBoundary label="The review pane" testId="review-render-error">
    {#if review.error}
      <!-- A failed refresh keeps the diff it was refreshing below this
           banner; Retry runs the same load again. -->
      <div class="flex items-start justify-between gap-3 border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-error">
        <span class="max-h-32 min-w-0 overflow-y-auto whitespace-pre-wrap break-words" data-testid="review-error-message">{review.error}</span>
        <button
          type="button"
          class="shrink-0 rounded border border-error/40 px-2 py-0.5 disabled:opacity-50"
          disabled={review.loading}
          onclick={() => { void review?.reload(); }}
          data-testid="review-error-retry"
        >Retry</button>
      </div>
    {/if}
    {#if branchesError && review.scope === 'branch'}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-branches-error">
        {branchesError}
      </div>
    {/if}
    {#if review.submitError}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-submit-error">
        {review.submitError}
      </div>
    {/if}
    {#if review.conflictsError}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-conflicts-error">
        {review.conflictsError}
      </div>
    {/if}
    {#if review.prUpdateError}
      {@const failure = review.prUpdateFailure}
      {#if failure?.kind === 'rate_limited'}
        <div class="border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-pr-rate-limited">
          {rateLimitMessage(review.prRef?.forge, failure)}
        </div>
      {:else if failure?.kind === 'setup' || failure?.kind === 'forbidden'}
        <!-- A login to fix outside the app (missing, expired, or without
             the scope or the project): the message names it, and no retry
             will change it until the user does. -->
        <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-pr-setup">
          {review.prUpdateError}
        </div>
      {:else}
        <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-pr-update-error">
          Retrying: {review.prUpdateError}
        </div>
      {/if}
    {/if}
    {#if review.prStale}
      <div class="flex items-center justify-between gap-3 border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-pr-stale">
        <span>PR updated.</span>
        <button type="button" class="rounded border border-warning/40 px-2 py-0.5" onclick={() => { void review?.reload(); }}>Reload</button>
      </div>
    {/if}
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
        {review}
        overviewOff={overviewShown && overviewOff}
      />
    {/if}
    {#if review.ciLogView}
      <ReviewCILogView
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
      />
    {:else if review.conflictView}
      <div class="flex items-center justify-between gap-3 border-b border-border bg-surface-1 px-3 py-2 text-xs">
        <span class="min-w-0 truncate text-fg-muted">
          {#if review.conflicts}
            Conflicts: {review.conflicts.baseLabel} ← {review.conflicts.headLabel} — read-only
          {:else}
            Conflicts — read-only
          {/if}
        </span>
        <button
          type="button"
          class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
          onclick={() => review?.closeConflictView()}
        >
          Back
        </button>
      </div>
      {#if review.conflicts && review.conflicts.messages.length > 0}
        <!-- Fallback only: per-file messages render as note rows inside
             their file's body; this strip carries the rare merge-tree
             message that names no conflicted path. -->
        <div
          class="max-h-28 overflow-y-auto border-b border-warning/30 bg-warning/5 px-3 py-2 font-mono text-[0.6875rem] leading-relaxed text-fg-muted whitespace-pre-wrap"
          data-testid="review-conflict-messages"
        >{review.conflicts.messages.join('\n')}</div>
      {/if}
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
    {:else if review.loading && review.files.length === 0}
      <div class="px-4 py-3 text-xs text-fg-muted">Loading…</div>
    {:else if review.files.length === 0 && review.awaitingPR}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-awaiting-pr">Waiting for the {forgeLabels(review.prRef?.forge).longSingular.toLowerCase()}…</div>
    {:else if review.files.length === 0 && review.error}
      <!-- Nothing loaded: the banner above says why. An empty diff is not
           what the load found. -->
    {:else if review.files.length === 0 && !overviewShown}
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
          />
        {/if}
        {#snippet leadRowContent()}
          {#if selectedCommit}
            <ReviewCommitCard commit={selectedCommit} ordinal={selectedCommitIndex + 1} count={review?.commits.length ?? 0} />
          {:else if review?.prDetail}
            <ReviewOverview {review} detail={review.prDetail} hasWorkspace={workspace !== null} />
          {/if}
        {/snippet}
        <ReviewDiffBody
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
          drafts={review.drafts}
          openEditors={review.openEditors}
          prThreads={review.prThreads}
          expandedPRThreadIds={review.expandedPRThreadIds}
          onAddComment={review.canDraftComments && review.commentsGranted ? (anchor) => review?.openDraftEditor(anchor) : undefined}
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
          {#snippet draftEditor(anchor)}
            <ReviewDraftEditor
              {anchor}
              body={review?.draftBodyFor(anchor) ?? ''}
              onBodyChange={(nextAnchor, body) => review?.setDraftBody(nextAnchor, body)}
              onCancel={(nextAnchor) => review?.closeDraftEditor(nextAnchor)}
              onSubmit={(nextAnchor, body) => review?.createComment(nextAnchor, body)}
              consumeFocus={(nextAnchor) => review?.consumeDraftEditorFocus(nextAnchor) ?? false}
            />
          {/snippet}
          {#snippet commentThread(threadKey, _anchor, pulse)}
            {@const comment = commentById(threadKey)}
            {#if comment}
              <ReviewCommentThread
                {comment}
                {pulse}
                orphaned={review?.orphanedDraftIds().has(comment.id) ?? false}
                onUpdate={(commentId, body) => review?.updateComment(commentId, body)}
                onDelete={(commentId) => review?.deleteComment(commentId)}
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
              error={review?.replyErrorFor(thread.id) ?? null}
              sending={review?.sendingReply(thread.id) ?? false}
              resolving={review?.resolvingThread(thread.id) ?? false}
              resolveError={review?.resolveErrorFor(thread.id) ?? null}
              onToggle={() => review?.togglePRThread(thread.id)}
              onBodyChange={(body) => review?.setReplyBody(thread.id, body)}
              onSendReply={() => review?.sendPRThreadReply(thread)}
              onResolve={thread.isResolvable
                ? (resolved) => { void review?.setPRThreadResolved(thread, resolved); }
                : undefined}
              onJumpToConversation={overviewShown
                ? () => review?.jumpToConversationThread(thread.id)
                : undefined}
            />
          {/snippet}
        </ReviewDiffBody>
      </div>
    {/if}
    {#if !review.conflictView && review.drafts.length > 0}
      <ReviewSendStrip {review} />
    {/if}
    </RenderBoundary>
  {/if}
</section>
