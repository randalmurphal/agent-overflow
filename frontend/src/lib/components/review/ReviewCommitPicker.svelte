<script lang="ts">
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import ReviewToolbarMenu from './ReviewToolbarMenu.svelte';
  import { relativeTime } from '../../utils/format';
  import {
    commitOrdinal,
    commitSelectionBetween,
    commitSelectionForClick,
    commitSelectionFromHere,
    commitSelectionIndexes,
    commitSelectionLabel,
    type CommitSelection,
  } from '../../utils/commitRange';
  import type { BranchCommit } from '../../types/git';

  // The toolbar's commit picker. The trigger is one fixed width for the
  // list and never clips: "All commits", the commit's ordinal and short
  // hash (`N/T sha`), or a run's ordinals (`N–M/T`); ordinals count from
  // the oldest commit to the head. Subjects are hover text only; the
  // subject, author and age live in the menu rows and the lead card
  // (ReviewCommitCard, ReviewRangeCard). With `ranges` (pr scope),
  // shift+click on a row spans from the picked commit to it, and a row's
  // `From here` picks it through the newest. The header steps to the
  // older or newer commit; the ends do not wrap.

  interface Props {
    /** Newest first, as the selector lists them. */
    commits: readonly BranchCommit[];
    selection: CommitSelection | null;
    /** Runs of commits can be picked (pr scope). */
    ranges?: boolean;
    disabled?: boolean;
    onSelect: (selection: CommitSelection | null) => void;
  }

  let { commits, selection, ranges = false, disabled = false, onSelect }: Props = $props();

  const at = $derived(commitSelectionIndexes(commits, selection));
  const single = $derived(at !== null && at.newer === at.older ? commits[at.newer] : null);
  const count = $derived(commits.length);
  const triggerText = $derived(commitSelectionLabel(commits, selection));
  const title = $derived.by(() => {
    if (!at) return `All commits (${count})`;
    const newer = commits[at.newer];
    if (at.newer === at.older) return `${newer.shortSha} ${newer.subject}`;
    return `${commits[at.older].shortSha}–${newer.shortSha} (${at.older - at.newer + 1} commits)`;
  });
  // Git abbreviates every hash of one log to the same length unless one
  // needs more; the longest sizes the trigger's ghost label.
  const widestShortSha = $derived(
    commits.reduce((widest, commit) => (commit.shortSha.length > widest.length ? commit.shortSha : widest), ''),
  );

  function age(commit: BranchCommit): string {
    return commit.authoredAt > 0 ? relativeTime(commit.authoredAt) : '';
  }

  // Older (left) and newer (right) by ordinal. From "All commits" older
  // starts at the head and newer at the first commit; from a run, the
  // commit past its older or newer end.
  function step(direction: 1 | -1): void {
    const index = !at
      ? (direction < 0 ? 0 : count - 1)
      : direction < 0 ? at.older + 1 : at.newer - 1;
    const commit = commits[index];
    if (commit) onSelect(commitSelectionBetween(commits, commit.sha, commit.sha));
  }
  const canStepOlder = $derived(!at ? count > 0 : at.older < count - 1);
  const canStepNewer = $derived(!at ? count > 0 : at.newer > 0);

  // The modifier of the click (or key) that picks a row: MenuItem reports
  // only that a row was chosen.
  let shiftHeld = false;
  function noteModifier(event: MouseEvent | KeyboardEvent): void {
    shiftHeld = event.shiftKey;
  }

  function inSelection(index: number): boolean {
    return at !== null && index >= at.newer && index <= at.older;
  }

  const stepClass = 'inline-flex size-6 items-center justify-center rounded-[var(--radius-field)] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-40 disabled:hover:bg-transparent compact:size-8';
</script>

<ReviewToolbarMenu
  ariaLabel="Commit"
  {title}
  {disabled}
  testId="review-commit-select"
  minWidthClass="min-w-[320px]"
>
  {#snippet label()}
    <!-- The trigger keeps one width across every state of this list, so a
         pick never rewraps the toolbar: every label is laid out in the
         same grid cell, the widest sets the size, the live one shows. The
         ghosts are pseudo-element text, so they are no part of the
         trigger's text content. -->
    <span class="grid">
      <span class="invisible col-start-1 row-start-1 whitespace-nowrap after:content-[attr(data-ghost)]" aria-hidden="true" data-ghost="All commits"></span>
      <span class="invisible col-start-1 row-start-1 whitespace-nowrap" aria-hidden="true">
        <span class="text-[0.6875rem] tabular-nums after:content-[attr(data-ghost)]" data-ghost="{count}/{count}"></span>
        <span class="font-mono text-[0.625rem] after:content-[attr(data-ghost)]" data-ghost={widestShortSha}></span>
      </span>
      {#if ranges}
        <span class="invisible col-start-1 row-start-1 whitespace-nowrap text-[0.6875rem] tabular-nums after:content-[attr(data-ghost)]" aria-hidden="true" data-ghost="{count}–{count}/{count}"></span>
      {/if}
      <span class="col-start-1 row-start-1 whitespace-nowrap">
        {#if single}
          <span class="text-[0.6875rem] tabular-nums text-fg-muted" data-testid="review-commit-ordinal">{triggerText}</span>
          <span class="font-mono text-[0.625rem]" data-testid="review-commit-hash">{single.shortSha}</span>
        {:else if at}
          <span class="text-[0.6875rem] tabular-nums" data-testid="review-commit-ordinal">{triggerText}</span>
        {:else}
          All commits
        {/if}
      </span>
    </span>
  {/snippet}
  {#snippet header()}
    <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted">
      <span class="min-w-0 flex-1 truncate" data-testid="review-commit-position">
        {#if at && at.newer === at.older}Commit {commitOrdinal(at.newer, count)} of {count}
        {:else if at}Commits {commitOrdinal(at.older, count)}–{commitOrdinal(at.newer, count)} of {count}
        {:else}{count} {count === 1 ? 'commit' : 'commits'}{/if}
      </span>
      {#if ranges && count > 1}
        <span class="hidden shrink-0 text-fg-hint [@media(hover:hover)]:inline" data-testid="review-commit-range-hint">Shift+click for a range</span>
      {/if}
      <button type="button" class={stepClass} aria-label="Older commit" title="Older commit" data-testid="review-commit-prev" disabled={disabled || !canStepOlder} onclick={() => step(-1)}>
        <Icon icon={ChevronLeft} size={13} />
      </button>
      <button type="button" class={stepClass} aria-label="Newer commit" title="Newer commit" data-testid="review-commit-next" disabled={disabled || !canStepNewer} onclick={() => step(1)}>
        <Icon icon={ChevronRight} size={13} />
      </button>
    </div>
  {/snippet}
  {#snippet children(close)}
    <MenuItem
      label="All commits"
      suffix={String(count)}
      checked={at === null}
      {disabled}
      testId="review-commit-all"
      onSelect={() => { close(); onSelect(null); }}
    />
    <MenuDivider />
    <div onclickcapture={noteModifier} onkeydowncapture={noteModifier} role="none">
      {#each commits as commit, index (commit.sha)}
        <MenuItem
          label={commit.subject}
          title={commit.subject}
          suffix={age(commit)}
          checked={inSelection(index)}
          {disabled}
          testId="review-commit-option"
          actionText={ranges && index < count - 1 ? 'From here' : undefined}
          actionTitle="This commit through the newest"
          actionLabel="Changes from {commit.shortSha} through the newest"
          actionReveal="hover"
          onAction={ranges && index < count - 1 ? () => { close(); onSelect(commitSelectionFromHere(commits, commit.sha)); } : undefined}
          onSelect={() => {
            const next = commitSelectionForClick(commits, selection, commit.sha, ranges && shiftHeld);
            shiftHeld = false;
            close();
            onSelect(next);
          }}
        >
          {#snippet labelContent()}
            <span class="mr-2 font-mono text-[0.6875rem] text-fg-muted">{commit.shortSha}</span>{commit.subject}
          {/snippet}
        </MenuItem>
      {/each}
    </div>
  {/snippet}
</ReviewToolbarMenu>
