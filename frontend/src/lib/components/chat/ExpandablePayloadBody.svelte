<script lang="ts">
  import AnsiText from './AnsiText.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
  import { formatPayloadSize } from '../../utils/payloadExpansion.svelte';

  let {
    expansion,
    id,
    testPrefix,
    bodyTestId = `${testPrefix}-body`,
    outputTestId = `${testPrefix}-output`,
    emptyMessage,
    deferredOutputState = '',
    deferredOutputError = '',
  }: {
    expansion: PayloadExpansionHandle;
    id: string;
    testPrefix: string;
    bodyTestId?: string;
    outputTestId?: string;
    emptyMessage: string;
    deferredOutputState?: string;
    deferredOutputError?: string;
  } = $props();
</script>

<div
  {id}
  class="ml-5 border-l border-border-subtle bg-surface-0/35"
  data-testid={bodyTestId}
>
  {#if expansion.loading}
    <p class="px-3 py-2 text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">
      Loading…
    </p>
  {:else if expansion.error}
    <div class="space-y-2 px-3 py-2">
      <p class="text-[11px] text-error" role="alert">
        Failed to load: {expansion.error}
      </p>
      <button
        type="button"
        class="text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        onclick={() => expansion.retry()}
        data-testid="{testPrefix}-retry"
      >
        Retry
      </button>
    </div>
  {:else if expansion.displayData !== null}
    <div
      class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 text-[11px] leading-relaxed text-fg-muted"
      data-testid={outputTestId}
    >
      <AnsiText source={expansion.displayData} />
    </div>
    {#if expansion.hasMore}
      <button
        type="button"
        class="mx-3 mb-3 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        onclick={() => expansion.showFull()}
        data-testid="{testPrefix}-show-full"
      >
        Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
      </button>
    {/if}
    {#if expansion.displayData}
      <CopyFooter text={expansion.displayData} label="Copy output" />
    {/if}
  {:else if deferredOutputState === 'loading'}
    <p class="px-3 py-2 text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">
      Loading…
    </p>
  {:else if deferredOutputState === 'error'}
    <p class="px-3 py-2 text-[11px] text-error" role="alert">
      Failed to load: {deferredOutputError || 'Background output could not be loaded.'}
    </p>
  {:else}
    <p class="px-3 py-2 text-[11px] text-fg-subtle italic">
      {emptyMessage}
    </p>
  {/if}
</div>
