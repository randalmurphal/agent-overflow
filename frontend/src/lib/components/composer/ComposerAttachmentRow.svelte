<script lang="ts">
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';
  import type { Attachment } from '../../types/attachment';
  import { formatAttachmentSize } from '../../types/attachment';
  import {
    createAttachmentPreviews,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';

  interface Props {
    attachments: Attachment[];
    onRemove: (id: string) => void;
    onExpand?: (preview: ExpandedImagePreview) => void;
    dragActive?: boolean;
  }

  let { attachments, onRemove, onExpand, dragActive = false }: Props = $props();
  const attachmentPreviews = createAttachmentPreviews(() => attachments);

  function expandAttachment(id: string): void {
    if (!onExpand) return;
    const expanded = attachmentPreviews.expandedPreview(id);
    if (expanded) onExpand(expanded);
  }
</script>

{#if attachments.length > 0 || dragActive}
  <div
    class="flex flex-wrap gap-2 border-b border-border bg-surface-0/40 px-4 py-2"
    class:bg-accent={dragActive}
    data-testid="composer-attachment-row"
  >
    {#each attachments as attachment, index (attachment.id)}
      {@const preview = attachmentPreviews.previewFor(attachment.id)}
      <div
        class="group relative h-16 w-16 overflow-hidden rounded-lg border border-border bg-surface-1 shadow-sm"
        data-testid="attachment-thumb"
        title={`${attachment.filename} (${formatAttachmentSize(attachment.size)})`}
      >
        <button
          type="button"
          aria-label={`Preview ${attachment.filename}`}
          class="flex h-full w-full items-center justify-center text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/60"
          onclick={() => expandAttachment(attachment.id)}
        >
          {#if preview}
            <img
              src={preview.url}
              alt={attachment.filename}
              class="h-full w-full object-cover"
            />
          {:else}
            <span class="line-clamp-3 px-1.5 text-center text-[10px] leading-tight text-text-secondary">
              {attachment.filename}
            </span>
          {/if}
        </button>
        <button
          type="button"
          aria-label={`Remove ${attachment.filename}`}
          class="absolute right-1 top-1 rounded-full bg-black/65 p-0.5 text-white opacity-90 transition hover:bg-black/80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white/70"
          onclick={() => onRemove(attachment.id)}
        >
          <Icon icon={X} size={12} strokeWidth={2.5} class="opacity-100" />
        </button>
        <span
          class="absolute bottom-1 left-1 rounded bg-black/70 px-1 py-0.5 text-[10px] font-medium leading-none text-white"
          aria-label={`Image ${index + 1}`}
        >
          #{index + 1}
        </span>
      </div>
    {/each}
    {#if dragActive && attachments.length === 0}
      <span class="self-center text-xs text-text-secondary">Drop an image to attach</span>
    {/if}
  </div>
{/if}
