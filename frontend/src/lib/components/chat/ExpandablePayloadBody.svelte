<script lang="ts">
  import type { Snippet } from 'svelte';
  import AnsiText from './AnsiText.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
  import { formatPayloadSize } from '../../utils/payloadExpansion.svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

  let {
    pane,
    expansion,
    id,
    testPrefix,
    bodyTestId = `${testPrefix}-body`,
    outputTestId = `${testPrefix}-output`,
    emptyMessage,
    deferredOutputState = '',
    deferredOutputError = '',
    renderContent,
    copyLabel = 'Copy output',
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    expansion: PayloadExpansionHandle;
    id: string;
    testPrefix: string;
    bodyTestId?: string;
    outputTestId?: string;
    emptyMessage: string;
    deferredOutputState?: string;
    deferredOutputError?: string;
    /**
     * Optional content renderer that replaces the default `<AnsiText>`
     * branch. Receives the resolved `displayData` and the
     * `outputTestId` so callers can wire their own renderer (e.g.
     * `AdvisorRow`'s prose body) while keeping the surrounding
     * loading / error / show-more / copy-footer chrome consistent.
     */
    renderContent?: Snippet<[{ data: string; testId: string }]>;
    /**
     * Override for the CopyFooter button label. Defaults to
     * "Copy output" — prose bodies like AdvisorRow override to
     * "Copy response" since the payload is text rather than
     * terminal output.
     */
    copyLabel?: string;
  } = $props();
</script>

<div
  {id}
  class="ml-5 border-l border-border-subtle bg-surface-0/35"
  data-testid={bodyTestId}
>
  {#if expansion.loading}
    <p class="px-3 py-2 text-[0.6875rem] text-fg-subtle animate-pulse" role="status" aria-live="polite">
      Loading…
    </p>
  {:else if expansion.error}
    <div class="space-y-2 px-3 py-2">
      <p class="text-[0.6875rem] text-error" role="alert">
        Failed to load: {expansion.error}
      </p>
      <button
        type="button"
        class="text-[0.6875rem] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        onclick={() => expansion.retry()}
        data-testid="{testPrefix}-retry"
      >
        Retry
      </button>
    </div>
  {:else if expansion.displayData !== null}
    {#if renderContent}
      {@render renderContent({ data: expansion.displayData, testId: outputTestId })}
    {:else}
      <div
        class="ansi-body min-w-0 max-w-full max-h-60 overflow-y-auto overflow-x-hidden whitespace-pre-wrap break-words px-3 py-2 text-[0.6875rem] leading-relaxed text-fg-muted"
        use:nestedScroll
        data-testid={outputTestId}
      >
        <AnsiText source={expansion.displayData} class="whitespace-pre-wrap break-all" />
      </div>
    {/if}
    {#if expansion.hasMore}
      <button
        type="button"
        class="mx-3 mb-3 text-[0.6875rem] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        onclick={(event) => preservePaneScrollAnchor(pane, event, () => expansion.showFull())}
        data-testid="{testPrefix}-show-full"
      >
        Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
      </button>
    {/if}
    {#if expansion.displayData}
      <CopyFooter text={expansion.displayData} label={copyLabel} />
    {/if}
  {:else if deferredOutputState === 'loading'}
    <p class="px-3 py-2 text-[0.6875rem] text-fg-subtle animate-pulse" role="status" aria-live="polite">
      Loading…
    </p>
  {:else if deferredOutputState === 'error'}
    <p class="px-3 py-2 text-[0.6875rem] text-error" role="alert">
      Failed to load: {deferredOutputError || 'Background output could not be loaded.'}
    </p>
  {:else}
    <p class="px-3 py-2 text-[0.6875rem] text-fg-subtle italic">
      {emptyMessage}
    </p>
  {/if}
</div>
