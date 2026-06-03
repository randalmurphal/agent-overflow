<script lang="ts">
  // Standalone "Terminals" group: project-less "home" terminals plus a
  // +terminal action, rendered as a project-style row directly beneath the
  // project list (it flows with the projects in the same scroll region — it is
  // NOT pinned to the sidebar's bottom edge). Per-project terminals are NOT
  // shown here; they live mixed into their project's thread list
  // (leading-icon-distinguished). This group only collects terminals with no
  // project.
  //
  // The header mirrors ProjectItem (chevron + leading icon + title + hover
  // action) minus the project-only machinery: no drag-reorder, rename, context
  // menu, or status-rollup dot — terminals don't reorder and have no live run
  // status. Expanding reveals TerminalsThreadList, which shares the project
  // list's indent rail + Show More. A separator above sets the group apart from
  // the projects, per the chosen UX.

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    isTerminalsGroupExpanded,
    toggleTerminalsGroup,
  } from '../../stores/sidebar.svelte';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Terminal from 'lucide-svelte/icons/terminal';
  import Icon from '../primitives/Icon.svelte';
  import TerminalsThreadList from './TerminalsThreadList.svelte';
  import type { ProjectNewTerminalHandler } from './projectNewThread';

  interface Props {
    /** Project-less terminal threads, already search-filtered by the caller. */
    terminals: Thread[];
    pane: ThreadPane | null;
    /** Opens a fresh home terminal (no project id). */
    onNewTerminal: ProjectNewTerminalHandler;
  }

  let { terminals, pane, onNewTerminal }: Props = $props();

  let expanded = $derived(isTerminalsGroupExpanded());

  function handleToggle(e: MouseEvent): void {
    e.stopPropagation();
    toggleTerminalsGroup();
  }

  function handleRowKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      toggleTerminalsGroup();
    }
  }

  function handleNewTerminalClick(e: MouseEvent): void {
    e.stopPropagation();
    void onNewTerminal();
  }
</script>

<!--
  Plain <div>, not a <section>: this group renders INSIDE ProjectsSection's
  <section aria-label="Projects">, and a nested labelled region would wrongly
  imply these home terminals are project-scoped. The row's accessible name
  comes from its aria-label below. px-2 matches ProjectList's list padding so
  the row + indent rail align with the project rows above.
-->
<div
  class="px-2 mt-1 pt-1 border-t border-border-subtle/60"
  data-testid="sidebar-terminals-group"
>
  <div
    role="button"
    tabindex={0}
    aria-expanded={expanded}
    aria-label={expanded ? 'Collapse Terminals' : 'Expand Terminals'}
    onclick={handleToggle}
    onkeydown={handleRowKeydown}
    data-testid="sidebar-terminals-row"
    class="group flex items-center gap-1.5 px-2 py-1 rounded-[var(--radius-field)] text-fg hover:bg-surface-2/40 transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  >
    <div class="flex h-4 w-4 items-center justify-center shrink-0">
      <button
        type="button"
        onclick={handleToggle}
        aria-label={expanded ? 'Collapse Terminals' : 'Expand Terminals'}
        aria-expanded={expanded}
        data-testid="sidebar-terminals-chevron"
        class="flex h-4 w-4 items-center justify-center rounded text-fg-subtle hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40"
      >
        <Icon
          icon={ChevronRight}
          size={11}
          strokeWidth={2.5}
          class={'opacity-80 transition-transform ' + (expanded ? 'rotate-90' : '')}
        />
      </button>
    </div>
    <Icon icon={Terminal} size={13} strokeWidth={2} class="shrink-0 text-ico-terminal" />
    <span class="text-[0.78125rem] font-medium truncate flex-1 text-fg select-none">
      Terminals
    </span>
    <button
      type="button"
      onclick={handleNewTerminalClick}
      title="New Terminal"
      aria-label="New Terminal"
      data-testid="sidebar-new-terminal-global"
      class="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity ml-1 shrink-0 flex h-5 w-5 items-center justify-center rounded text-fg-subtle hover:text-fg hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    >
      <Icon icon={Terminal} size={12} strokeWidth={2} class="opacity-90" />
    </button>
  </div>

  {#if expanded}
    <TerminalsThreadList {terminals} {pane} {onNewTerminal} />
  {/if}
</div>
