<script lang="ts">
  import type { Item } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  const time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  // /\S/.test short-circuits at the first non-whitespace character — for
  // long streaming summaries this matters; .trim() would allocate the
  // full string every reactive tick.
  const canCopy = $derived(item.status !== 'streaming' && /\S/.test(item.summary));
</script>

<div class="group mb-3" data-item-kind={item.kind}>
  <div
    class="text-fg-muted"
    data-testid="assistant-message-body"
    data-render-mode="client-markdown"
  >
    <ChatMarkdown
      source={item.summary}
      streaming={item.status === 'streaming'}
      workspacePath={paneWorkspacePath(pane)}
    />
  </div>
  <div class="mt-1.5 flex items-center gap-1.5 text-[10px] text-fg-hint/70">
    <time class="tabular-nums" datetime={isoTime}>
      {time}
    </time>
    {#if canCopy}
      <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
        <CopyButton
          text={item.summary}
          label="Copy message"
          onError={() => addToast('error', 'Failed to copy')}
        />
      </span>
    {/if}
  </div>
</div>
