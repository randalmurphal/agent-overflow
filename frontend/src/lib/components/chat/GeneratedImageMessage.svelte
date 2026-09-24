<script lang="ts">
  /*
   * A picture the agent generated, rendered at assistant-prose level.
   *
   * It is deliberately NOT a tool row: the tool row stays on the activity
   * rail with its lifecycle chip, and this is the output the reader came for.
   * The tile, the pane-owned blob cache and the lightbox are the composer
   * path's (`createAttachmentPreviews` / `ExpandedImageDialog`), so the image
   * is served through the ordinary attachment route and works the same in the
   * embedded webview, a connected browser and on the phone.
   *
   * A failed import renders its reason instead. There is no third state: the
   * backend writes either bytes or a reason (`generatedImageRow` returns null
   * when a row has neither, and the ordinary assistant renderer takes it).
   */
  import { untrack } from 'svelte';
  import ImageOff from '@lucide/svelte/icons/image-off';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import {
    createAttachmentPreviews,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';
  import { generatedImageRow } from '../../utils/generatedImageMeta';
  import { attachmentImageMenuTag } from '../../utils/imageMenuActions';
  import { formatTimeOfDay } from '../../utils/format';

  let {
    pane,
    item,
    onImageExpand,
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    item: Item;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  } = $props();

  const row = $derived(generatedImageRow(item));
  const images = $derived(row?.images ?? []);
  const cache = untrack(() => (pane ? pane.attachmentCacheFor(item.id) : undefined));
  const attachmentPreviews = createAttachmentPreviews(() => [...images], { cache });
  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());
  // The caption is the model's revised prompt when it reported one. The row's
  // summary carries the same text (every summary-reading surface needs it),
  // so the fallback label is not repeated under the picture.
  const caption = $derived(row?.prompt ?? '');

  async function expand(id: string): Promise<void> {
    if (!onImageExpand) return;
    const expanded = await attachmentPreviews.loadExpandedPreview(id);
    if (expanded) onImageExpand(expanded);
  }
</script>

{#if row}
  <div class="mb-4" data-testid="generated-image-message">
    {#if row.error}
      <div
        class="flex max-w-[420px] items-start gap-2 rounded-[var(--radius-control)] border border-error/30 bg-error/10 px-3 py-2 text-sm text-error"
        data-testid="generated-image-error"
      >
        <Icon icon={ImageOff} size={16} class="mt-0.5 shrink-0" />
        <span class="min-w-0">
          <span class="block font-medium">The generated image could not be saved.</span>
          <span class="mt-0.5 block text-xs opacity-90">{row.error}</span>
        </span>
      </div>
    {:else}
      <div class="flex flex-col gap-2" data-testid="generated-image-attachments">
        {#each images as attachment (attachment.id)}
          {@const preview = attachmentPreviews.previewFor(attachment.id)}
          <button
            type="button"
            aria-label={`Preview ${attachment.filename}`}
            class="max-w-[520px] overflow-hidden rounded-lg border border-border bg-surface-1 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60"
            onclick={() => expand(attachment.id)}
            {...attachmentImageMenuTag(attachment)}
          >
            {#if preview}
              <img
                src={preview.url}
                alt={caption || attachment.filename}
                class="block w-full"
              />
            {:else}
              <span
                class="flex aspect-[4/3] w-full items-center justify-center px-2 text-center text-xs text-text-secondary"
              >
                {attachment.filename}
              </span>
            {/if}
          </button>
        {/each}
      </div>
    {/if}
    {#if caption}
      <p class="mt-1.5 max-w-[520px] text-xs leading-5 text-fg-muted" data-testid="generated-image-caption">
        {caption}
      </p>
    {/if}
    <time class="mt-1 block text-[0.625rem] text-fg-hint" datetime={isoTime}>{time}</time>
  </div>
{/if}
