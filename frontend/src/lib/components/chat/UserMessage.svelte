<script lang="ts">
  import type { Item } from '../../types/models';
  import {
    createAttachmentPreviews,
    parseUserMessageAttachments,
    type AttachmentPreviewSource,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';

  interface Props {
    item: Item;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  }

  let { item, onImageExpand }: Props = $props();

  let attachmentRoot: HTMLDivElement | undefined = $state(undefined);
  let shouldLoadPreviews = $state(false);
  const attachments = $derived<AttachmentPreviewSource[]>(parseUserMessageAttachments(item.meta));
  const attachmentPreviews = createAttachmentPreviews(
    () => attachments,
    { shouldLoad: () => shouldLoadPreviews },
  );
  const visibleSummary = $derived(
    item.summary.replace(/\n\n!\[[^\]]*]\(attachment:\/\/[^\s)]+\)/g, '').trimEnd(),
  );

  $effect(() => {
    if (attachments.length === 0) return;
    const root = attachmentRoot;
    if (!root) return;

    if (typeof IntersectionObserver !== 'function') {
      shouldLoadPreviews = true;
      return;
    }

    const observer = new IntersectionObserver((entries) => {
      if (!entries.some((entry) => entry.isIntersecting)) return;
      shouldLoadPreviews = true;
      observer.disconnect();
    }, { rootMargin: '240px' });
    observer.observe(root);
    return () => observer.disconnect();
  });

  async function expandAttachment(id: string): Promise<void> {
    if (!onImageExpand) return;
    const expanded = attachmentPreviews.expandedPreview(id)
      ?? await attachmentPreviews.loadExpandedPreview(id);
    if (expanded) onImageExpand(expanded);
  }

  // Display-only short time. Purely cosmetic — the authoritative
  // timestamps for turn accounting live on the pane, not on user text.
  const time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
</script>

<div class="group mb-5 flex justify-end">
  <div class="flex max-w-[82%] flex-col items-end gap-1">
    <div
      class="rounded-[18px] rounded-br-[8px] border border-border-subtle bg-surface-2/60
             px-3.5 py-2 text-[13px] leading-[1.55] text-fg shadow-sheet"
    >
      {#if attachments.length > 0}
        <div
          bind:this={attachmentRoot}
          class="mb-2 grid max-w-[420px] grid-cols-2 gap-2"
          data-testid="user-message-attachments"
        >
          {#each attachments as attachment, index (attachment.id)}
            {@const preview = attachmentPreviews.previewFor(attachment.id)}
            <button
              type="button"
              aria-label={`Preview ${attachment.filename}`}
              class="relative overflow-hidden rounded-lg border border-border bg-surface-1 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
              onclick={() => expandAttachment(attachment.id)}
            >
              {#if preview}
                <img
                  src={preview.url}
                  alt={attachment.filename}
                  class="max-h-[220px] w-full object-cover"
                />
              {:else}
                <span class="flex h-24 items-center justify-center px-2 text-center text-xs text-text-secondary">
                  {attachment.filename}
                </span>
              {/if}
              <span
                class="absolute bottom-1 left-1 rounded bg-black/70 px-1 py-0.5 text-[10px] font-medium leading-none text-white"
                aria-label={`Image ${index + 1}`}
              >
                #{index + 1}
              </span>
            </button>
          {/each}
        </div>
      {/if}
      {#if visibleSummary}
        <p class="whitespace-pre-wrap">{visibleSummary}</p>
      {/if}
    </div>
    <time
      class="text-[10px] text-fg-hint"
      datetime={new Date(item.createdAt).toISOString()}
    >
      {time}
    </time>
  </div>
</div>
