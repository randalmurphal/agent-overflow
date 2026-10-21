<script lang="ts">
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import GitFork from '@lucide/svelte/icons/git-fork';
  import Icon from '../primitives/Icon.svelte';
  import { READINESS_PILL_CLASS } from '../../utils/reviewReadiness';
  import { pullRowQuiet, pullStatePill, sinceYouLookedText } from '../../utils/pullsAttention';
  import { ciStatusDotClass } from '../../utils/ciStatus';
  import { pullNumberLabel, type PullRowView } from './pullRowView';

  // One merge request in the page's list: number and title, then the
  // state pill with what moved since the reader looked, then the facts
  // (project, author, age, CI, approvals, unresolved). A seen row with
  // nothing new dims; the selected row carries the accent bar the sidebar
  // uses for the focused thread. The row is a button so the list is one
  // tab stop per MR; the menu lives on the trailing button the page adds.

  interface Props {
    row: PullRowView;
    selected: boolean;
    onOpen: (row: PullRowView, event: MouseEvent | KeyboardEvent) => void;
    /** The page's row menu trigger (Open with agent, Open on forge, Copy URL). */
    onMenu?: (row: PullRowView, anchor: HTMLElement) => void;
  }

  let { row, selected, onOpen, onMenu }: Props = $props();

  const pill = $derived(pullStatePill(row.state));
  const since = $derived(sinceYouLookedText(row.since));
  const quiet = $derived(pullRowQuiet(row.seen, row.since));
  const ciLabel = $derived(row.ci === '' ? 'no CI' : 'CI');
  let menuEl: HTMLButtonElement | undefined = $state(undefined);
</script>

<div
  class="group/pull relative flex items-stretch border-b border-border-subtle transition-colors
    {selected ? 'bg-accent/8' : 'hover:bg-surface-1/60'} {quiet && !selected ? 'opacity-60' : ''}"
  data-testid="pull-row"
  data-pr-key={row.key}
  data-pull-state={row.state}
  data-selected={selected ? 'true' : undefined}
>
  {#if selected}
    <span class="absolute inset-y-0 left-0 w-0.5 bg-accent" aria-hidden="true"></span>
  {/if}
  <button
    type="button"
    class="flex min-w-0 flex-1 flex-col gap-1 px-3 py-2 text-left compact:select-none compact:py-2.5
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
    <span class="flex min-w-0 items-baseline gap-2">
      <span class="shrink-0 font-mono text-[0.6875rem] text-fg-subtle">{pullNumberLabel(row.forge, row.number)}</span>
      <span class="min-w-0 truncate text-[0.8125rem] font-medium text-fg">{row.title}</span>
      {#if row.crossRepository}
        <span class="inline-flex shrink-0 items-center text-fg-subtle" title="From a fork">
          <Icon icon={GitFork} size={11} strokeWidth={1.75} />
        </span>
      {/if}
    </span>
    <span class="flex min-w-0 items-center gap-2 text-[0.6875rem]">
      <span class="inline-flex shrink-0 items-center rounded-full px-1.5 py-px font-medium {READINESS_PILL_CLASS[pill.tone]}" data-testid="pull-state-pill">{pill.label}</span>
      {#if since}
        <span class="min-w-0 truncate text-fg-muted" data-testid="pull-since">{since}</span>
      {/if}
    </span>
    <span class="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[0.6875rem] text-fg-muted">
      <span class="truncate text-fg-subtle">{row.projectName}</span>
      <span class="truncate">{row.author}</span>
      <span class="shrink-0">{row.age}</span>
      <span class="inline-flex shrink-0 items-center gap-1" data-testid="pull-ci">
        <span class="h-1.5 w-1.5 rounded-full {ciStatusDotClass(row.ci)}" aria-hidden="true"></span>{ciLabel}
      </span>
      {#if row.approvals}
        <span class="shrink-0 tabular-nums" data-testid="pull-approvals">approvals {row.approvals.given}/{row.approvals.required}</span>
      {/if}
      {#if row.unresolved}
        <span class="shrink-0 text-warning" data-testid="pull-unresolved">{row.unresolved} unresolved</span>
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
