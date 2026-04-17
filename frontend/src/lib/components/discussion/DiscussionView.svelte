<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ChannelView from './ChannelView.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let thread = $derived(pane.thread);
  let channelId = $derived(thread?.discussionId ?? '');
  let isDiscussion = $derived(!!thread && thread.interactionMode === 'discussion' && !!channelId);
</script>

{#if isDiscussion}
  <div class="flex flex-col h-full min-h-0">
    <div class="border-b border-border bg-surface-1 px-4 py-2.5 flex items-center gap-2 shrink-0">
      <span class="text-xs font-medium px-1.5 py-0.5 rounded bg-accent/20 text-accent" aria-label="Discussion thread">D</span>
      <h2 class="text-sm font-medium text-text-primary truncate">{thread?.title ?? 'Discussion'}</h2>
      <span class="text-xs text-text-secondary">Discussion channel</span>
      <span class="ml-auto text-xs text-text-secondary truncate min-w-0 shrink max-w-[280px]" title={thread?.workspacePath}>
        {thread?.workspacePath}
      </span>
    </div>
    <ChannelView {pane} channelId={channelId} />
  </div>
{:else}
  <div class="flex items-center justify-center h-full text-xs text-text-secondary">
    Thread is not running a discussion.
  </div>
{/if}
