<script lang="ts">
  import { GetPayloadData } from '../../stores/bindings';
  import { MAX_INLINE_BYTES, shouldLazyLoad, truncateForPreview } from '../../utils/inlineThreshold';

  interface Props {
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

  let { payloadId, preview, label = 'Show all' }: Props = $props();

  let expanded = $state(false);
  let loading = $state(false);
  let loadError = $state<string | null>(null);
  let fullContent = $state<string | null>(null);

  // Threshold check is on the preview text itself. A caller that already
  // knows the preview is short but still wants the button can pass any
  // preview > MAX_INLINE_BYTES to force the control to appear.
  const previewIsLarge = $derived(shouldLazyLoad(preview));
  const canExpand = $derived(Boolean(payloadId) && (previewIsLarge || expanded));
  const displayPreview = $derived(truncateForPreview(preview, MAX_INLINE_BYTES));

  async function toggle() {
    if (expanded) {
      expanded = false;
      return;
    }
    expanded = true;
    if (fullContent !== null || !payloadId) return;

    loading = true;
    loadError = null;
    try {
      fullContent = await GetPayloadData(payloadId);
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }
</script>

{#if expanded}
  {#if loading}
    <p class="text-xs text-text-secondary animate-pulse" role="status" aria-live="polite" data-testid="lazy-content-loading">
      Loading…
    </p>
  {:else if loadError}
    <p class="text-xs text-error" role="alert" data-testid="lazy-content-error">
      Failed to load: {loadError}
    </p>
  {:else if fullContent !== null}
    <pre class="whitespace-pre-wrap break-words text-xs text-text-secondary" data-testid="lazy-content-full">{fullContent}</pre>
  {:else}
    <pre class="whitespace-pre-wrap break-words text-xs text-text-secondary" data-testid="lazy-content-preview">{displayPreview}</pre>
  {/if}
{:else}
  <p class="text-xs text-text-secondary" data-testid="lazy-content-preview">{displayPreview}</p>
{/if}

{#if canExpand}
  <button
    type="button"
    onclick={toggle}
    aria-expanded={expanded}
    data-testid="lazy-content-toggle"
    class="mt-1 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
  >
    {expanded ? 'Show less' : label}
  </button>
{/if}
