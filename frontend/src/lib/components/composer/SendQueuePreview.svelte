<script lang="ts">
  // Send-queue overlay. Renders inside the composerOverlay
  // (ChatView.svelte) between the working indicator and the composer
  // textarea. Two visually distinct zones in arrival order:
  //
  //  - Zone 1 — backend-queued items waiting for the next round's
  //    first non-subagent tool_use to fire the flush trigger. Still
  //    retractable: the UP-arrow handler in the composer pulls every
  //    queued item back into one editable composer draft (Claude
  //    TUI's `popAllEditable` behaviour). Rendered with `↳` prefix +
  //    dim italic — same style as the previous architecture's queued
  //    look so the muscle memory of "italic ↳ = queued" is preserved.
  //  - Zone 2 — items the dispatcher has flushed to the provider but
  //    whose wire echo (Claude `--replay-user-messages` envelope or
  //    Codex `item/completed userMessage`) has not yet stamped
  //    `provider_item_id` onto the optimistic timeline row. Rendered
  //    with `⟳` prefix + lower opacity — the visual delta from Zone 1
  //    signals "no longer retractable, on its way." Drops as soon as
  //    the matching `provider:item_event` upsert proves the wire
  //    confirmed delivery (events.ts → confirmFlushedByUserItemId).
  //
  // Empty state renders nothing — composerOverlay's ResizeObserver
  // shrinks `--composer-height` so the timeline reclaims the vertical
  // space.
  //
  // Lives OUTSIDE `<VList>` per chat/CLAUDE.md "Render OUTSIDE the
  // virtualizer" rule. Height changes propagate via
  // `--composer-height`. No transition:slide adjacent to the scroll
  // area — see chat/CLAUDE.md "No transition:slide" rule.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    getFlushedForThread,
    getQueueForThread,
  } from '../../stores/sendQueue.svelte';
  import { getActiveTurn, hasPendingSend } from '../../stores/threadStatuses.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let queued = $derived(getQueueForThread(pane.threadId ?? ''));
  let flushed = $derived(getFlushedForThread(pane.threadId ?? ''));
  let hasAny = $derived(queued.length > 0 || flushed.length > 0);

  // Pull up against the working indicator (mb-6) when the indicator
  // is showing — same trick LiveTodoPanel uses so both read as
  // adjacent lines of one logical "what's happening" block.
  let pulledUp = $derived(
    getActiveTurn(pane.threadId) !== null
    || hasPendingSend(pane.threadId),
  );
</script>

{#if hasAny && pane.threadId}
  <div
    class={`mb-2 flex flex-col gap-1 pl-1.5 text-[12px] ${pulledUp ? '-mt-5' : ''}`}
    data-testid="send-queue-preview"
    aria-label="Queued messages"
  >
    {#if flushed.length > 0}
      <div class="flex flex-col gap-1" data-testid="send-queue-zone-flushed">
        {#each flushed as item (item.userItemId)}
          <div
            class="flex items-start gap-2"
            data-testid="send-queue-preview-flushed-row"
          >
            <span class="select-none pt-0.5 text-fg-hint/40" aria-hidden="true">⟳</span>
            <span class="line-clamp-3 flex-1 px-1 py-0.5 italic text-fg-muted/55">
              {item.message}
            </span>
          </div>
        {/each}
      </div>
    {/if}
    {#if queued.length > 0}
      <div class="flex flex-col gap-1" data-testid="send-queue-zone-queued">
        {#each queued as item (item.id)}
          <div
            class="flex items-start gap-2"
            data-testid="send-queue-preview-row"
          >
            <span class="select-none pt-0.5 text-fg-hint/55" aria-hidden="true">↳</span>
            <span class="line-clamp-3 flex-1 px-1 py-0.5 italic text-fg-muted/85">
              {item.message}
            </span>
          </div>
        {/each}
      </div>
    {/if}
  </div>
{/if}
