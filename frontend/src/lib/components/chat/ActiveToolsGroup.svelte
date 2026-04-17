<script lang="ts">
  import type { WorkEntryData } from '../../types/models';
  import { summarizeActiveTools } from '../../utils/activeToolsSummary';
  import WorkEntry from './WorkEntry.svelte';

  interface Props {
    entries: WorkEntryData[];
  }

  let { entries }: Props = $props();

  // Local expansion state. Defaults to collapsed whenever the group has more
  // than one tool so streaks of parallel work don't flood the timeline. Single
  // tools always render expanded — nothing to hide.
  let userExpanded = $state(false);
  let summary = $derived(summarizeActiveTools(entries));

  // Auto-collapse the expansion flag the moment the group drops back to one
  // tool. This lines up with the spec: once work finishes, the chip goes away
  // and the remaining cards render inline on their own. When a fresh streak of
  // 2+ tools arrives later the chip starts collapsed again.
  $effect(() => {
    if (entries.length <= 1 && userExpanded) {
      userExpanded = false;
    }
  });

  function toggle() {
    userExpanded = !userExpanded;
  }

  function handleKeydown(event: KeyboardEvent) {
    // Browsers already fire click on Enter/Space for <button>, but we keep an
    // explicit handler so tests (and assistive layers emitting synthetic
    // keydown without the follow-up click) get the same behavior.
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      toggle();
    }
  }

  // Whether child cards render. For count >= 2 we hide them until the user
  // opts in to the detailed view; for <2 there's nothing to hide.
  let expanded = $derived(entries.length <= 1 || userExpanded);
</script>

{#if entries.length > 0}
  <div class="mb-3 flex flex-col gap-1" role="group" aria-label="Active tool calls">
    {#if entries.length >= 2}
      <button
        type="button"
        class="flex items-center gap-2 rounded border border-border bg-surface-1 px-3 py-2 text-sm text-left hover:bg-surface-2/50 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        onclick={toggle}
        onkeydown={handleKeydown}
        aria-expanded={userExpanded}
        aria-controls="active-tools-group-children"
        data-testid="active-tools-chip"
      >
        <span class="font-mono text-xs text-text-secondary shrink-0" aria-hidden="true">[T]</span>
        <span class="text-text-primary truncate flex-1">{summary.label}</span>
        <span class="text-xs text-text-secondary shrink-0" aria-hidden="true">
          {userExpanded ? '▼' : '▶'}
        </span>
      </button>
    {/if}

    {#if expanded}
      <div
        id="active-tools-group-children"
        class={entries.length >= 2 ? 'flex flex-col gap-1 pl-2 border-l border-border/60' : 'flex flex-col gap-1'}
        data-testid="active-tools-children"
      >
        {#each entries as child (child.id)}
          <WorkEntry entry={child} />
        {/each}
      </div>
    {/if}
  </div>
{/if}
