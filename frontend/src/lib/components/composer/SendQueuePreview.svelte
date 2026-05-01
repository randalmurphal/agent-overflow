<script lang="ts">
  // Queued user-message preview. Rendered inside the composerOverlay
  // (ChatView.svelte) between the working indicator and LiveTodoPanel.
  // Items live in `sendQueue.svelte.ts` — an in-memory per-thread
  // SvelteMap keyed by threadId. Click a row to lift it back into the
  // composer for editing; click × to drop it.
  //
  // Mirrors Codex's `pending_input_preview.rs` and Claude Code's
  // `PromptInputQueuedCommands.tsx` — `↳` prefix, dim italic body,
  // line-clamped to keep long messages readable. Empty state renders
  // nothing (no placeholder card / no "No queued messages" text), so
  // the composer overlay's measured height shrinks to zero when no
  // items are queued and the timeline reclaims that vertical space.
  //
  // Lives OUTSIDE `<VList>` — the chat scroll surface stays clean of
  // queued items and never measures them as transcript rows.
  // composerOverlay's ResizeObserver picks up our height changes and
  // updates `--composer-height` so the timeline bottom padding tracks.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import {
    cancelItem,
    getQueueForThread,
    popItem,
    snapshotFromQueueItem,
    type QueueItem,
  } from '../../stores/sendQueue.svelte';
  import { getActiveTurn, hasPendingSend } from '../../stores/threadStatuses.svelte';
  import Icon from '../primitives/Icon.svelte';
  import X from 'lucide-svelte/icons/x';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
  }

  let { pane, draft }: Props = $props();

  let items = $derived(getQueueForThread(pane.threadId ?? ''));
  // Pull up against the working indicator (mb-6) when the indicator is
  // showing. Same trick LiveTodoPanel uses; both want to read as
  // adjacent lines of one logical "what's happening" block.
  let pulledUp = $derived(
    getActiveTurn(pane.threadId) !== null
    || hasPendingSend(pane.threadId),
  );

  function handleEdit(item: QueueItem): void {
    if (!pane.threadId) return;
    const removed = popItem(pane.threadId, item.id);
    if (!removed) return;
    void draft.restoreDraftFor(pane.threadId, snapshotFromQueueItem(removed));
  }

  function handleCancel(itemId: string): void {
    if (!pane.threadId) return;
    cancelItem(pane.threadId, itemId);
  }
</script>

{#if items.length > 0 && pane.threadId}
  <div
    class={`mb-2 flex flex-col gap-1 pl-1.5 text-[12px] ${pulledUp ? '-mt-5' : ''}`}
    data-testid="send-queue-preview"
    aria-label="Queued messages"
  >
    {#each items as item (item.id)}
      <div
        class="group flex items-start gap-2"
        data-testid="send-queue-preview-row"
      >
        <span class="select-none pt-0.5 text-fg-hint/55" aria-hidden="true">↳</span>
        <button
          type="button"
          onclick={() => handleEdit(item)}
          class="line-clamp-3 flex-1 cursor-text rounded px-1 py-0.5 text-left italic text-fg-muted/85 transition-colors hover:bg-surface-2/40 hover:text-fg-default focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
          aria-label="Edit queued message"
          data-testid="send-queue-preview-edit"
        >
          {item.message}
        </button>
        <button
          type="button"
          onclick={() => handleCancel(item.id)}
          class="invisible mt-0.5 inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-full text-fg-hint/65 transition-colors hover:bg-surface-2/50 hover:text-fg-muted focus-visible:visible focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 group-hover:visible"
          aria-label="Remove from queue"
          title="Remove from queue"
          data-testid="send-queue-preview-cancel"
        >
          <Icon icon={X} size={11} strokeWidth={2.25} />
        </button>
      </div>
    {/each}
  </div>
{/if}
