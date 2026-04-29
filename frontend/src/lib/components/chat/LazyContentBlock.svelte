<script lang="ts">
  import { untrack } from 'svelte';
  import { MAX_INLINE_BYTES, shouldLazyLoad, truncateForPreview } from '../../utils/inlineThreshold';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createPayloadExpansion,
    compactPayloadVersion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';

  interface Props {
    /** Pane for the per-payload expansion registry. When omitted, falls
     * back to local state — fine for unit tests, but in chat surfaces
     * the registry preserves expand state and loaded chunks across
     * virtua's overscan eviction. */
    pane?: ThreadPane;
    threadId?: string;
    /**
     * Payload id the full body lives under, or undefined if only `preview`
     * is available. When undefined, the "Show all" button is suppressed
     * unless `preview` itself is short enough to skip the threshold.
     */
    payloadId: string | undefined;
    /**
     * Bounded preview shown before expansion. Truncated visually when it
     * exceeds MAX_INLINE_BYTES; no trimming happens when the preview is
     * already short.
     */
    preview: string;
    /**
     * Optional label for the expand button. Default "Show all".
     */
    label?: string;
  }

  let { pane, threadId, payloadId, preview, label = 'Show all' }: Props = $props();

  // Use the pane's payload-keyed registry when payloadId is defined and
  // pane is available. When payloadId is undefined the expand button is
  // suppressed entirely, so caching doesn't matter — local state is fine.
  // pane + payloadId stable across a row's lifetime; read once via `untrack`.
  const localFallback = untrack(() =>
    (pane && payloadId)
      ? null
      : createPayloadExpansion(
          () => payloadId,
          () => threadId,
          { payloadVersion: () => compactPayloadVersion(preview) },
        ),
  );
  const expansion = $derived.by(() => {
    if (pane && payloadId) {
      return pane.expansionStateForPayload(
        payloadId,
        threadId ?? '',
        compactPayloadVersion(preview),
      );
    }
    return localFallback!;
  });
  keepExpandedPayloadFresh(() => expansion, () => Boolean(payloadId));

  // Threshold check is on the preview text itself. A caller that already
  // knows the preview is short but still wants the button can pass any
  // preview > MAX_INLINE_BYTES to force the control to appear.
  const previewIsLarge = $derived(shouldLazyLoad(preview));
  const canExpand = $derived(Boolean(payloadId) && (previewIsLarge || expansion.expanded));
  const displayPreview = $derived(truncateForPreview(preview, MAX_INLINE_BYTES));

  async function toggle() {
    if (!payloadId) return;
    await expansion.toggle();
  }
</script>

{#if expansion.expanded}
  {#if expansion.loading}
    <p class="text-xs text-text-secondary animate-pulse" role="status" aria-live="polite" data-testid="lazy-content-loading">
      Loading…
    </p>
  {:else if expansion.error}
    <p class="text-xs text-error" role="alert" data-testid="lazy-content-error">
      Failed to load: {expansion.error}
    </p>
  {:else}
    <div data-testid={expansion.fullData !== null ? 'lazy-content-full' : 'lazy-content-preview'}>
      <AnsiText source={expansion.displayData ?? ''} class="whitespace-pre-wrap break-words text-xs text-text-secondary" />
    </div>
    {#if expansion.hasMore}
      <button
        type="button"
        onclick={() => expansion.showFull()}
        class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        data-testid="lazy-content-show-full"
      >
        Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
      </button>
    {/if}
  {/if}
{:else}
  <p class="text-xs text-text-secondary" data-testid="lazy-content-preview">{displayPreview}</p>
{/if}

{#if canExpand}
  <button
    type="button"
    onclick={toggle}
    aria-expanded={expansion.expanded}
    data-testid="lazy-content-toggle"
    class="mt-1 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
  >
    {expansion.expanded ? 'Show less' : label}
  </button>
{/if}
