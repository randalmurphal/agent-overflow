<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import ChevronsDownUp from 'lucide-svelte/icons/chevrons-down-up';
  import ChevronsUpDown from 'lucide-svelte/icons/chevrons-up-down';
  import Columns2 from 'lucide-svelte/icons/columns-2';
  import ListTree from 'lucide-svelte/icons/list-tree';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import WrapText from 'lucide-svelte/icons/wrap-text';
  import ReviewCILogView from './ReviewCILogView.svelte';
  import ReviewCommentThread from './ReviewCommentThread.svelte';
  import ReviewDiffBody from './ReviewDiffBody.svelte';
  import ReviewDraftEditor from './ReviewDraftEditor.svelte';
  import ReviewPRHeader from './ReviewPRHeader.svelte';
  import ReviewPRThreadRow from './ReviewPRThreadRow.svelte';
  import ReviewRail, { type ReviewRailTab } from './ReviewRail.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    disposeReviewStateForPane,
    reviewStateForPane,
    type ReviewScope,
  } from '../../stores/reviewPane.svelte';
  import { GitListBranches } from '../../stores/bindings';
  import type { GitBranch } from '../../types/git';
  import type { DiffReviewComment } from '../../types/models';
  import {
    buildCommentGroups,
    commentCountsByFile,
    commentTally,
  } from '../../utils/reviewComments';
  import { fileExtensionLabel } from '../../utils/reviewTree';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    ctx: PanelContext;
  }

  let { ctx }: Props = $props();
  // Extension-filter state is owned here (not in the tree) so the
  // dropdown's "Apply filter to diff" toggle can also narrow the diff
  // body, and the selection survives rail tab switches.
  const activeExtensions = new SvelteSet<string>();
  let extensionsFilterDiff = $state(false);
  // Captured at init, NOT $derived: ctx.threadId is fixed for this
  // instance (CompanionPane keys the panel body on `${thread.id}:${kind}`,
  // so a thread change remounts it), and reviewStateForPane is
  // side-effectful — its thread-mismatch branch disposes the replaced
  // state, which writes $state and is therefore illegal inside a derived
  // (state_unsafe_mutation). As a derived, a source-pane thread switch
  // could re-evaluate it on the OLD instance before the {#key} teardown
  // and crash the render flush.
  // svelte-ignore state_referenced_locally
  const review = ctx.threadId ? reviewStateForPane(ctx.paneId, ctx.threadId, ctx.thread) : null;
  let branches: GitBranch[] = $state([]);
  let branchesError: string | null = $state(null);
  const storedTreeVisible = readTreeVisiblePref();
  let rootEl: HTMLElement | undefined = $state();
  let treeVisible = $state(storedTreeVisible ?? true);
  let jumpFilePath: string | null = $state(null);
  let topFileIndex = $state(-1);
  // The files the diff body renders: the full set, or the extension-
  // filtered subset when the dropdown's apply-to-diff toggle is on.
  const diffFiles = $derived.by(() => {
    const files = review?.files ?? [];
    if (!extensionsFilterDiff || activeExtensions.size === 0) return files;
    return files.filter((file) => activeExtensions.has(fileExtensionLabel(file.path)));
  });
  // The tree highlights by index into the FULL file list; the diff body
  // reports indexes into diffFiles — map through the path.
  const treeActiveFileIndex = $derived.by(() => {
    if (topFileIndex < 0) return -1;
    const path = diffFiles[topFileIndex]?.path;
    if (path === undefined) return -1;
    return (review?.files ?? []).findIndex((file) => file.path === path);
  });
  const totalAdditions = $derived(
    diffFiles.reduce((sum, file) => sum + file.additions, 0),
  );
  const totalDeletions = $derived(
    diffFiles.reduce((sum, file) => sum + file.deletions, 0),
  );
  const collapsibleFileCount = $derived(
    review
      ? (review.conflictView ? (review.conflicts?.paths.length ?? 0) : review.files.length)
      : 0,
  );
  let railTab: ReviewRailTab = $state('files');
  const commentGroups = $derived(
    review
      ? buildCommentGroups({
          files: review.files,
          prThreads: review.prThreads,
          drafts: review.drafts,
          orphanedDraftIds: review.orphanedDraftIds(),
        })
      : [],
  );
  const commentCounts = $derived(commentCountsByFile(commentGroups));
  const tally = $derived(commentTally(commentGroups));
  const tallyLabel = $derived.by(() => {
    const parts: string[] = [];
    if (tally.unresolved > 0) parts.push(`${tally.unresolved} unresolved`);
    if (tally.drafts > 0) parts.push(`${tally.drafts} ${tally.drafts === 1 ? 'draft' : 'drafts'}`);
    if (parts.length === 0) parts.push(`${tally.total} ${tally.total === 1 ? 'comment' : 'comments'}`);
    return parts.join(' · ');
  });

  function openCommentsTab(): void {
    railTab = 'comments';
    if (!treeVisible) toggleTree();
  }

  onMount(() => {
    if (storedTreeVisible !== null) return;
    const width = rootEl?.clientWidth ?? 0;
    if (width > 0 && width < 700) treeVisible = false;
  });

  $effect(() => {
    const threadId = ctx.threadId;
    const scope = review?.scope;
    if (!threadId || scope !== 'branch') return;
    let cancelled = false;
    void GitListBranches(threadId)
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

  onDestroy(() => {
    disposeReviewStateForPane(ctx.paneId);
  });

  function setScope(value: string): void {
    if (!review || !isReviewScope(value)) return;
    void review.setScope(value);
  }

  function setCheckpoint(value: string): void {
    if (!review) return;
    void review.selectCheckpoint(value || null);
  }

  function setBaseBranch(value: string): void {
    if (!review) return;
    void review.setScope('branch', { baseBranch: value });
  }

  function toggleTree(): void {
    treeVisible = !treeVisible;
    appStorageSet('reviewTreeVisible', treeVisible ? 'true' : 'false');
  }

  function jumpToFile(filePath: string): void {
    jumpFilePath = filePath;
  }

  function onJumpConsumed(): void {
    jumpFilePath = null;
    review?.consumePendingJumpFilePath();
  }

  function isReviewScope(value: string): value is ReviewScope {
    return value === 'turn' || value === 'session' || value === 'workspace' || value === 'branch' || value === 'pr';
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
  <div class="flex shrink-0 items-center gap-2 border-b border-border-subtle px-3 py-2">
    <select
      class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-xs text-fg"
      aria-label="Review scope"
      data-testid="review-scope-select"
      value={review?.scope ?? 'workspace'}
      onchange={(event) => setScope(event.currentTarget.value)}
      disabled={!review}
    >
      {#if ctx.workspacePath}
        <option value="turn">Turn</option>
        <option value="session">Session</option>
        <option value="workspace">Workspace</option>
        <option value="branch">Branch</option>
      {/if}
      {#if review?.prRef}
        <option value="pr">{review.prScopeLabel ?? 'PR'}</option>
      {/if}
    </select>

    {#if review?.scope === 'branch'}
      <select
        class="min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-xs text-fg"
        aria-label="Base branch"
        data-testid="review-base-branch-select"
        value={review.baseBranch ?? ''}
        onchange={(event) => setBaseBranch(event.currentTarget.value)}
        disabled={review.loading}
      >
        {#if review.baseBranch && !branches.some((branch) => branch.name === review.baseBranch)}
          <option value={review.baseBranch}>{review.baseBranch}</option>
        {/if}
        {#each branches as branch (branch.name)}
          <option value={branch.name}>{branch.name}</option>
        {/each}
      </select>
    {/if}

    {#if review?.scope === 'turn'}
      <select
        class="min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-1 text-xs text-fg"
        aria-label="Turn checkpoint"
        data-testid="review-checkpoint-select"
        value={review.selectedCheckpointUserItemId ?? ''}
        onchange={(event) => setCheckpoint(event.currentTarget.value)}
        disabled={review.loading}
      >
        <option value="">Latest</option>
        {#each review.checkpoints as checkpoint (checkpoint.userItemId)}
          <option value={checkpoint.userItemId}>Turn {checkpoint.turnIndex}</option>
        {/each}
      </select>
    {/if}

    <div class="min-w-0 flex-1 truncate text-right text-[0.6875rem] tabular-nums text-fg-muted">
      {#if review && !review.conflictView}
        {#if review.scope === 'pr' && review.prDetail}
          <span data-testid="review-pr-meta">
            <span class="rounded border border-border-subtle px-1.5 py-0.5">{review.prDetail.draft ? 'draft' : review.prDetail.state}</span>
            <span class="ml-1.5">{review.prDetail.baseRefName} ← {review.prDetail.headRefName}</span>
          </span>
        {/if}
        {#if diffFiles.length > 0}
          <span data-testid="review-diff-stats" class="ml-2">
            {diffFiles.length} {diffFiles.length === 1 ? 'file' : 'files'}
            {#if totalAdditions > 0}<span class="text-success">+{totalAdditions}</span>{/if}
            {#if totalDeletions > 0}<span class="text-error">-{totalDeletions}</span>{/if}
          </span>
        {/if}
        {#if tally.total > 0}
          <button
            type="button"
            class="ml-2 rounded border border-border-subtle px-1.5 py-0.5 text-fg-muted hover:text-fg"
            title="Open comments list"
            data-testid="review-comment-tally"
            onclick={openCommentsTab}
          >
            {tallyLabel}
          </button>
        {/if}
      {/if}
    </div>

    <button
      type="button"
      class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle disabled:opacity-50 {treeVisible ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:text-fg'}"
      aria-label="Toggle file tree"
      aria-pressed={treeVisible}
      title="File tree"
      data-testid="review-tree-toggle"
      disabled={!review}
      onclick={toggleTree}
    >
      <Icon icon={ListTree} size={14} />
    </button>

    <button
      type="button"
      class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle disabled:opacity-50 {review?.viewMode === 'split' ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:text-fg'}"
      aria-label="Toggle split view"
      aria-pressed={review?.viewMode === 'split'}
      title="Split view"
      data-testid="review-split-toggle"
      disabled={!review}
      onclick={() => review?.setViewMode(review.viewMode === 'split' ? 'stacked' : 'split')}
    >
      <Icon icon={Columns2} size={14} />
    </button>

    <button
      type="button"
      class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle disabled:opacity-50 {review?.wordWrap ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:text-fg'}"
      aria-label="Toggle word wrap"
      aria-pressed={review?.wordWrap}
      title="Word wrap"
      data-testid="review-wrap-toggle"
      disabled={!review}
      onclick={() => review?.setWordWrap(!review.wordWrap)}
    >
      <Icon icon={WrapText} size={14} />
    </button>

    <button
      type="button"
      class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg disabled:opacity-50"
      aria-label={review?.allCollapsed ? 'Expand all files' : 'Collapse all files'}
      title={review?.allCollapsed ? 'Expand all files' : 'Collapse all files'}
      data-testid="review-collapse-all-toggle"
      disabled={!review || !!review.ciLogView || collapsibleFileCount === 0}
      onclick={() => { void review?.toggleCollapseAll(); }}
    >
      <Icon icon={review?.allCollapsed ? ChevronsUpDown : ChevronsDownUp} size={14} />
    </button>

    <button
      type="button"
      class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg disabled:opacity-50"
      aria-label="Reload review diff"
      title="Reload"
      data-testid="review-reload"
      disabled={!review || review.loading}
      onclick={() => { void review?.reload(); }}
    >
      <Icon icon={RefreshCw} size={14} class={review?.loading ? 'animate-spin' : ''} />
    </button>
  </div>

  {#if !ctx.threadId}
    <div class="p-3 text-sm text-fg-muted">No thread selected.</div>
  {:else if review}
    {#if review.error}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-error">
        {review.error}
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
    {#if review.prStale}
      <div class="flex items-center justify-between gap-3 border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-pr-stale">
        <span>PR updated.</span>
        <button type="button" class="rounded border border-warning/40 px-2 py-0.5" onclick={() => { void review?.reload(); }}>Reload</button>
      </div>
    {/if}
    {#if review.scope === 'pr' && review.prDetail}
      <ReviewPRHeader
        detail={review.prDetail}
        hasWorkspace={!!ctx.workspacePath}
        onViewConflicts={() => { void review?.openConflictView(); }}
        ciPipeline={review.ciPipeline}
        ciLoading={review.ciLoading}
        ciError={review.ciError}
        onOpenCIJob={(stageName, job) => { void review?.openCIJobLog(stageName, job); }}
      />
    {/if}
    {#if review.ciLogView}
      <ReviewCILogView
        view={review.ciLogView}
        log={review.ciLog}
        loading={review.ciLogLoading}
        error={review.ciLogError}
        savedPath={review.ciLogSavedPath}
        onBack={() => review?.closeCILogView()}
        onRefresh={() => { void review?.refreshCILog(); }}
        onSave={() => { void review?.saveCILog(); }}
        onSend={() => { void review?.sendCILogToChat(); }}
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
            threadId={review.threadId}
            scope={`${review.scope}:conflicts`}
            files={review.conflictFiles}
            viewMode={review.viewMode}
            wordWrap={review.wordWrap}
            collapsedPaths={review.conflictCollapsedPaths}
            onToggleCollapsed={review.toggleConflictCollapsed}
            onExpandFold={review.expandConflictFold}
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
    {:else if review.files.length === 0}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-empty">No changed files.</div>
    {:else}
      <div class="flex min-h-0 flex-1">
        {#if treeVisible}
          <ReviewRail
            tab={railTab}
            onTabChange={(tab) => { railTab = tab; }}
            files={review.files}
            activeFileIndex={treeActiveFileIndex}
            onSelectFile={jumpToFile}
            {commentCounts}
            {commentGroups}
            onSelectComment={(item) => review?.jumpToComment(item)}
            reviews={review.scope === 'pr' ? (review.prDetail?.latestReviews ?? []) : []}
            {activeExtensions}
            filterDiff={extensionsFilterDiff}
            onFilterDiffChange={(value) => { extensionsFilterDiff = value; }}
          />
        {/if}
        {#if diffFiles.length === 0}
          <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-filter-empty">
            No files match the type filter.
          </div>
        {:else}
        <ReviewDiffBody
          threadId={review.threadId}
          scope={review.scope}
          files={diffFiles}
          viewMode={review.viewMode}
          wordWrap={review.wordWrap}
          collapsedPaths={review.collapsedPaths}
          onToggleCollapsed={review.toggleCollapsed}
          drafts={review.drafts}
          openEditors={review.openEditors}
          prThreads={review.prThreads}
          expandedPRThreadIds={review.expandedPRThreadIds}
          onAddComment={(anchor) => review?.openDraftEditor(anchor)}
          jumpToFilePath={jumpFilePath ?? review.pendingJumpFilePath}
          onJumpConsumed={onJumpConsumed}
          jumpToRowKey={review.pendingJumpRowKey}
          onJumpRowConsumed={() => review?.consumePendingJumpRowKey()}
          onTopFileChange={(fileIndex) => { topFileIndex = fileIndex; }}
        >
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
          {#snippet commentThread(threadKey, _anchor)}
            {@const comment = commentById(threadKey)}
            {#if comment}
              <ReviewCommentThread
                {comment}
                orphaned={review?.scope === 'pr' && review.orphanedDraftIds().has(comment.id)}
                onUpdate={(commentId, body) => review?.updateComment(commentId, body)}
                onDelete={(commentId) => review?.deleteComment(commentId)}
              />
            {/if}
          {/snippet}
          {#snippet prThread(thread, anchor, collapsed, orphaned)}
            <ReviewPRThreadRow
              {thread}
              {anchor}
              {collapsed}
              {orphaned}
              body={review?.replyBodyFor(thread.id) ?? ''}
              error={review?.replyErrorFor(thread.id) ?? null}
              sending={review?.sendingReply(thread.id) ?? false}
              isTurnActive={review?.isTurnActive ?? false}
              onToggle={() => review?.togglePRThread(thread.id)}
              onBodyChange={(body) => review?.setReplyBody(thread.id, body)}
              onSendReply={() => review?.sendPRThreadReply(thread)}
              onSendToAgent={() => review?.sendPRThreadToAgent(thread)}
            />
          {/snippet}
        </ReviewDiffBody>
        {/if}
      </div>
    {/if}
    {#if !review.conflictView && review.drafts.length > 0}
      <section class="border-t border-border bg-surface-1/85 px-3 py-2" aria-label="Review comments" data-testid="review-send-strip">
        <div class="flex items-center justify-between gap-3">
          <div class="text-[0.6875rem] font-medium uppercase tracking-[0.08em] text-fg-muted">
            {review.drafts.length} {review.drafts.length === 1 ? 'draft' : 'drafts'}
          </div>
          <div class="flex items-center gap-2">
            {#if review.scope === 'pr'}
              <select
                class="rounded border border-border-subtle bg-surface-0 px-2 py-1 text-[0.6875rem]"
                value={review.submitTarget}
                onchange={(event) => review?.setSubmitTarget(event.currentTarget.value === 'pr' ? 'pr' : 'agent')}
              >
                <option value="agent">Linked agent</option>
                <option value="pr">{review.prScopeLabel ?? 'PR'} review</option>
              </select>
            {/if}
            <button
              type="button"
              class="rounded border border-accent/45 px-2 py-1 text-[0.6875rem] font-medium text-accent hover:bg-accent/10 disabled:opacity-45"
              disabled={review.sendingComments || (review.submitTarget === 'agent' && review.isTurnActive)}
              title={review.isTurnActive && review.submitTarget === 'agent' ? 'Send from the chat box while the agent is working' : 'Send comments'}
              onclick={() => { void (review?.submitTarget === 'pr' ? review?.submitPRReview() : review?.sendComments()); }}
            >
              Send comments
            </button>
          </div>
        </div>
        {#if review.scope === 'pr' && review.submitTarget === 'pr'}
          <div class="mt-2 flex flex-wrap items-center gap-2">
            {#each ['comment', 'approve', 'request-changes'] as nextVerdict}
              {@const ownPRBlocked = review.prRef?.forge === 'github' && review.prDetail?.viewerIsAuthor && nextVerdict !== 'comment'}
              <button
                type="button"
                class="rounded border px-2 py-1 text-[0.6875rem] transition-colors disabled:cursor-not-allowed disabled:opacity-40 {review.verdict === nextVerdict ? 'border-accent bg-accent/10 text-accent' : 'border-border-subtle text-fg-muted hover:text-fg'}"
                disabled={ownPRBlocked}
                title={ownPRBlocked ? 'GitHub rejects approving or requesting changes on your own PR' : ''}
                onclick={() => review?.setVerdict(nextVerdict as 'comment' | 'approve' | 'request-changes')}
              >
                {nextVerdict === 'request-changes' ? 'Request changes' : nextVerdict[0].toUpperCase() + nextVerdict.slice(1)}
              </button>
            {/each}
          </div>
          <textarea
            class="mt-2 w-full resize-none rounded border border-border-subtle bg-surface-0 px-2 py-1.5 text-xs"
            rows="2"
            value={review.summaryBody}
            oninput={(event) => review?.setSummaryBody(event.currentTarget.value)}
          ></textarea>
        {/if}
      </section>
    {/if}
  {/if}
</section>
