<script lang="ts">
  import GitFork from 'lucide-svelte/icons/git-fork';
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createAttachmentPreviews,
    type AttachmentPreviewSource,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import {
    parseUserMessageAttachments,
    parseUserMessageMeta,
  } from '../../utils/userMessageMeta';
  import { formatTimeOfDay } from '../../utils/format';
  import type { UserMessageActions } from './userMessageActions';

  interface Props {
    item: Item;
    pane?: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    actions?: UserMessageActions;
    targetFlash?: boolean;
    targetFlashNonce?: number;
  }

  let {
    item,
    pane,
    onImageExpand,
    actions,
    targetFlash = false,
    targetFlashNonce = 0,
  }: Props = $props();

  const userMeta = $derived(parseUserMessageMeta(item.meta));
  const isWireOnlyUserMessage = $derived(userMeta?.wire_only === true);
  const canRequestFork = $derived(typeof actions?.onForkMessage === 'function');
  const forkBusy = $derived(actions?.forkingItemId === item.id);

  // Eligibility for the fork message action. Stable for the message's
  // lifetime. Deliberately does NOT gate on getActiveTurn:
  // unmounting the toolbar at turn-started / re-mounting at turn-completed
  // would collapse the footer row's height (the icon buttons are taller
  // than the timestamp text), which reads as jitter on the just-sent
  // message. Race prevention during an active turn is handled by
  // `actionsTurnLocked`, which disables the buttons — grayed out via the
  // IconButton disabled styling — instead of removing them from the DOM.
  const showMessageActions = $derived(
    pane !== undefined && !isWireOnlyUserMessage,
  );

  // True while a turn is in flight on this pane's thread. Disables the
  // action buttons so fork can't race the active turn.
  const actionsTurnLocked = $derived(
    pane !== undefined && getActiveTurn(pane.threadId) !== null,
  );

  const attachments = $derived<AttachmentPreviewSource[]>(
    parseUserMessageAttachments(item.meta, item.threadId),
  );
  // Pane-owned blob cache: blob URLs survive the window's overscan eviction,
  // so back-scrolling to a previously-mounted UserMessage doesn't refetch
  // attachments from Go or re-allocate object URLs. The IntersectionObserver
  // gate has been dropped — the virtualizer's buffer already bounds which
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
  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  async function requestFork(): Promise<void> {
    if (!canRequestFork || forkBusy || actionsTurnLocked) return;
    await actions?.onForkMessage?.(item);
  }
</script>

<div class="group mb-5 flex justify-end">
  <div class="flex max-w-[82%] flex-col items-end">
    <div
      class="rounded-[18px] rounded-br-[8px] border border-border-subtle bg-surface-2/60
             px-4 py-2.5 text-[0.8125rem] leading-[1.55] text-fg shadow-sheet"
      class:user-message-target-flash-a={targetFlash && targetFlashNonce % 2 === 0}
      class:user-message-target-flash-b={targetFlash && targetFlashNonce % 2 === 1}
      data-target-flash={targetFlash ? 'true' : undefined}
      data-testid="user-message-bubble"
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
                  class="aspect-[4/3] w-full object-cover"
                />
              {:else}
                <span class="flex aspect-[4/3] w-full items-center justify-center px-2 text-center text-xs text-text-secondary">
                  {attachment.filename}
                </span>
              {/if}
              <span
                class="absolute bottom-1 left-1 rounded bg-black/70 px-1 py-0.5 text-[0.625rem] font-medium leading-none text-white"
                aria-label={`Image ${index + 1}`}
              >
                #{index + 1}
              </span>
            </button>
          {/each}
        </div>
      {/if}
      {#if visibleSummary}
        <!-- wrap-anywhere, not break-words: `overflow-wrap: break-word` doesn't
             lower a line's min-content width, and the bubble is a shrink-to-fit
             flex child whose fit-content sizing floors at min-content — pasted
             plaintext tables (NBSP-padded cells, unbroken border runs) blew the
             bubble past the pane edge instead of wrapping. `anywhere` counts the
             break opportunities in min-content, so the 82% cap holds.
             Guard: userMessageOverflow.browser.test.ts -->
        <p class="whitespace-pre-wrap wrap-anywhere">{visibleSummary}</p>
      {/if}
    </div>
    <div class="mt-1 flex items-center justify-end gap-1.5 pr-1 text-[0.625rem] text-fg-hint">
      {#if showMessageActions && pane}
        {#if canRequestFork}
          <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
            <IconButton
              label="Fork from this message"
              size="sm"
              variant="ghost"
              disabled={forkBusy || actionsTurnLocked}
              onClick={() => void requestFork()}
            >
              {#snippet children()}
                <Icon icon={GitFork} size={13} strokeWidth={2.2} />
              {/snippet}
            </IconButton>
          </span>
        {/if}
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

<style>
  @keyframes user-message-target-glow-a {
    0% {
      border-color: color-mix(in oklab, var(--accent) 88%, var(--text-primary) 12%);
      box-shadow:
        0 0 0 1px color-mix(in oklab, var(--accent) 72%, transparent),
        0 0 26px color-mix(in oklab, var(--accent) 34%, transparent);
    }
    58% {
      border-color: color-mix(in oklab, var(--accent) 66%, var(--border-subtle));
      box-shadow:
        0 0 0 1px color-mix(in oklab, var(--accent) 38%, transparent),
        0 0 18px color-mix(in oklab, var(--accent) 20%, transparent);
    }
    100% {
      border-color: var(--border-subtle);
      box-shadow: var(--shadow-sheet);
    }
  }

  @keyframes user-message-target-glow-b {
    0% {
      border-color: color-mix(in oklab, var(--accent) 88%, var(--text-primary) 12%);
      box-shadow:
        0 0 0 1px color-mix(in oklab, var(--accent) 72%, transparent),
        0 0 26px color-mix(in oklab, var(--accent) 34%, transparent);
    }
    58% {
      border-color: color-mix(in oklab, var(--accent) 66%, var(--border-subtle));
      box-shadow:
        0 0 0 1px color-mix(in oklab, var(--accent) 38%, transparent),
        0 0 18px color-mix(in oklab, var(--accent) 20%, transparent);
    }
    100% {
      border-color: var(--border-subtle);
      box-shadow: var(--shadow-sheet);
    }
  }

  .user-message-target-flash-a {
    animation: user-message-target-glow-a 900ms ease-out;
  }

  .user-message-target-flash-b {
    animation: user-message-target-glow-b 900ms ease-out;
  }

  @media (prefers-reduced-motion: reduce) {
    .user-message-target-flash-a,
    .user-message-target-flash-b {
      animation: none;
      border-color: color-mix(in oklab, var(--accent) 70%, var(--border-subtle));
      box-shadow:
        0 0 0 1px color-mix(in oklab, var(--accent) 42%, transparent),
        0 0 16px color-mix(in oklab, var(--accent) 18%, transparent);
    }
  }
</style>
