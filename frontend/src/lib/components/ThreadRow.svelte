<script lang="ts">
  import type { Thread } from '../types/models';
  import type { ThreadPane } from '../stores/thread.svelte';
  import { ArchiveThread } from '../stores/bindings';
  import { removeThread } from '../stores/threads.svelte';
  import { relativeTime } from '../utils/time';

  let { thread, pane }: { thread: Thread; pane: ThreadPane } = $props();

  let isActive = $derived(pane.threadId === thread.id);

  function handleClick() {
    pane.switchThread(thread);
  }

  async function handleArchive(e: MouseEvent) {
    e.stopPropagation();
    try {
      await ArchiveThread(thread.id);
      removeThread(thread.id);
      if (isActive) {
        pane.clear();
      }
    } catch (err) {
      console.error('Failed to archive thread:', err);
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  onclick={handleClick}
  onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handleClick(); } }}
  role="button"
  tabindex={0}
  class="group w-full text-left px-3 py-2 rounded-md cursor-pointer transition-colors
    {isActive ? 'bg-accent/15 text-text-primary' : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary'}"
>
  <div class="flex items-center gap-2">
    <span class="text-[10px] font-bold px-1 py-0.5 rounded shrink-0
      {thread.provider === 'claude' ? 'bg-accent/20 text-accent' : 'bg-orange-900/30 text-orange-300'}">
      {thread.provider === 'claude' ? 'C' : 'X'}
    </span>
    <span class="text-sm truncate flex-1">{thread.title || 'Untitled'}</span>
    <button
      onclick={handleArchive}
      class="opacity-0 group-hover:opacity-100 text-text-secondary hover:text-text-primary text-xs px-1 shrink-0 cursor-pointer"
      title="Archive thread"
    >
      x
    </button>
  </div>
  <div class="text-xs text-text-secondary/60 mt-0.5 ml-6">
    {relativeTime(thread.updatedAt)}
  </div>
</div>
