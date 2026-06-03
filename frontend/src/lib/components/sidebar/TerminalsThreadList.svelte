<script lang="ts">
  // Flat thread list for the standalone Terminals group. Deliberately a
  // simpler sibling of ProjectThreadList: terminals have no discussion
  // children and no live run-status, so this skips the tree build, the
  // per-discussion expand/collapse machinery, and the status-rollup pills —
  // it just slices a flat array to the shared preview limit and offers the
  // same Show More / Show Less reveal. The indent rail, indentation, row
  // component, and the visible-limit store helpers are shared with the project
  // list so the two read as one UI. (Kept separate, not folded into
  // ProjectThreadList, so terminals don't drag the discussion-tree code path.)

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    collapseThreadList,
    getThreadListVisibleLimit,
    isThreadListExpanded,
    revealMoreThreadList,
  } from '../../stores/sidebar.svelte';
  import {
    THREAD_PREVIEW_LIMIT,
    THREAD_REVEAL_INCREMENT,
  } from '../../utils/sidebarThreadLimits';
  import { autoAnimate } from '../../utils/autoAnimate';
  import Terminal from 'lucide-svelte/icons/terminal';
  import Icon from '../primitives/Icon.svelte';
  import ThreadRow from './ThreadRow.svelte';
  import type { ProjectNewTerminalHandler } from './projectNewThread';

  // Synthetic key into the shared per-list visible-limit store. Project ids are
  // UUIDs, so this sentinel never collides; it gives the Terminals list its own
  // persisted Show More state.
  const TERMINALS_LIST_KEY = '__terminals__';

  interface Props {
    /** Terminal threads to show, already search-filtered + ordered by caller. */
    terminals: Thread[];
    pane: ThreadPane | null;
    /** Opens a fresh terminal — used by the empty-state button. */
    onNewTerminal: ProjectNewTerminalHandler;
  }

  let { terminals, pane, onNewTerminal }: Props = $props();

  let visibleLimit = $derived(getThreadListVisibleLimit(TERMINALS_LIST_KEY));
  let listExpanded = $derived(isThreadListExpanded(TERMINALS_LIST_KEY));

  let visibleTerminals = $derived(terminals.slice(0, visibleLimit));
  let hiddenCount = $derived(Math.max(0, terminals.length - visibleLimit));
  let nextRevealCount = $derived(Math.min(THREAD_REVEAL_INCREMENT, hiddenCount));

  function handleShowMore(e: MouseEvent): void {
    e.stopPropagation();
    revealMoreThreadList(TERMINALS_LIST_KEY);
  }

  function handleShowLess(e: MouseEvent): void {
    e.stopPropagation();
    collapseThreadList(TERMINALS_LIST_KEY);
  }

  function handleEmptyNewTerminal(): void {
    void onNewTerminal();
  }
</script>

{#if terminals.length === 0}
  <button
    type="button"
    onclick={handleEmptyNewTerminal}
    data-testid="terminals-thread-list-empty"
    class="ml-4 mr-2 my-1 inline-flex items-center gap-1 rounded-[var(--radius-field)] px-2 py-1 text-[0.6875rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  >
    <Icon icon={Terminal} size={11} strokeWidth={2.2} class="opacity-80" />
    <span>New Terminal</span>
  </button>
{:else}
  <div
    class="flex flex-col gap-px ml-2 border-l border-border-subtle/60"
    role="list"
    aria-label="Terminals"
    data-testid="terminals-thread-list"
    use:autoAnimate
  >
    {#each visibleTerminals as terminal (terminal.id)}
      <div role="listitem">
        <ThreadRow thread={terminal} {pane} indent={1} />
      </div>
    {/each}

    {#if hiddenCount > 0 || (listExpanded && terminals.length > THREAD_PREVIEW_LIMIT)}
      <div class="flex items-center gap-1 mr-1">
        {#if hiddenCount > 0}
          <button
            type="button"
            onclick={handleShowMore}
            data-testid="terminals-thread-list-show-more"
            class="flex items-center gap-1.5 h-6 pl-6 pr-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          >
            Show {nextRevealCount} More{hiddenCount > THREAD_REVEAL_INCREMENT ? ` (${hiddenCount})` : ''}
          </button>
        {/if}

        {#if listExpanded && terminals.length > THREAD_PREVIEW_LIMIT}
          <button
            type="button"
            onclick={handleShowLess}
            data-testid="terminals-thread-list-show-less"
            class="flex items-center h-6 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          >
            Show Less
          </button>
        {/if}
      </div>
    {/if}
  </div>
{/if}
