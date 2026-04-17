<script lang="ts">
  import type { Attachment } from '../../types/attachment';
  import { formatAttachmentSize } from '../../types/attachment';

  interface Props {
    attachments: Attachment[];
    onRemove: (id: string) => void;
    dragActive?: boolean;
  }

  let { attachments, onRemove, dragActive = false }: Props = $props();
</script>

{#if attachments.length > 0 || dragActive}
  <div
    class="flex flex-wrap gap-2 border-b border-border bg-surface-0 px-4 py-2"
    class:bg-accent={dragActive}
    data-testid="composer-attachment-row"
  >
    {#each attachments as attachment (attachment.id)}
      <div
        class="flex items-center gap-2 rounded-md border border-border bg-surface-1 px-2 py-1 text-xs"
        data-testid="attachment-chip"
      >
        <span class="truncate max-w-[12rem]" title={attachment.filename}>
          {attachment.filename}
        </span>
        <span class="text-text-secondary">{formatAttachmentSize(attachment.size)}</span>
        <button
          type="button"
          aria-label={`Remove ${attachment.filename}`}
          class="rounded p-0.5 text-text-secondary hover:text-text-primary hover:bg-surface-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          onclick={() => onRemove(attachment.id)}
        >
          <span aria-hidden="true">x</span>
        </button>
      </div>
    {/each}
    {#if dragActive && attachments.length === 0}
      <span class="text-xs text-text-secondary">Drop an image to attach</span>
    {/if}
  </div>
{/if}
