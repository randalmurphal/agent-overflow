<script lang="ts">
  import { MAX_INLINE_BYTES, shouldLazyLoad, truncateForPreview } from '../../utils/inlineThreshold';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

  interface Props {
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

  let { threadId, payloadId, preview, label = 'Show all' }: Props = $props();

  const expansion = createPayloadExpansion(() => payloadId, () => threadId);

  // Threshold check is on the preview text itself. A caller that already
  // knows the preview is short but still wants the button can pass any
  // preview > MAX_INLINE_BYTES to force the control to appear.
  const previewIsLarge = $derived(shouldLazyLoad(preview));
  const canExpand = $derived(Boolean(payloadId) && (previewIsLarge || expansion.expanded));
  const displayPreview = $derived(truncateForPreview(preview, MAX_INLINE_BYTES));

  $effect(() => {
    threadId;
    payloadId;
    preview;
    expansion.reset();
  });

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
    <pre class="ansi-body whitespace-pre-wrap break-words text-xs text-text-secondary" data-testid={expansion.fullData !== null ? 'lazy-content-full' : 'lazy-content-preview'}>{@html expansion.displayHtml ?? ''}</pre>
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
