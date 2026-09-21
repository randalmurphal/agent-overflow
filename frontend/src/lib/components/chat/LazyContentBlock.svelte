<script lang="ts">
  import { MAX_INLINE_BYTES, shouldLazyLoad, truncateForPreview } from '../../utils/inlineThreshold';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import {
    createPayloadExpansion,
    compactPayloadVersion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { useLeasedPayloadExpansion } from './useLeasedPayloadExpansion.svelte';

  type Props = {
    /** Pane for the per-payload expansion registry. When omitted, falls
     * back to local state — fine for unit tests, but in chat surfaces
     * the registry preserves expand state and loaded chunks across
     * the window's overscan eviction. */
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    threadId?: string;
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
  } & (
    | { payloadId: string | undefined; fullText?: undefined }
    | { payloadId?: undefined; fullText: string }
  );

  let { pane, threadId, payloadId, preview, fullText, label = 'Show all' }: Props = $props();

  const inlineBodyId = $props.id();
  let inlineExpanded = $state(false);
  $effect(() => { if (fullText === undefined) inlineExpanded = false; });

  // Keep a fallback ready when a row changes from a payload to inline content.
  const localFallback = createPayloadExpansion(
    () => payloadId,
    () => threadId,
    { payloadVersion: () => compactPayloadVersion(preview) },
  );
  const expansionRef = useLeasedPayloadExpansion({
    getPane: () => pane,
    getPayloadId: () => payloadId,
    getThreadId: () => threadId ?? '',
    getFallback: () => localFallback,
    getOptions: () => compactPayloadVersion(preview),
  });
  const expansion = $derived(expansionRef.current!);
  const expanded = $derived(fullText !== undefined ? inlineExpanded : expansion.expanded);
  keepExpandedPayloadFresh(() => expansion, () => Boolean(payloadId));

  // Threshold check is on the preview text itself. A caller that already
  // knows the preview is short but still wants the button can pass any
  // preview > MAX_INLINE_BYTES to force the control to appear.
  const previewIsLarge = $derived(shouldLazyLoad(preview));
  const canExpand = $derived((fullText !== undefined || Boolean(payloadId))
    && (previewIsLarge || expanded || (fullText !== undefined && fullText !== preview)));
  const displayPreview = $derived(truncateForPreview(preview, MAX_INLINE_BYTES));

  // One derived id for both halves of the disclosure (utils/chatDomIds.ts):
  // the toggle's `aria-controls` and the body's `id` must be one string.
  // Inline content uses an instance id. The body is always mounted (the preview is its
  // collapsed state), which an enclosing activity run's height cap accounts
  // for through the measured collapsed baseline (utils/activityRunClip.ts).
  const bodyDomId = $derived(
    payloadId ? chatRowDomId(pane, 'lazy-content', payloadId) : inlineBodyId,
  );

  async function toggle() {
    if (fullText !== undefined) { inlineExpanded = !inlineExpanded; return; }
    if (!payloadId) return;
    await expansion.toggle();
  }
</script>

<div id={bodyDomId}>
  {#if expanded}
    {#if fullText !== undefined}
      <div data-testid="lazy-content-full"><AnsiText source={fullText} class="whitespace-pre-wrap break-words text-xs text-text-secondary" /></div>
    {:else if expansion.loading}
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
          onclick={(event) => preservePaneScrollAnchor(pane, event, () => expansion.showFull())}
          class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
          data-testid="lazy-content-show-full"
        >
          Show more output ({formatPayloadSize(expansion.totalSize)}) ↓
        </button>
      {/if}
    {/if}
  {:else}
    <p class="text-xs text-text-secondary" data-testid="lazy-content-preview">{displayPreview}</p>
  {/if}
</div>

{#if canExpand}
  <button
    type="button"
    onclick={(event) => preservePaneScrollAnchor(pane, event, toggle)}
    aria-expanded={expanded}
    aria-controls={bodyDomId}
    data-testid="lazy-content-toggle"
    class="mt-1 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
  >
    {expanded ? 'Show less' : label}
  </button>
{/if}
