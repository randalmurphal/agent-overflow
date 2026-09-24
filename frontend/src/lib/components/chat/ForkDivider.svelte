<script lang="ts">
  import GitFork from '@lucide/svelte/icons/git-fork';
  import Icon from '../primitives/Icon.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import { getThreadById } from '../../stores/threads.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

  // The row a fork holds at its cut (store: forkOrigin). Everything above
  // it is read from the source thread; once the source is deleted that
  // history is gone and the divider records the deletion.
  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  const origin = $derived.by(() => {
    const meta = parseJsonObject(item.meta);
    const title = typeof meta?.sourceTitle === 'string' ? meta.sourceTitle.trim() : '';
    return {
      sourceThreadId: typeof meta?.sourceThreadId === 'string' ? meta.sourceThreadId : '',
      sourceTitle: title || 'Untitled',
      sourceDeleted: meta?.sourceDeleted === true,
    };
  });
  const label = $derived(`Forked from ${origin.sourceTitle}`);

  // The source as the sidebar tracks it. Undefined when it is deleted or
  // not in the sidebar's view, which leaves the label plain.
  const source = $derived(
    origin.sourceDeleted || !origin.sourceThreadId ? undefined : getThreadById(origin.sourceThreadId),
  );

  async function openSource(): Promise<void> {
    if (!source) return;
    await openThreadFromNavigation(source, pane);
  }
</script>

<div data-testid="fork-divider" class="my-8">
  <div class="flex items-center gap-3 text-[0.625rem] uppercase tracking-[0.18em] text-fg-subtle">
    <div class="timeline-hairline flex-1"></div>
    {#if source}
      <button
        type="button"
        data-testid="fork-divider-source"
        aria-label={`Open fork source ${origin.sourceTitle}`}
        class="flex cursor-pointer items-center gap-1.5 bg-transparent uppercase tracking-[0.18em] text-fg-subtle hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        onclick={openSource}
      >
        <Icon icon={GitFork} size={11} strokeWidth={2} class="opacity-70" />
        <span>{label}</span>
      </button>
    {:else}
      <span class="flex items-center gap-1.5">
        <Icon icon={GitFork} size={11} strokeWidth={2} class="opacity-70" />
        <span>{label}</span>
        {#if origin.sourceDeleted}
          <span data-testid="fork-divider-deleted" class="text-fg-hint">· source deleted</span>
        {/if}
      </span>
    {/if}
    <div class="timeline-hairline flex-1"></div>
  </div>
</div>
