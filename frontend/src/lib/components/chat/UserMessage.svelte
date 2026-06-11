<script lang="ts">
  import RotateCcw from 'lucide-svelte/icons/rotate-ccw';
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
  import Popover from '../primitives/Popover.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import {
    parseUserMessageAttachments,
    parseUserMessageMeta,
  } from '../../utils/userMessageMeta';
  import { formatTimeOfDay } from '../../utils/format';
  import type { RevertMode } from '../../types/checkpoint';
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
  let revertAnchor: HTMLSpanElement | undefined = $state(undefined);

  const userMeta = $derived(parseUserMessageMeta(item.meta));
  const hasMessageCheckpoint = $derived(pane?.diffPanel.checkpointUserItemIds.has(item.id) ?? false);
  const isWireOnlyUserMessage = $derived(userMeta?.wire_only === true);
  const canRequestRevert = $derived(typeof actions?.onRevertMessage === 'function');
  const canRequestFork = $derived(typeof actions?.onForkMessage === 'function');
  const canConfirmRevert = $derived(typeof actions?.onConfirmRevertMessage === 'function');
  const revertPopoverOpen = $derived(actions?.revertTargetItemId === item.id);
  const revertAffectedFiles = $derived(actions?.revertAffectedFiles ?? []);
  const revertTotals = $derived.by(() => revertAffectedFiles.reduce(
    (acc, file) => ({
      additions: acc.additions + file.additions,
      deletions: acc.deletions + file.deletions,
    }),
    { additions: 0, deletions: 0 },
  ));
  const revertBusy = $derived(actions?.revertingItemId === item.id);
  const forkBusy = $derived(actions?.forkingItemId === item.id);

  // Eligibility for the revert/fork message actions. Flips at most once
  // per message lifetime — when the per-message checkpoint is captured —
  // and then stays stable. Deliberately does NOT gate on getActiveTurn:
  // unmounting the toolbar at turn-started / re-mounting at turn-completed
  // toggles the bubble's footer width (no min-width on the bubble), which
  // shows up as visible jitter on the just-sent user message. Race
  // prevention during an active turn is handled by `actionsTurnLocked`,
  // which renders the buttons as `visibility:hidden` — still in layout,
  // no pointer events — instead of removing them from the DOM.
  const showMessageActions = $derived(
    pane !== undefined
      && (canRequestRevert || canRequestFork)
      && pane.diffPanel.checkpointsLoaded
      && !pane.diffPanel.checkpointsUnavailable
      && hasMessageCheckpoint
      && !isWireOnlyUserMessage,
  );

  // True while a turn is in flight on this pane's thread. Drives a
  // `visibility:hidden` class on the action button wrappers so the
  // bubble width stays constant across turn-started/turn-completed
  // transitions and the buttons stay non-interactive during the turn.
  const actionsTurnLocked = $derived(
    pane !== undefined && getActiveTurn(pane.threadId) !== null,
  );

  const attachments = $derived<AttachmentPreviewSource[]>(
    parseUserMessageAttachments(item.meta, item.threadId),
  );
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
  const time = $derived(formatTimeOfDay(item.createdAt));
  const isoTime = $derived(new Date(item.createdAt).toISOString());

  async function requestRevert(): Promise<void> {
    if (!canRequestRevert || revertBusy) return;
    if (revertPopoverOpen) {
      actions?.onCancelRevertMessage?.();
      return;
    }
    await actions?.onRevertMessage?.(item);
  }

  async function confirmRevert(mode: RevertMode): Promise<void> {
    if (!canConfirmRevert || revertBusy) return;
    await actions?.onConfirmRevertMessage?.(mode);
  }

  async function requestFork(): Promise<void> {
    if (!canRequestFork || forkBusy) return;
    await actions?.onForkMessage?.(item);
  }
</script>

<div class="group mb-5 flex justify-end">
  <div
    class="max-w-[82%] rounded-[18px] rounded-br-[8px] border border-border-subtle bg-surface-2/60
           px-3.5 py-2 text-[0.8125rem] leading-[1.55] text-fg shadow-sheet"
    class:user-message-target-flash-a={targetFlash && targetFlashNonce % 2 === 0}
    class:user-message-target-flash-b={targetFlash && targetFlashNonce % 2 === 1}
    data-target-flash={targetFlash ? 'true' : undefined}
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
      <p class="whitespace-pre-wrap break-words">{visibleSummary}</p>
    {/if}
    <div class="mt-1.5 flex items-center justify-end gap-1.5 text-[0.625rem] text-fg-hint/70">
      {#if showMessageActions && pane}
        {#if canRequestRevert}
          <span
            bind:this={revertAnchor}
            class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100"
            class:invisible={actionsTurnLocked}
          >
            <IconButton
              label="Revert to this message"
              size="sm"
              variant="ghost"
              disabled={revertBusy}
              ariaHaspopup="menu"
              ariaExpanded={revertPopoverOpen}
              onClick={() => void requestRevert()}
            >
              {#snippet children()}
                <Icon icon={RotateCcw} size={13} strokeWidth={2.2} />
              {/snippet}
            </IconButton>
          </span>
        {/if}
        {#if canRequestFork}
          <span
            class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100"
            class:invisible={actionsTurnLocked}
          >
            <IconButton
              label="Fork from this message"
              size="sm"
              variant="ghost"
              disabled={forkBusy}
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
    <Popover
      anchor={revertAnchor}
      open={revertPopoverOpen}
      onClose={() => actions?.onCancelRevertMessage?.()}
      placement="bottom-end"
      offset={8}
      role="menu"
      ariaLabel="Revert message options"
    >
      <div
        class="w-[244px] overflow-hidden rounded-[10px] border border-border bg-surface-1/98 p-1.5 text-left shadow-menu"
        data-testid="user-message-revert-popover"
      >
        <button
          type="button"
          class="grid w-full grid-cols-[minmax(0,1fr)_auto] items-center gap-3 rounded-[var(--radius-control)] px-2.5 py-2 text-left text-[0.75rem] text-fg transition-colors hover:bg-surface-2/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-default disabled:opacity-55"
          disabled={revertBusy}
          onclick={() => void confirmRevert('conversation-and-files')}
          role="menuitem"
          data-testid="revert-conversation-and-files"
        >
          <span class="whitespace-nowrap font-medium">Conversation & files</span>
          <span class="flex shrink-0 items-center gap-1 whitespace-nowrap font-mono text-[0.6875rem] tabular-nums">
            <span class="text-success">+{revertTotals.additions}</span>
            <span class="text-error">-{revertTotals.deletions}</span>
          </span>
        </button>
        <button
          type="button"
          class="mt-0.5 flex w-full items-center rounded-[var(--radius-control)] px-2.5 py-2 text-left text-[0.75rem] font-medium text-fg-muted transition-colors hover:bg-surface-2/70 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-default disabled:opacity-55"
          disabled={revertBusy}
          onclick={() => void confirmRevert('conversation-only')}
          role="menuitem"
          data-testid="revert-conversation-only"
        >
          Conversation only
        </button>
      </div>
    </Popover>
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
