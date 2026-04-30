<script lang="ts">
  import Undo2 from 'lucide-svelte/icons/undo-2';
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createAttachmentPreviews,
    parseUserMessageAttachments,
    type AttachmentPreviewSource,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import UserMessageActionsPopover from './UserMessageActionsPopover.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';

  interface Props {
    item: Item;
    pane?: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  }

  let { item, pane, onImageExpand }: Props = $props();

  // Trigger button for the actions popover. Visibility gating ensures
  // the button is only present when at least one option is actionable:
  //   - we have a pane to act on
  //   - no turn currently in flight (revert/fork would race with active state)
  //   - checkpoint history is loaded and available (not a non-git workspace)
  //   - this isn't an in-flight message that hasn't landed yet
  const showActionsTrigger = $derived(
    pane !== undefined
      && getActiveTurn(pane.threadId) === null
      && pane.diffPanel.checkpointsLoaded
      && !pane.diffPanel.checkpointsUnavailable
      && !item.id.startsWith('local-pending-'),
  );

  let actionsAnchor: HTMLButtonElement | undefined = $state(undefined);
  let actionsOpen = $state(false);

  const attachments = $derived<AttachmentPreviewSource[]>(parseUserMessageAttachments(item.meta));
  // Pane-owned blob cache: blob URLs survive virtua's overscan eviction,
  // so back-scrolling to a previously-mounted UserMessage doesn't refetch
  // attachments from Go or re-allocate object URLs. The IntersectionObserver
  // gate has been dropped — virtua's bufferSize=900 already bounds which
  // rows are mounted to "near the visible viewport"; loading on mount
  // costs at most a small read-ahead and the cache de-dupes across remounts.
  // pane + item.id stable per row instance; capture the cache once via untrack.
  const cache = untrack(() => (pane ? pane.attachmentCacheFor(item.id) : undefined));
  const attachmentPreviews = createAttachmentPreviews(
    () => attachments,
    { cache },
  );
  const visibleSummary = $derived(
    item.summary.replace(/\n\n!\[[^\]]*]\(attachment:\/\/[^\s)]+\)/g, '').trimEnd(),
  );

  async function expandAttachment(id: string): Promise<void> {
    if (!onImageExpand) return;
    // Always refetches full bytes; the per-pane attachment cache holds
    // thumbnails, not full-size pixels, so there's no synchronous shortcut.
    const expanded = await attachmentPreviews.loadExpandedPreview(id);
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
  const isoTime = $derived(new Date(item.createdAt).toISOString());
</script>

<div class="group mb-5 flex justify-end">
  <div
    class="max-w-[82%] rounded-[18px] rounded-br-[8px] border border-border-subtle bg-surface-2/60
           px-3.5 py-2 text-[13px] leading-[1.55] text-fg shadow-sheet"
  >
    {#if attachments.length > 0}
      <div
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
    <div class="mt-1.5 flex items-center justify-end gap-1.5 text-[10px] text-fg-hint/70">
      {#if showActionsTrigger && pane}
        <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
          <button
            bind:this={actionsAnchor}
            type="button"
            class="flex h-5 w-5 items-center justify-center rounded text-fg-hint transition-colors hover:bg-surface-2/60 hover:text-fg focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40"
            aria-label="Revert or fork from this message"
            aria-haspopup="menu"
            aria-expanded={actionsOpen}
            title="Revert or fork from this message"
            onclick={() => (actionsOpen = !actionsOpen)}
          >
            <Icon icon={Undo2} size={12} strokeWidth={2.2} />
          </button>
        </span>
      {/if}
      {#if visibleSummary}
        <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
          <CopyButton
            text={visibleSummary}
            label="Copy message"
            onError={() => addToast('error', 'Failed to copy')}
          />
        </span>
      {/if}
      <time class="tabular-nums" datetime={isoTime}>
        {time}
      </time>
    </div>
  </div>
</div>

{#if pane && showActionsTrigger}
  <UserMessageActionsPopover
    {pane}
    userTurnIndex={item.turnIndex}
    anchor={actionsAnchor}
    open={actionsOpen}
    onClose={() => (actionsOpen = false)}
  />
{/if}
