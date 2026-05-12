<script lang="ts">
  import { untrack } from 'svelte';
  import CopyFooter from './CopyFooter.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  // pane is stable across a row's lifetime; read once via `untrack`.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          { payloadVersion: () => item.updatedAt },
        ),
  );
  const expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);
  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  const isStreaming = $derived(item.status === 'streaming');

  // Tracks whether the user has clicked the disclosure since the row
  // entered the streaming phase. While the thinking block is still
  // streaming we render the body inline by default; on settle we
  // auto-collapse, unless the user has touched the toggle. Local
  // `$state` is fine here — the only race is virtua remounting a row
  // mid-stream past `bufferSize=900`, which is rare and the only cost
  // is a briefly-incorrect default before the user re-asserts intent.
  let userTouched = $state(false);

  // Body visibility:
  //  - Streaming, untouched: show the live deltas inline (default open).
  //  - Streaming, touched: respect `expansion.expanded` so a user
  //    toggle during the stream sticks.
  //  - Settled: standard disclosure behavior driven by
  //    `expansion.expanded`.
  const bodyVisible = $derived(
    isStreaming ? (userTouched ? expansion.expanded : true) : expansion.expanded,
  );

  // Content source. While streaming, `item.summary` carries the live
  // accumulated deltas (resolveDisplayItem in TimelineLeaf swaps in
  // `pane.liveItemSummaries[item.id]` so this row sees the full text
  // even though the persisted Summary is preview-truncated). After
  // settle we prefer the payload — `item.summary` after a thread
  // reload is just the persisted preview, while `expansion.displayData`
  // is the full body. Picking by length covers both cases without
  // needing a separate "is-reloaded" flag.
  const bodyText = $derived.by<string>(() => {
    const live = item.summary ?? '';
    const persisted = expansion.displayData ?? '';
    return persisted.length > live.length ? persisted : live;
  });

  const preview = $derived(
    item.summary.length > 200 ? item.summary.slice(0, 200) + '...' : item.summary,
  );

  // Settle hook. On the streaming → settled boundary, auto-collapse
  // (matches peer agent-side rows at rest) unless either (a) the user
  // explicitly toggled during the stream or (b) the user has scrolled
  // up to read the thinking text — in case (b) we pre-expand the
  // persisted payload so the body stays visible across the lifecycle
  // boundary instead of yanking content out from under them. The
  // expansion handle's `expanded` flag survives virtua remount via the
  // pane registry, so the chosen post-settle state is durable.
  // Initialized to false rather than $state(isStreaming) so we don't
  // capture a reactive read in an initializer (Svelte 5 warns); the
  // effect below runs on mount and seeds the latch via its trailing
  // assignment. If the row mounts already-streaming the first effect
  // run sees wasStreaming=false (no settle), then writes the latch to
  // true; the subsequent transition to settled fires the collapse path
  // correctly.
  let wasStreaming = $state(false);
  $effect(() => {
    const nowStreaming = isStreaming;
    if (wasStreaming && !nowStreaming && !userTouched) {
      const atBottom = pane?.scrollController?.isAtBottom ?? true;
      if (atBottom) {
        expansion.collapse();
      } else {
        void expansion.expand();
      }
    }
    wasStreaming = nowStreaming;
  });

  async function handleToggle() {
    // Capture visible-state BEFORE flipping userTouched, otherwise the
    // $derived would re-evaluate against the new userTouched=true read
    // and the first click on an untouched streaming row would read
    // `bodyVisible=false` (the post-touch value) and erroneously route
    // into the expand branch.
    const wasVisible = bodyVisible;
    userTouched = true;
    if (isStreaming) {
      if (wasVisible) {
        expansion.collapse();
      } else {
        await expansion.expand();
      }
    } else {
      await expansion.toggle();
    }
  }

  const copyText = $derived(bodyText);
</script>

<div class="group/thinking mb-1.5 overflow-hidden">
  <TranscriptDisclosureHeader
    expanded={bodyVisible}
    controls={`thinking-${item.id}`}
    ariaLabel="Toggle Thinking Block"
    testId="thinking-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    onToggle={() => handleToggle()}
  >
    <span class="text-[11px] text-fg-muted font-medium uppercase tracking-[0.04em] shrink-0">Thinking</span>
    {#if !bodyVisible}
      <span class="text-[12px] text-fg-muted/70 italic line-clamp-3 flex-1 min-w-0">{preview}</span>
    {/if}
  </TranscriptDisclosureHeader>

  {#if bodyVisible}
    <div class="ml-5 border-l border-border-subtle bg-surface-0/35">
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div id="thinking-{item.id}" class="px-3 py-2 max-h-80 overflow-y-auto" tabindex="0" role="region" aria-label="Thinking Content">
        {#if !isStreaming && expansion.loading && !bodyText}
          <p class="text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">Loading thinking content...</p>
        {:else if !isStreaming && expansion.error && !bodyText}
          <p class="text-[11px] text-error" role="alert">Failed to load: {expansion.error}</p>
        {:else}
          <AnsiText source={bodyText} class="text-[11px] text-fg-muted whitespace-pre-wrap leading-relaxed italic" />
          {#if !isStreaming && expansion.hasMore}
            <button
              type="button"
              class="mt-2 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
              onclick={() => expansion.showFull()}
              data-testid="thinking-show-full"
            >
              Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
            </button>
          {/if}
        {/if}
      </div>
      {#if !isStreaming && !expansion.loading && !expansion.error && copyText}
        <CopyFooter text={copyText} label="Copy thinking" />
      {/if}
    </div>
  {/if}
</div>
