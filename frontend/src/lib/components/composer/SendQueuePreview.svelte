<script lang="ts">
  // Send-queue overlay. Renders inside the composerOverlay
  // (ChatView.svelte) between the working indicator and the composer
  // textarea. Two visually distinct zones in arrival order:
  //
  //  - Zone 2 (rendered FIRST / on top) — items the dispatcher has
  //    flushed to the provider but whose wire echo (Claude
  //    `--replay-user-messages` envelope or Codex
  //    `item/completed userMessage`) has not yet stamped
  //    `provider_item_id` onto the timeline row. Reads as
  //    "in flight, headed to history" — `→` glyph, lower opacity than
  //    Zone 1, no retract affordance. Drops as soon as the matching
  //    `provider:item_event` upsert proves the wire confirmed delivery
  //    (events.ts → confirmFlushedByUserItemId).
  //  - Zone 1 (rendered BELOW Zone 2) — backend-queued items waiting
  //    for the next safe provider boundary. Still retractable: the UP-arrow handler in the
  //    composer pulls every queued item back into one editable
  //    composer draft (Claude TUI's `popAllEditable` behaviour).
  //    Rendered with the muscle-memory `↳` prefix + dim italic so the
  //    "italic ↳ = queued" visual still maps to "this is yours to
  //    retract".
  //
  // Stacking order makes natural sense as a launching queue: items
  // already on the way (Zone 2) sit ahead of the items still queued
  // behind them (Zone 1). The 1px hairline between zones (only when
  // both populated) reads as a state boundary without being loud.
  //
  // Empty state renders nothing — composerOverlay's ResizeObserver
  // shrinks `--composer-height` so the timeline reclaims the vertical
  // space.
  //
  // Lives OUTSIDE `<Virtualizer>` per chat/CLAUDE.md "Render OUTSIDE the
  // virtualizer" rule. Height changes propagate via
  // `--composer-height`. No transition:slide / transition:fade
  // adjacent to the scroll area — see chat/CLAUDE.md "No
  // transition:slide" rule. Rows mount and unmount instantly.

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
  let hasBothZones = $derived(queued.length > 0 && flushed.length > 0);

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
    class={`mb-2 flex flex-col gap-0.5 pl-1.5 text-[11px] leading-snug ${pulledUp ? '-mt-5' : ''}`}
    data-testid="send-queue-preview"
    aria-label="Queued user messages"
  >
    {#if flushed.length > 0}
      <ul
        class="flex flex-col gap-0.5"
        data-testid="send-queue-zone-flushed"
        aria-label="Messages awaiting wire confirmation"
      >
        {#each flushed as item (item.userItemId)}
          <li
            class="flex items-start gap-1.5"
            data-testid="send-queue-preview-flushed-row"
            data-zone="flushed"
            data-user-item-id={item.userItemId}
          >
            <span
              class="select-none pt-px font-mono text-fg-hint/45 animate-pulse"
              aria-hidden="true"
            >→</span>
            <span class="line-clamp-3 flex-1 italic text-fg-hint/70">
              {item.message}
            </span>
          </li>
        {/each}
      </ul>
    {/if}
    {#if hasBothZones}
      <!-- Hairline between zones. Visible state boundary so the eye
           reads "these two groups are different things" without
           heavier chrome. -->
      <div
        class="my-1 h-px w-12 bg-border-subtle/50"
        aria-hidden="true"
        data-testid="send-queue-zone-divider"
      ></div>
    {/if}
    {#if queued.length > 0}
      <ul
        class="flex flex-col gap-0.5"
        data-testid="send-queue-zone-queued"
        aria-label="Queued messages, retractable with up arrow"
      >
        {#each queued as item (item.id)}
          <li
            class="flex items-start gap-1.5"
            data-testid="send-queue-preview-row"
            data-zone="queued"
            data-queue-id={item.id}
          >
            <span
              class="select-none pt-px font-mono text-fg-hint/60"
              aria-hidden="true"
            >↳</span>
            <span class="line-clamp-3 flex-1 italic text-fg-muted/85">
              {item.message}
            </span>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
{/if}
