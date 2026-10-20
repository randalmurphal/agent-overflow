<script lang="ts">
  import { onDestroy, onMount, setContext, untrack } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import ChevronsDownUp from '@lucide/svelte/icons/chevrons-down-up';
  import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import X from '@lucide/svelte/icons/x';
  import ReviewCILogView from './ReviewCILogView.svelte';
  import ReviewCommentThread from './ReviewCommentThread.svelte';
  import ReviewCommitCard from './ReviewCommitCard.svelte';
  import ReviewCommitPicker from './ReviewCommitPicker.svelte';
  import ReviewDiffBody from './ReviewDiffBody.svelte';
  import ReviewDraftEditor from './ReviewDraftEditor.svelte';
  import ReviewOverview from './ReviewOverview.svelte';
  import ReviewPRThreadRow from './ReviewPRThreadRow.svelte';
  import ReviewPRTitleBar from './ReviewPRTitleBar.svelte';
  import ReviewRail from './ReviewRail.svelte';
  import ReviewToolbarMenu from './ReviewToolbarMenu.svelte';
  import { appStorageGet, appStorageSet } from '../../stores/appStorage';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import {
    disposeReviewStateForPane,
    reviewStateForPane,
    reviewSubjectForPane,
    type ReviewScope,
  } from '../../stores/reviewPane.svelte';
  import { GitListBranches } from '../../stores/bindings';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import type { GitBranch, GitStatus } from '../../types/git';
  import type { DiffReviewComment } from '../../types/models';
  import { truncateMiddle } from '../../utils/format';
  import { countReviewComments, type CommentCounts } from '../../utils/reviewComments';
  import type { ReviewLeadRow } from '../../utils/reviewRows';
  import { filterReviewFiles } from '../../utils/reviewTree';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import { rateLimitMessage } from '../../utils/forgeFailure';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { threadHasScope } from '../../transport/entityScopes';
  import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from '../chat/markdown/forgeAttachmentContext';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../primitives/MenuSectionHeader.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import RenderBoundary from '../shared/RenderBoundary.svelte';

  interface Props {
    ctx: PanelContext;
  }

  let { ctx }: Props = $props();
  // File-filter state (type chips, hide tests) is owned here, not in the
  // tree, so the dropdown's "Apply filter to diff" toggle can also narrow
  // the diff body, and the selection survives the rail hiding.
  const activeExtensions = new SvelteSet<string>();
  let hideTests = $state(false);
  let filterDiff = $state(false);
  // The "..." menu: view toggles that are set once per reading session
  // (whitespace, wrap, split), off the toolbar so the row fits a phone.
  let moreOpen = $state(false);
  let moreTriggerEl: HTMLButtonElement | undefined = $state(undefined);
  // Drafting, sending and submitting comments all write under
  // `threads:operate` on the thread's computer (Create/SendDiffReviewComments).
  // The gutter affordance is withheld and the send button goes inert with a
  // reason; reading the diff and existing comments is unaffected.
  let operateUngranted = $derived(!threadHasScope('threads:operate', ctx.threadId, ctx.thread?.projectId));
  // Captured at init, NOT $derived: the subject is fixed for this instance
  // (CompanionPane keys the body on conversation, ownership epoch and checkout,
  // so a move, workspace change or draft materialization remounts it), and
  // reviewStateForPane is side-effectful: its
  // subject-mismatch branch disposes the replaced state, which writes
  // $state and is therefore illegal inside a derived
  // (state_unsafe_mutation). As a derived, a source-pane thread switch
  // could re-evaluate it on the OLD instance before the {#key} teardown
  // and crash the render flush.
  // svelte-ignore state_referenced_locally
  const subject = reviewSubjectForPane(ctx);
  // svelte-ignore state_referenced_locally
  const sourcePaneId = ctx.paneId;
  const review = subject ? reviewStateForPane(sourcePaneId, subject) : null;
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
  // scope, the overview (Description + Conversation) when there is
  // anything to put in it; else nothing.
  const leadRow = $derived.by((): ReviewLeadRow | null => {
    if (selectedCommit) return 'commit';
    if (
      review?.scope === 'pr' && review.prDetail !== null
      && (review.prDetail.body !== '' || review.prThreads.length + review.prDetail.latestReviews.length > 0)
    ) return 'overview';
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
    const workspace = ctx.workspace;
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

  onDestroy(() => {
    if (review) disposeReviewStateForPane(sourcePaneId, review);
  });

  function setScope(value: ReviewScope): void {
    if (!review) return;
    void review.setScope(value);
  }

  function setBaseBranch(value: string): void {
    if (!review) return;
    void review.setScope('branch', { baseBranch: value });
  }

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

  function closeMore(reason?: PopoverCloseReason): void {
    moreOpen = false;
    restorePickerFocus(reason, { triggerEl: moreTriggerEl });
  }

  // A pick closes the menu, as the header's "..." menu does.
  function pickViewOption(action: () => void): void {
    moreOpen = false;
    action();
  }

  function onJumpConsumed(): void {
    jumpFilePath = null;
    review?.consumePendingJumpFilePath();
  }

  // The scopes this pane can show: worktree scopes need a workspace, edits
  // a thread, the PR an open pull request.
  const scopeOptions = $derived.by((): { value: ReviewScope; label: string }[] => {
    const options: { value: ReviewScope; label: string }[] = [];
    if (ctx.workspace) options.push({ value: 'workspace', label: 'Workspace' }, { value: 'branch', label: 'Branch' });
    if (ctx.threadId) options.push({ value: 'edits', label: 'Edits' });
    if (review?.prRef) options.push({ value: 'pr', label: review.prScopeLabel ?? 'PR' });
    return options;
  });
  const scopeLabel = $derived(
    scopeOptions.find((option) => option.value === review?.scope)?.label ?? 'Workspace',
  );
  // The base-branch menu lists the repository's branches plus the current
  // base when it is not among them (a deleted or remote-only branch).
  const baseBranchOptions = $derived.by(() => {
    const names = branches.map((branch) => branch.name);
    const current = review?.baseBranch;
    return current && !names.includes(current) ? [current, ...names] : names;
  });
  // Trigger labels for values of unbounded length (a branch name, an
  // edit's title) shorten in the middle so the row never grows past the
  // pane; the whole value is the trigger's hover text.
  const TRIGGER_LABEL_MAX = 28;

  function setEdit(value: string): void {
    if (!review) return;
    void review.selectEdit(value || null);
  }

  // Edits selector groups: ascending turn order; each group offers the
  // whole-turn option first, then every edit — including single-edit
  // turns, so a pinned `item:` selection always has a matching option.
  const editGroups = $derived.by(() => {
    if (!review || review.scope !== 'edits') return [];
    const groups = new Map<number, typeof review.edits[number][]>();
    for (const entry of review.edits) {
      const group = groups.get(entry.turnIndex);
      if (group) group.push(entry);
      else groups.set(entry.turnIndex, [entry]);
    }
    return [...groups.entries()].map(([turnIndex, entries]) => ({
      turnIndex,
      label: review.editTurnLabels.get(turnIndex) ?? `Turn ${turnIndex}`,
      entries,
    }));
  });

  // What the edits trigger reads: the picked edit's title, or the turn's
  // prompt label (pre-capped from Go, editSelectorLabel) for a whole turn.
  const editLabel = $derived.by((): string => {
    const key = review?.selectedEditKey;
    if (!key) return 'Edits';
    if (key.startsWith('turn:')) {
      const turnIndex = Number(key.slice('turn:'.length));
      return editGroups.find((group) => group.turnIndex === turnIndex)?.label ?? `Turn ${turnIndex}`;
    }
    const itemId = key.slice('item:'.length);
    return review?.edits.find((entry) => entry.itemId === itemId)?.title ?? 'Edit';
  });

  function commentById(commentId: string): DiffReviewComment | null {
    return review?.comments.find((comment) => comment.id === commentId) ?? null;
  }

  function readTreeVisiblePref(): boolean | null {
    const raw = appStorageGet('reviewTreeVisible');
    if (raw === 'true') return true;
    if (raw === 'false') return false;
    return null;
  }

  // ------------------------------------------------------------------
  // Working-tree staleness hint
  // ------------------------------------------------------------------
  // The source chat pane already owns a live gitwatch slot; reading its
  // status here costs nothing extra. A signature captured when the diff
  // finished loading, diverging from the live one, means the loaded
  // patch no longer matches the working tree — surfaced as a dot on the
  // refresh button, NEVER as an automatic reload (the diff must not
  // move under the reader).
  function gitStatusSignature(status: GitStatus | null): string | null {
    if (!status?.isRepo) return null;
    return [
      status.branch,
      status.fileCount,
      status.insertions,
      status.deletions,
      status.aheadCount,
      status.behindCount,
    ].join(':');
  }

  const liveGitSignature = $derived(
    gitStatusSignature(getPane(ctx.paneId)?.gitStatus.status ?? null),
  );
  let loadedGitSignature = $state<string | null>(null);
  let wasLoading = false;
  $effect(() => {
    const loading = review?.loading ?? false;
    const live = liveGitSignature;
    untrack(() => {
      if (wasLoading && !loading) {
        // A load just completed: the diff corresponds to this status.
        loadedGitSignature = live;
      } else if (loadedGitSignature === null && live !== null) {
        // The diff loaded before the gitwatch slot produced its first
        // status; adopt it as the baseline instead of flagging stale.
        loadedGitSignature = live;
      }
      wasLoading = loading;
    });
  });
  const WORKTREE_SCOPES: readonly ReviewScope[] = ['workspace', 'branch'];
  const diffStale = $derived(
    !!review && !review.loading && !review.conflictView
      && WORKTREE_SCOPES.includes(review.scope)
      && loadedGitSignature !== null && liveGitSignature !== null
      && liveGitSignature !== loadedGitSignature,
  );
</script>

<section bind:this={rootEl} class="flex h-full min-h-0 flex-col bg-surface-1" data-testid="review-pane">
  <!-- Wraps: a narrow pane carries the counts and the icon run on a second
       row instead of clipping them. -->
  <div class="flex shrink-0 flex-wrap items-center gap-2 border-b border-border-subtle px-3 py-2" data-testid="review-toolbar">
    <ReviewToolbarMenu ariaLabel="Review scope" testId="review-scope-select" disabled={!review}>
      {#snippet label()}<span>{scopeLabel}</span>{/snippet}
      {#snippet children(close)}
        {#each scopeOptions as option (option.value)}
          <MenuItem
            label={option.label}
            checked={option.value === review?.scope}
            testId={`review-scope-option-${option.value}`}
            onSelect={() => { close(); setScope(option.value); }}
          />
        {/each}
      {/snippet}
    </ReviewToolbarMenu>

    {#if review?.scope === 'branch'}
      <ReviewToolbarMenu
        ariaLabel="Base branch"
        title={review.baseBranch ?? 'Base branch'}
        testId="review-base-branch-select"
        disabled={review.loading}
      >
        {#snippet label()}<span>{truncateMiddle(review?.baseBranch ?? 'Base branch', TRIGGER_LABEL_MAX)}</span>{/snippet}
        {#snippet children(close)}
          {#each baseBranchOptions as name (name)}
            <MenuItem
              label={name}
              title={name}
              checked={name === review?.baseBranch}
              testId="review-base-branch-option"
              onSelect={() => { close(); setBaseBranch(name); }}
            />
          {/each}
        {/snippet}
      </ReviewToolbarMenu>
    {/if}

    {#if review && (review.scope === 'branch' || review.scope === 'pr') && review.commits.length > 0}
      <ReviewCommitPicker
        commits={review.commits}
        selectedSha={review.selectedCommitSHA}
        disabled={review.loading}
        onSelect={(sha) => { void review?.selectCommit(sha); }}
      />
    {/if}

    {#if review && review.scope === 'edits' && editGroups.length > 0}
      <ReviewToolbarMenu
        ariaLabel="Edit"
        title={editLabel}
        testId="review-edit-select"
        disabled={review.loading}
        minWidthClass="min-w-[280px]"
      >
        {#snippet label()}<span>{truncateMiddle(editLabel, TRIGGER_LABEL_MAX)}</span>{/snippet}
        {#snippet children(close)}
          {#each editGroups as group (group.turnIndex)}
            <MenuSectionHeader label={group.label} />
            <MenuItem
              label="All edits in turn"
              suffix={String(group.entries.length)}
              checked={review?.selectedEditKey === `turn:${group.turnIndex}`}
              testId="review-edit-option"
              onSelect={() => { close(); setEdit(`turn:${group.turnIndex}`); }}
            />
            {#each group.entries as entry (entry.itemId)}
              <MenuItem
                label={entry.title}
                title={entry.title}
                suffix={`+${entry.insertions} −${entry.deletions}`}
                checked={review?.selectedEditKey === `item:${entry.itemId}`}
                testId="review-edit-option"
                onSelect={() => { close(); setEdit(`item:${entry.itemId}`); }}
              />
            {/each}
          {/each}
        {/snippet}
      </ReviewToolbarMenu>
    {/if}

    <!-- The counts end the row, or the last row when the controls wrap on
         the phone, instead of truncating between them. -->
    <div class="ml-auto shrink-0 whitespace-nowrap text-right text-[0.6875rem] tabular-nums text-fg-muted compact:order-last">
      {#if review && !review.conflictView && diffFiles.length > 0}
        <span data-testid="review-diff-stats">
          {diffFiles.length} {diffFiles.length === 1 ? 'file' : 'files'}
          {#if totalAdditions > 0}<span class="text-success">+{totalAdditions}</span>{/if}
          {#if totalDeletions > 0}<span class="text-error">-{totalDeletions}</span>{/if}
        </span>
      {/if}
    </div>

    <button
      type="button"
      class="inline-flex h-7 w-7 compact:h-9 compact:w-9 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg disabled:opacity-50"
      aria-label={review?.allCollapsed ? 'Expand all files' : 'Collapse all files'}
      title={review?.allCollapsed ? 'Expand all files' : 'Collapse all files'}
      data-testid="review-collapse-all-toggle"
      disabled={!review || !!review.ciLogView || collapsibleFileCount === 0}
      onclick={() => { void review?.toggleCollapseAll(); }}
    >
      <Icon icon={review?.allCollapsed ? ChevronsUpDown : ChevronsDownUp} size={14} />
    </button>

    <!-- View settings a reader sets once: off the toolbar so the row fits
         a phone, each a checkbox row that stays open-able while the diff
         reloads under it. -->
    <button
      bind:this={moreTriggerEl}
      type="button"
      class="inline-flex h-7 w-7 compact:h-9 compact:w-9 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg disabled:opacity-50 {moreOpen ? 'bg-surface-2 text-fg' : ''}"
      aria-label="View options"
      title="View options"
      aria-haspopup="menu"
      aria-expanded={moreOpen}
      data-testid="review-view-options"
      disabled={!review}
      onclick={() => { moreOpen = !moreOpen; }}
    >
      <Icon icon={Ellipsis} size={14} />
    </button>
    <Popover anchor={moreTriggerEl} open={moreOpen} onClose={closeMore} placement="bottom-end" role="none">
      {#if review}
        <Menu ariaLabel="View options" onClose={closeMore} minWidthClass="min-w-[220px]">
          <MenuItem
            label="Hide whitespace changes"
            checkbox
            checked={review.ignoreWhitespace}
            disabled={!review.canIgnoreWhitespace || review.loading}
            title={!review.canIgnoreWhitespace ? 'Not available for this diff' : undefined}
            testId="review-ignore-whitespace-toggle"
            onSelect={() => pickViewOption(() => { void review?.setIgnoreWhitespace(!review.ignoreWhitespace); })}
          />
          <MenuItem
            label="Word wrap"
            checkbox
            checked={review.wordWrap}
            testId="review-wrap-toggle"
            onSelect={() => pickViewOption(() => review?.setWordWrap(!review.wordWrap))}
          />
          <MenuItem
            label="Split view"
            checkbox
            checked={review.viewMode === 'split'}
            testId="review-split-toggle"
            onSelect={() => pickViewOption(() => review?.setViewMode(review.viewMode === 'split' ? 'stacked' : 'split'))}
          />
        </Menu>
      {/if}
    </Popover>

    <button
      type="button"
      class="relative inline-flex h-7 w-7 compact:h-9 compact:w-9 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg disabled:opacity-50"
      aria-label="Reload review diff"
      title={diffStale ? 'Workspace changed — reload' : 'Reload'}
      data-testid="review-reload"
      data-stale={diffStale ? 'true' : undefined}
      disabled={!review || review.loading}
      onclick={() => { void review?.reload(); }}
    >
      <Icon icon={RefreshCw} size={14} class={review?.loading ? 'animate-spin' : ''} />
      {#if diffStale}
        <span
          class="absolute -right-0.5 -top-0.5 size-2 rounded-full bg-warning ring-2 ring-surface-1"
          data-testid="review-stale-dot"
          aria-hidden="true"
        ></span>
      {/if}
    </button>

    <!-- Close sits past a divider so it reads as "leave", not as one more
         view toggle in the run. Same close the header badge / chord perform;
         it exists because re-clicking the badge is not discoverable. -->
    <span class="h-[18px] w-px shrink-0 bg-border-subtle" aria-hidden="true"></span>
    <button
      type="button"
      class="inline-flex h-7 w-7 compact:h-9 compact:w-9 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg"
      aria-label="Close review pane"
      title="Close review pane"
      data-testid="review-close"
      onclick={() => ctx.close()}
    >
      <Icon icon={X} size={14} />
    </button>
  </div>

  {#if !review}
    <div class="p-3 text-sm text-fg-muted">No thread selected.</div>
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
      {:else if failure?.kind === 'setup'}
        <!-- A login to fix outside the app: the message names it, and no
             retry will change it until the user does. -->
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
        hasWorkspace={ctx.workspace !== null}
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
        onSend={() => { void review?.sendCILogToChat(); }}
        onToggleSection={(sectionKey) => review?.toggleCILogSection(sectionKey)}
        onSetSectionsOpen={(sectionKeys, open) => review?.setCILogSectionsOpen(sectionKeys, open)}
        onCopySection={(section) => review?.copyCILogSection(section) ?? Promise.resolve(false)}
        onSendSection={(section) => { void review?.sendCILogSectionToChat(section); }}
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
    {:else if review.files.length === 0}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-empty">No changed files.</div>
    {:else}
      <!-- Relative: on the phone the open rail lays over the diff. -->
      <div class="relative flex min-h-0 flex-1">
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
        {#if diffFiles.length === 0}
          <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-filter-empty">
            No files match the filter.
          </div>
        {:else}
        {#snippet leadRowContent()}
          {#if selectedCommit}
            <ReviewCommitCard commit={selectedCommit} ordinal={selectedCommitIndex + 1} count={review?.commits.length ?? 0} />
          {:else if review?.prDetail}
            <ReviewOverview {review} detail={review.prDetail} />
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
          onAddComment={ctx.threadId && !operateUngranted ? (anchor) => review?.openDraftEditor(anchor) : undefined}
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
            {#if review.scope === 'pr' && !review.selectedCommitSHA}
              <!-- Single-commit view: drafts anchor to that commit's diff,
                   not the PR head diff — PR submission would mis-place
                   them, so the only target is the linked agent. -->
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
              disabled={operateUngranted || review.sendingComments || (review.effectiveSubmitTarget === 'agent' && review.isTurnActive)}
              title={operateUngranted ? 'Not granted to this device' : review.isTurnActive && review.effectiveSubmitTarget === 'agent' ? 'Send from the chat box while the agent is working' : 'Send comments'}
              onclick={() => { void (review?.effectiveSubmitTarget === 'pr' ? review?.submitPRReview() : review?.sendComments()); }}
            >
              Send comments
            </button>
          </div>
        </div>
        {#if review.effectiveSubmitTarget === 'pr'}
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
    </RenderBoundary>
  {/if}
</section>
