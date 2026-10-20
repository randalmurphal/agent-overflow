<script lang="ts">
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import ReviewToolbarMenu from './ReviewToolbarMenu.svelte';
  import { relativeTime } from '../../utils/format';
  import type { BranchCommit } from '../../types/git';

  // The toolbar's commit picker. The trigger is one fixed width for the
  // list and never clips: "All commits", or the commit's ordinal and
  // short hash once one is picked (the subject is the hover text). The subject,
  // author and age live where there is room: one row per commit in the
  // menu, and the commit card that leads the diff (ReviewCommitCard).
  // The menu's header steps to the previous or next commit without
  // reading the list; the ends do not wrap.

  interface Props {
    /** Newest first, as the selector lists them. */
    commits: readonly BranchCommit[];
    selectedSha: string | null;
    disabled?: boolean;
    onSelect: (sha: string | null) => void;
  }

  let { commits, selectedSha, disabled = false, onSelect }: Props = $props();

  const selectedIndex = $derived(selectedSha ? commits.findIndex((commit) => commit.sha === selectedSha) : -1);
  const selected = $derived(selectedIndex >= 0 ? commits[selectedIndex] : null);
  const title = $derived(selected ? `${selected.shortSha} ${selected.subject}` : `All commits (${commits.length})`);
  // Git abbreviates every hash of one log to the same length unless one
  // needs more; the longest sizes the trigger's ghost label.
  const widestShortSha = $derived(
    commits.reduce((widest, commit) => (commit.shortSha.length > widest.length ? commit.shortSha : widest), ''),
  );

  function age(commit: BranchCommit): string {
    return commit.authoredAt > 0 ? relativeTime(commit.authoredAt) : '';
  }

  // Stepping from "All commits" starts at the newest.
  function step(direction: 1 | -1): void {
    const next = selectedIndex < 0 ? (direction > 0 ? 0 : commits.length - 1) : selectedIndex + direction;
    const commit = commits[next];
    if (commit) onSelect(commit.sha);
  }
  const canStepPrev = $derived(selectedIndex > 0 || (selectedIndex < 0 && commits.length > 0));
  const canStepNext = $derived(selectedIndex < 0 ? commits.length > 0 : selectedIndex < commits.length - 1);

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
         pick never rewraps the toolbar: both labels are laid out in the
         same grid cell, the widest sets the size, the live one shows. The
         ghosts are pseudo-element text, so they are no part of the
         trigger's text content. -->
    <span class="grid">
      <span class="invisible col-start-1 row-start-1 whitespace-nowrap after:content-[attr(data-ghost)]" aria-hidden="true" data-ghost="All commits"></span>
      <span class="invisible col-start-1 row-start-1 whitespace-nowrap" aria-hidden="true">
        <span class="text-[0.6875rem] tabular-nums after:content-[attr(data-ghost)]" data-ghost="{commits.length}/{commits.length}"></span>
        <span class="font-mono text-[0.625rem] after:content-[attr(data-ghost)]" data-ghost={widestShortSha}></span>
      </span>
      <span class="col-start-1 row-start-1 whitespace-nowrap">
        {#if selected}
          <span class="text-[0.6875rem] tabular-nums text-fg-muted" data-testid="review-commit-ordinal">{selectedIndex + 1}/{commits.length}</span>
          <span class="font-mono text-[0.625rem]" data-testid="review-commit-hash">{selected.shortSha}</span>
        {:else}
          All commits
        {/if}
      </span>
    </span>
  {/snippet}
  {#snippet header()}
    <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted">
      <span class="min-w-0 flex-1 truncate" data-testid="review-commit-position">
        {#if selected}Commit {selectedIndex + 1} of {commits.length}{:else}{commits.length} {commits.length === 1 ? 'commit' : 'commits'}{/if}
      </span>
      <button type="button" class={stepClass} aria-label="Previous commit" title="Previous commit" data-testid="review-commit-prev" disabled={!canStepPrev} onclick={() => step(-1)}>
        <Icon icon={ChevronLeft} size={13} />
      </button>
      <button type="button" class={stepClass} aria-label="Next commit" title="Next commit" data-testid="review-commit-next" disabled={!canStepNext} onclick={() => step(1)}>
        <Icon icon={ChevronRight} size={13} />
      </button>
    </div>
  {/snippet}
  {#snippet children(close)}
    <MenuItem
      label="All commits"
      suffix={String(commits.length)}
      checked={selected === null}
      testId="review-commit-all"
      onSelect={() => { close(); onSelect(null); }}
    />
    <MenuDivider />
    {#each commits as commit, index (commit.sha)}
      <MenuItem
        label={commit.subject}
        title={commit.subject}
        suffix={age(commit)}
        checked={index === selectedIndex}
        testId="review-commit-option"
        onSelect={() => { close(); onSelect(commit.sha); }}
      >
        {#snippet labelContent()}
          <span class="mr-2 font-mono text-[0.6875rem] text-fg-muted">{commit.shortSha}</span>{commit.subject}
        {/snippet}
      </MenuItem>
    {/each}
  {/snippet}
</ReviewToolbarMenu>
