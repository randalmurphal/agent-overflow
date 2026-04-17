<script lang="ts">
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import {
    filterThreads,
    getSelectedThreadIds,
    toggleThreadSelection,
    isThreadSelected,
  } from '../../stores/threadFilter.svelte';
  import {
    ensureExpanded,
    getExpandedParents,
    toggleParent,
  } from '../../stores/threadTree.svelte';
  import { buildDisplayRows, defaultExpandedParents, type ThreadDisplayRow } from '../../utils/threadTree';
  import ThreadRow from './ThreadRow.svelte';
  import VirtualList from '../shared/VirtualList.svelte';

  let {
    pane,
    onStartDiscussion,
  }: {
    pane: ThreadPane;
    onStartDiscussion?: (thread: Thread) => void;
  } = $props();

  let threads = $derived(filterThreads(getThreads()));
  let selected = $derived(getSelectedThreadIds());
  let lastClickedId: string | null = $state(null);

  // Auto-expand the parent of whichever thread is active so the user
  // never sees a collapsed tree hiding the thread they just opened.
  // Additive only — collapse is strictly user-driven via the chevron.
  $effect(() => {
    const activeId = pane.threadId;
    const expansions = defaultExpandedParents(threads, activeId);
    if (expansions.size > 0) ensureExpanded(expansions);
  });

  // Flatten the filtered thread list into nested rows. Children follow
  // their parent; orphans (parent filtered out of the visible set) fall
  // back to top-level so the user can still reach them.
  let rows = $derived<ThreadDisplayRow[]>(
    buildDisplayRows(threads, getExpandedParents()),
  );

  // ThreadRow renders two lines (title + relative timestamp) with
  // px-3 py-2 padding, which measures ~56px on the current Tailwind v4
  // theme. Keep this in sync with the ThreadRow outer container if its
  // density changes — the virtualized rows are positioned absolutely
  // and need a fixed height for math to work.
  const ROW_HEIGHT = 56;

  function handleRowSelectClick(
    thread: Thread,
    modifier: 'toggle' | 'range' | 'single' | null,
  ): boolean {
    // Returns true if the click was handled as a selection action (so the row
    // should NOT trigger a thread switch). Single click (no modifier) returns
    // false so the thread-switch path runs.
    if (modifier === null || modifier === 'single') {
      lastClickedId = thread.id;
      return false;
    }
    if (modifier === 'toggle') {
      toggleThreadSelection(thread.id);
      lastClickedId = thread.id;
      return true;
    }
    if (modifier === 'range') {
      if (!lastClickedId) {
        toggleThreadSelection(thread.id);
        lastClickedId = thread.id;
        return true;
      }
      const ids = threads.map((t) => t.id);
      const a = ids.indexOf(lastClickedId);
      const b = ids.indexOf(thread.id);
      if (a === -1 || b === -1) {
        toggleThreadSelection(thread.id);
        lastClickedId = thread.id;
        return true;
      }
      const [lo, hi] = a < b ? [a, b] : [b, a];
      for (let i = lo; i <= hi; i += 1) {
        if (!isThreadSelected(ids[i])) toggleThreadSelection(ids[i]);
      }
      return true;
    }
    return false;
  }
</script>

{#if rows.length === 0}
  <div class="flex-1 overflow-y-auto px-2 py-2" role="list" aria-label="Threads">
    <div class="mx-1 mt-3 rounded-2xl border border-dashed border-border/70 bg-surface-0/45 px-4 py-8 text-center shadow-[inset_0_1px_0_rgba(255,255,255,0.02)]">
      <div class="mx-auto flex h-10 w-10 items-center justify-center rounded-2xl border border-border/60 bg-surface-1/80 text-text-secondary/70">
        <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
        </svg>
      </div>
      <p class="mt-3 text-sm font-medium text-text-primary">No threads here</p>
      <p class="mt-1 text-xs text-text-secondary/70">Adjust the search or archived filter, or create a new thread.</p>
    </div>
  </div>
{:else}
  <VirtualList
    items={rows}
    rowHeight={ROW_HEIGHT}
    ariaLabel="Threads"
    class="flex-1 px-2 py-2"
  >
    {#snippet children(row: ThreadDisplayRow)}
      <div role="listitem" class="px-0 py-0">
        <ThreadRow
          thread={row.thread}
          {pane}
          {onStartDiscussion}
          indent={row.indent}
          hasChildren={row.hasVisibleChildren}
          expanded={row.expanded}
          onToggleExpand={() => toggleParent(row.thread.id)}
          selected={selected.has(row.thread.id)}
          onSelectClick={(modifier) => handleRowSelectClick(row.thread, modifier)}
        />
      </div>
    {/snippet}
  </VirtualList>
{/if}
