<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ChannelView from './ChannelView.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let thread = $derived(pane.thread);
  let channelId = $derived(thread?.discussionId ?? '');
  let isDiscussion = $derived(!!thread && thread.mode === 'discussion' && !!channelId);
</script>

{#if isDiscussion}
  <div class="flex flex-col h-full min-h-0">
    <div class="border-b border-border-subtle px-5 py-2 flex items-center gap-2 shrink-0">
      <span
        class="text-[0.625rem] font-semibold px-1.5 py-0.5 rounded-[var(--radius-field)] bg-accent/15 text-accent tracking-wide"
        aria-label="Discussion Thread"
      >D</span>
      <h2 class="text-sm font-medium text-fg truncate">{thread?.title ?? 'Discussion'}</h2>
      <span class="text-[0.6875rem] text-fg-subtle">Discussion channel</span>
      <span
        class="ml-auto text-[0.6875rem] text-fg-muted truncate min-w-0 shrink max-w-[280px] font-mono"
        title={thread?.workspacePath}
      >
        {thread?.workspacePath}
      </span>
    </div>
    <ChannelView {pane} channelId={channelId} />
  </div>
{:else}
  <div class="flex items-center justify-center h-full text-[0.75rem] text-fg-subtle">
    Thread is not running a discussion.
  </div>
{/if}
