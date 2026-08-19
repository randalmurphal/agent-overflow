<script lang="ts">
  import Pencil from '@lucide/svelte/icons/pencil';
  import GitFork from '@lucide/svelte/icons/git-fork';
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createAttachmentPreviews,
    type AttachmentPreviewSource,
    type ExpandedImagePreview,
  } from '../../utils/attachmentPreview.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import UserMessageBody from './UserMessageBody.svelte';
  import UserMessageEditor from './UserMessageEditor.svelte';
  import { preservePaneScrollAnchorAt } from './preserveScrollAnchor';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import {
    parseUserMessageAttachments,
    parseUserMessageMeta,
    userMessageCommandRanges,
  } from '../../utils/userMessageMeta';
  import { commandSegments } from '../../utils/commandWords';
  import { formatTimeOfDay } from '../../utils/format';
  import type { UserMessageActions } from './userMessageActions';

  interface Props {
    item: Item;
    pane?: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    actions?: UserMessageActions;
  }

  let { item, pane, onImageExpand, actions }: Props = $props();

  const userMeta = $derived(parseUserMessageMeta(item.meta));
  const isWireOnlyUserMessage = $derived(userMeta?.wire_only === true);
  const canRequestEdit = $derived(typeof actions?.onEditMessage === 'function');
  // ANY active edit session locks every pencil, not just the anchor
  // row's: only one edit can run at a time, and a disabled control beats
  // a click that ChatView's flow guard would silently swallow (the
  // session spans the editor, preflight and the confirm dialog, not just
  // the destructive RPC).
  const editLocked = $derived((actions?.editSession ?? null) !== null);
  // This row IS the open editor. The bubble renders the editor in place
  // of its body — the read-only attachment grid included, since the
  // editor's own attachment row renders the draft's copy of them.
  const editSession = $derived(
    actions?.editSession?.itemId === item.id ? actions.editSession : null,
  );
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
  // A composer command that expanded at send time (D31) keeps its words in the
  // transcript — the block it added is never stored — and every occurrence
  // renders in the accent colour, exactly as the composer showed them while
  // typing.
  const commandRanges = $derived(userMessageCommandRanges(item.meta, visibleSummary));
  const summarySegments = $derived(
    commandRanges.length > 0 ? commandSegments(visibleSummary, commandRanges) : [],
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

  // Both mode transitions change the row's height by a lot (a clamped
  // paragraph becomes a composer card, and back), so both hold the
  // bubble's top edge: the reader's eye is on the message they are about
  // to rewrite, and it must not slide out from under them.
  let bubbleEl: HTMLDivElement | undefined = $state(undefined);

  function requestEdit(): void {
    if (!canRequestEdit || editLocked || actionsTurnLocked) return;
    void preservePaneScrollAnchorAt(pane, bubbleEl, () => {
      // A clamped message must open in full to be edited — and stay open
      // if the edit is cancelled, because the reader chose to look at it.
      pane?.setUserMessageExpanded(item.id, true);
      void actions?.onEditMessage?.(item);
    });
  }

  function cancelEdit(): void {
    const session = editSession;
    if (!session) return;
    void preservePaneScrollAnchorAt(pane, bubbleEl, () => {
      session.onCancel();
    });
  }

  async function requestFork(): Promise<void> {
    if (!canRequestFork || forkBusy || actionsTurnLocked) return;
    await actions?.onForkMessage?.(item);
  }
</script>

{#snippet readOnlyBody()}
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
            class="absolute bottom-1 left-1 rounded bg-scrim/70 px-1 py-0.5 text-[0.625rem] font-medium leading-none text-scrim-fg"
            aria-label={`Image ${index + 1}`}
          >
            #{index + 1}
          </span>
        </button>
      {/each}
    </div>
  {/if}
  {#if visibleSummary}
    <!-- Text only: attachments above stay outside the clamped region. -->
    <UserMessageBody
      text={visibleSummary}
      segments={summarySegments}
      itemId={item.id}
      {pane}
    />
  {/if}
{/snippet}

<div class="group mb-5 flex justify-end">
  <div class="flex max-w-[82%] flex-col items-end">
    <div
      bind:this={bubbleEl}
      class="rounded-[18px] rounded-br-[8px] border border-accent/20 bg-accent/15
             px-4 py-2.5 text-[0.8125rem] leading-[1.55] text-fg shadow-sheet"
      class:w-[46rem]={editSession !== null}
      class:max-w-full={editSession !== null}
      data-testid="user-message-bubble"
    >
      {#if editSession && pane}
        <UserMessageEditor
          {pane}
          session={editSession}
          onCancel={cancelEdit}
          {onImageExpand}
        />
      {:else}
        {@render readOnlyBody()}
      {/if}
    </div>
    <div class="mt-1 flex items-center justify-end gap-1.5 pr-1 text-[0.625rem] text-fg-hint">
      {#if showMessageActions && pane}
        {#if canRequestEdit}
          <span class="opacity-0 transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100">
            <IconButton
              label="Edit message and resend from here"
              size="sm"
              variant="ghost"
              disabled={editLocked || actionsTurnLocked}
              onClick={requestEdit}
            >
              {#snippet children()}
                <Icon icon={Pencil} size={13} strokeWidth={2.2} />
              {/snippet}
            </IconButton>
          </span>
        {/if}
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
