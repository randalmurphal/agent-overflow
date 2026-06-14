<script lang="ts">
  import { untrack } from 'svelte';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';
  import {
    COMPACTION_PAYLOAD_EXPANSION_STATE_KEY,
    payloadVersionForItem,
  } from '../../utils/payloadVersion';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  const label = $derived(item.summary?.trim() || 'Context compacted');

  // The claudetui provider links a "compaction" payload carrying the
  // summarizer's reasoning + committed summary; headless claude / Codex
  // boundaries carry none and stay a plain, non-interactive divider.
  const hasCapture = $derived(
    item.payloadKind === 'compaction' && Boolean(item.payloadId),
  );

  // No-pane fallback (tests, detached renders). The compaction payload is
  // immutable once linked, so the default module cache and the settled
  // payloadVersion are correct — no streaming freshness handling needed.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          {
            loadMode: 'full',
            payloadVersion: () => payloadVersionForItem(item),
          },
        ),
  );
  const expansionRef = useLeasedItemExpansion({
    enabled: () => hasCapture,
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
    getOptions: () => ({
      loadMode: 'full',
      stateKey: COMPACTION_PAYLOAD_EXPANSION_STATE_KEY,
      // Module-scope helper only: the pane registry retains this callback
      // for the entry's lifetime (see RowExpansionStateOptions).
      payloadVersion: payloadVersionForItem,
    }),
  });
  const expansion = $derived(expansionRef.current);
  const expanded = $derived(expansion?.expanded ?? false);

  // Payload data is the raw committed summary text (same shape as a thinking
  // payload — raw text, not a JSON wrapper), loaded on expand. The summarizer's
  // reasoning streamed live as its own `compaction_reasoning` row, so it is not
  // part of this payload; the divider is summary-only.
  const summary = $derived(expansion?.displayData ?? '');

  async function handleToggle(): Promise<void> {
    if (!expansion) return;
    if (expanded) expansion.collapse();
    else await expansion.expand();
  }
</script>

<div data-testid="compaction-divider" class="my-8">
  <div
    class="flex items-center gap-3 text-[0.625rem] uppercase tracking-[0.18em] text-fg-subtle"
  >
    <div class="h-px flex-1 bg-border-subtle"></div>
    {#if hasCapture}
      <button
        type="button"
        data-testid="compaction-toggle"
        aria-expanded={expanded}
        aria-controls={`compaction-detail-${item.id}`}
        aria-label="Toggle compaction summary"
        class="flex cursor-pointer items-center gap-1.5 bg-transparent uppercase tracking-[0.18em] text-fg-subtle transition-colors hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        onclick={(event) => preservePaneScrollAnchor(pane, event, handleToggle)}
      >
        <span
          class="transition-transform duration-150"
          class:rotate-90={expanded}
          aria-hidden="true"
        >
          <Icon icon={ChevronRight} size={11} strokeWidth={2} class="opacity-70" />
        </span>
        <span>{label}</span>
      </button>
    {:else}
      <span>{label}</span>
    {/if}
    <div class="h-px flex-1 bg-border-subtle"></div>
  </div>

  {#if hasCapture && expanded}
    <div
      id={`compaction-detail-${item.id}`}
      data-testid="compaction-detail"
      class="mx-auto mt-3 max-w-2xl rounded-[var(--radius-control)] border border-border-subtle bg-surface-2/20 px-4 py-3 text-xs leading-relaxed text-fg-muted"
    >
      <div data-testid="compaction-summary" class="whitespace-pre-wrap">{summary}</div>
    </div>
  {/if}
</div>
