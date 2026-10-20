<script lang="ts">
  import { untrack } from 'svelte';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronsDownUp from '@lucide/svelte/icons/chevrons-down-up';
  import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import X from '@lucide/svelte/icons/x';
  import ReviewCommitPicker from './ReviewCommitPicker.svelte';
  import ReviewToolbarMenu from './ReviewToolbarMenu.svelte';
  import type { ReviewPaneState, ReviewScope } from '../../stores/reviewPane.svelte';
  import type { GitBranch, GitStatus, WorkspaceRef } from '../../types/git';
  import { truncateMiddle } from '../../utils/format';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../primitives/MenuSectionHeader.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';

  // The review pane's toolbar: the scope selectors, the diff's counts,
  // collapse-all, the view-options menu, reload, and close (or Back on a
  // review pinned to one PR, which has no other scope to select).

  interface Props {
    review: ReviewPaneState | null;
    /** The checkout the worktree scopes diff. */
    workspace: WorkspaceRef | null;
    /** The thread row; the edits scope needs one. */
    threadId: string | null;
    /** The repository's branches, for the branch scope's base menu. */
    branches: readonly GitBranch[];
    /** The source pane's git status, for the worktree scopes' stale dot;
     * null for a review with no source pane. */
    gitStatus: GitStatus | null;
    /** What the diff body shows (the tree filter may narrow it). */
    stats: { files: number; additions: number; deletions: number };
    collapsibleFileCount: number;
    onClose?: () => void;
    /** A pinned review returns to its page's list instead of closing. */
    onBack?: () => void;
  }

  let { review, workspace, threadId, branches, gitStatus, stats, collapsibleFileCount, onClose, onBack }: Props = $props();

  // The "..." menu: view toggles that are set once per reading session
  // (whitespace, wrap, split), off the toolbar so the row fits a phone.
  let moreOpen = $state(false);
  let moreTriggerEl: HTMLButtonElement | undefined = $state(undefined);

  function setScope(value: ReviewScope): void {
    if (!review) return;
    void review.setScope(value);
  }

  function setBaseBranch(value: string): void {
    if (!review) return;
    void review.setScope('branch', { baseBranch: value });
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

  // The scopes this pane can show: worktree scopes need a workspace, edits
  // a thread, the PR an open pull request. A pinned review shows its PR
  // alone, so it has no selector.
  const scopeOptions = $derived.by((): { value: ReviewScope; label: string }[] => {
    const options: { value: ReviewScope; label: string }[] = [];
    if (review?.pinned) return options;
    if (workspace) options.push({ value: 'workspace', label: 'Workspace' }, { value: 'branch', label: 'Branch' });
    if (threadId) options.push({ value: 'edits', label: 'Edits' });
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

  const liveGitSignature = $derived(gitStatusSignature(gitStatus));
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

<!-- Wraps: a narrow pane carries the counts and the icon run on a second
     row instead of clipping them. -->
<div class="flex shrink-0 flex-wrap items-center gap-2 border-b border-border-subtle px-3 py-2" data-testid="review-toolbar">
  {#if onBack}
    <button
      type="button"
      class="inline-flex h-7 w-7 compact:h-9 compact:w-9 shrink-0 items-center justify-center rounded-[var(--radius-field)] border border-border-subtle text-fg-muted hover:text-fg"
      aria-label="Back to the list"
      title="Back to the list"
      data-testid="review-back"
      onclick={() => onBack?.()}
    >
      <Icon icon={ChevronLeft} size={14} />
    </button>
  {/if}

  {#if scopeOptions.length > 0}
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
  {/if}

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
    {#if review && !review.conflictView && stats.files > 0}
      <span data-testid="review-diff-stats">
        {stats.files} {stats.files === 1 ? 'file' : 'files'}
        {#if stats.additions > 0}<span class="text-success">+{stats.additions}</span>{/if}
        {#if stats.deletions > 0}<span class="text-error">-{stats.deletions}</span>{/if}
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

  {#if onClose}
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
      onclick={() => onClose?.()}
    >
      <Icon icon={X} size={14} />
    </button>
  {/if}
</div>
