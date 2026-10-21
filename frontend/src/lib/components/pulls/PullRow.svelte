<script lang="ts">
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import GitFork from '@lucide/svelte/icons/git-fork';
  import Icon from '../primitives/Icon.svelte';
  import { ciStatusDotClass, ciStatusText } from '../../utils/ciStatus';
  import type { ReadinessTone } from '../../utils/reviewReadiness';
  import { pullNumberLabel, type PullRowView } from './pullRowView';

  // One merge request in the page's list, read top down: the project,
  // number and author with the age at the right; the title, up to two
  // lines; then the facts, wrapping and never cut: the state label in its
  // tone (why the row is where it is), the approvals, the unresolved
  // threads and, on the reader's own MRs, CI. The state's tone also
  // colours the bar at the left. The row is a button so the list is one
  // tab stop per MR; the menu lives on the trailing button the page adds.

  interface Props {
    row: PullRowView;
    selected: boolean;
    onOpen: (row: PullRowView, event: MouseEvent | KeyboardEvent) => void;
    /** The page's row menu trigger (Open with agent, Open on forge, Copy URL). */
    onMenu?: (row: PullRowView, anchor: HTMLElement) => void;
  }

  let { row, selected, onOpen, onMenu }: Props = $props();

  const TONE_TEXT: Record<ReadinessTone, string> = {
    accent: 'text-accent',
    error: 'text-error',
    success: 'text-success',
    warning: 'text-warning',
    muted: 'text-fg-subtle',
  };
  const TONE_BAR: Record<ReadinessTone, string> = {
    accent: 'bg-accent',
    error: 'bg-error',
    success: 'bg-success',
    warning: 'bg-warning',
    muted: 'bg-fg-subtle/40',
  };

  const approvalsMet = $derived(row.approvals !== null && row.approvals.given >= row.approvals.required);
  let menuEl: HTMLButtonElement | undefined = $state(undefined);
</script>

<div
  class="group/pull relative flex items-stretch border-b border-border-subtle transition-colors
    {selected ? 'bg-accent/8' : 'hover:bg-surface-1/60'}"
  data-testid="pull-row"
  data-pr-key={row.key}
  data-pull-state={row.state}
  data-selected={selected ? 'true' : undefined}
>
  <span class="my-[7px] w-0.5 shrink-0 rounded-full {selected ? 'bg-accent' : TONE_BAR[row.tone]}" aria-hidden="true"></span>
  <button
    type="button"
    class="flex min-w-0 flex-1 flex-col gap-[3px] py-[7px] pr-3 pl-2.5 text-left compact:select-none compact:py-2.5
      focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40"
    aria-current={selected ? 'true' : undefined}
    onclick={(event) => onOpen(row, event)}
    onkeydown={(event) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        onOpen(row, event);
      }
    }}
  >
    <span class="flex min-w-0 items-center gap-1.5 text-[0.6875rem] tabular-nums">
      <span class="shrink-0 truncate rounded bg-surface-2 px-1.5 leading-4 text-fg" data-testid="pull-project">{row.projectName}</span>
      <span class="shrink-0 text-fg-muted">{pullNumberLabel(row.forge, row.number)}</span>
      <span class="min-w-0 truncate text-fg-muted" data-testid="pull-author">{row.author}</span>
      {#if row.crossRepository}
        <span class="inline-flex shrink-0 items-center text-fg-subtle" title="From a fork">
          <Icon icon={GitFork} size={11} strokeWidth={1.75} />
        </span>
      {/if}
      <span class="ml-auto shrink-0 pl-2 text-fg-subtle" data-testid="pull-age">{row.age}</span>
    </span>
    <span class="line-clamp-2 text-[0.8125rem] font-medium leading-[1.35] text-fg">{row.title}</span>
    <span class="flex min-w-0 flex-wrap items-center gap-x-3.5 gap-y-0.5 text-[0.6875rem] text-fg-muted">
      <span class="font-medium {TONE_TEXT[row.tone]}" data-testid="pull-state">{row.label}</span>
      {#if row.approvals}
        <span class="tabular-nums {approvalsMet ? 'text-success' : ''}" data-testid="pull-approvals">{row.approvals.given}/{row.approvals.required} approvals</span>
      {/if}
      {#if row.unresolved}
        <span class="text-warning" data-testid="pull-unresolved">{row.unresolved} unresolved</span>
      {/if}
      {#if row.viewerIsAuthor && row.ci !== ''}
        <span class="inline-flex items-center gap-1.5" data-testid="pull-ci">
          <span class="h-1.5 w-1.5 rounded-full {ciStatusDotClass(row.ci)}" aria-hidden="true"></span>CI {ciStatusText(row.ci)}
        </span>
      {/if}
    </span>
  </button>
  {#if onMenu}
    <button
      bind:this={menuEl}
      type="button"
      class="mr-1 mt-1.5 inline-flex h-6 w-6 shrink-0 cursor-pointer items-center justify-center self-start rounded text-fg-subtle opacity-0 transition-opacity
        hover:bg-surface-2/40 hover:text-fg focus-visible:opacity-100 group-hover/pull:opacity-100 compact:h-9 compact:w-9 compact:opacity-100
        focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      aria-label="More actions for {pullNumberLabel(row.forge, row.number)}"
      data-testid="pull-row-menu"
      onclick={(event) => {
        event.stopPropagation();
        if (menuEl) onMenu(row, menuEl);
      }}
    >
      <Icon icon={Ellipsis} size={14} strokeWidth={1.75} />
    </button>
  {/if}
</div>
