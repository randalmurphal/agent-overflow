<script lang="ts">
  import X from '@lucide/svelte/icons/x';
  import { untrack } from 'svelte';
  import Icon from '../primitives/Icon.svelte';
  import type { Attachment } from '../../types/attachment';
  import { formatAttachmentSize } from '../../types/attachment';
  import {
    createAttachmentPreviews,
    type AttachmentPreviewCache,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';

  interface Props {
    attachments: Attachment[];
    onRemove: (id: string) => void;
    onExpand?: (preview: ExpandedImagePreview) => void;
    dragActive?: boolean;
    /**
     * Blob-URL owner. Absent, this row owns them and revokes on destroy —
     * right for the composer, which only unmounts with its pane. A row
     * inside the virtualized timeline passes the pane's cache so a
     * windowing remount re-uses the decoded thumbnails instead of
     * revoking and re-fetching every image.
     */
    cache?: AttachmentPreviewCache;
  }

  let { attachments, onRemove, onExpand, dragActive = false, cache }: Props = $props();
  // Captured once, deliberately: the factory holds the cache for the
  // component's lifetime, and a host's cache identity is fixed per mounted
  // row (it is keyed by pane + item, both stable while this row exists).
  const attachmentPreviews = createAttachmentPreviews(
    () => attachments,
    { cache: untrack(() => cache) },
  );

  async function expandAttachment(id: string): Promise<void> {
    if (!onExpand) return;
    // The composer row's preview cache holds thumbnails; the lightbox
    // wants the original-resolution image. Always go through the
    // load-full-size path.
    const expanded = await attachmentPreviews.loadExpandedPreview(id);
    if (expanded) onExpand(expanded);
  }
</script>

{#if attachments.length > 0 || dragActive}
  <div
    class="flex flex-wrap gap-2 border-b border-border px-4 py-2"
    class:bg-accent={dragActive}
    data-testid="composer-attachment-row"
  >
    {#each attachments as attachment, index (attachment.id)}
      {@const preview = attachmentPreviews.previewFor(attachment.id)}
      <div
        class="group relative h-16 w-16 overflow-hidden rounded-lg border border-border bg-surface-1 shadow-sheet"
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
            <span class="line-clamp-3 px-1.5 text-center text-[0.625rem] leading-tight text-text-secondary">
              {attachment.filename}
            </span>
          {/if}
        </button>
        <button
          type="button"
          aria-label={`Remove ${attachment.filename}`}
          class="absolute right-1 top-1 rounded-full bg-scrim/65 p-0.5 text-scrim-fg opacity-90 transition hover:bg-scrim/80 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-scrim-fg/70"
          onclick={() => onRemove(attachment.id)}
        >
          <Icon icon={X} size={12} strokeWidth={2.5} class="opacity-100" />
        </button>
        <span
          class="absolute bottom-1 left-1 rounded bg-scrim/70 px-1 py-0.5 text-[0.625rem] font-medium leading-none text-scrim-fg"
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
