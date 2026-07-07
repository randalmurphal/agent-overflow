<script lang="ts">
  import { untrack } from 'svelte';
  import ExternalLink from 'lucide-svelte/icons/external-link';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import AnsiText from '../chat/AnsiText.svelte';
  import Icon from '../primitives/Icon.svelte';
  import TimelineVirtualizer from '../virtual/TimelineVirtualizer.svelte';
  import { createReviewScrollOwner } from './reviewScroll';
  import { OpenExternalURL } from '../../stores/bindings';
  import type { CIJobLogResult } from '../../types/models';
  import type { CILogView } from '../../stores/reviewPane.svelte';
  import { ciStatusDotClass, ciStatusTextClass, formatCIDuration } from '../../utils/ciStatus';
  import type { RowEstimate, TimelineVirtualizerHandle } from '../../utils/virtual/types';

  // CI job log view — replaces the diff body (same pattern as the
  // conflict viewer). The log is chunked into fixed line blocks and
  // virtualized; each block renders through AnsiText (CI traces are
  // ANSI-heavy). Bottom-anchored on load: failures live at the tail.

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  const CHUNK_LINES = 200;
  const LINE_ESTIMATE_PX = 18;

  interface Props {
    view: CILogView;
    log: CIJobLogResult | null;
    loading: boolean;
    error: string | null;
    savedPath: string | null;
    onBack: () => void;
    onRefresh: () => void;
    onSave: () => void;
    onSend: () => void;
  }

  let { view, log, loading, error, savedPath, onBack, onRefresh, onSave, onSend }: Props = $props();

  interface LogChunk {
    id: number;
    text: string;
    lines: number;
  }

  const chunks = $derived.by(() => {
    const text = log?.text ?? '';
    if (!text) return [] as LogChunk[];
    const lines = text.split('\n');
    if (lines.at(-1) === '') lines.pop();
    const out: LogChunk[] = [];
    for (let start = 0; start < lines.length; start += CHUNK_LINES) {
      const slice = lines.slice(start, start + CHUNK_LINES);
      out.push({ id: start, text: slice.join('\n'), lines: slice.length });
    }
    return out;
  });

  // Stable wrapper reading the current derived — same estimate-coherence
  // contract as ReviewDiffBody (the engine takes its estimate once).
  const estimate: RowEstimate = {
    at: (index) => (chunks[index]?.lines ?? CHUNK_LINES) * LINE_ESTIMATE_PX,
    isExact: () => false,
  };
  const getKey = (chunk: LogChunk) => chunk.id;

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: TimelineVirtualizerHandle | undefined = $state();
  const scroll = createReviewScrollOwner(() => scrollEl);

  // Bottom-anchor each newly loaded log exactly once (per log identity):
  // failures are at the tail. Manual scrolling afterwards stays put.
  let anchoredLog: CIJobLogResult | null = null;
  $effect(() => {
    const current = log;
    const ref = listRef;
    if (!current || !ref || chunks.length === 0 || anchoredLog === current) return;
    anchoredLog = current;
    untrack(() => ref.scrollToIndex(chunks.length - 1));
  });

  const totalMB = $derived(((log?.totalBytes ?? 0) / (1024 * 1024)).toFixed(1));
  const steps = $derived(view.job.steps ?? []);
</script>

<div class="flex min-h-0 flex-1 flex-col" data-testid="review-ci-log">
  <div class="flex items-center gap-2 border-b border-border bg-surface-1 px-3 py-2 text-xs">
    <span class="h-2 w-2 shrink-0 rounded-full {ciStatusDotClass(view.job.status)}"></span>
    <span class="min-w-0 truncate font-mono text-fg" title={view.job.name}>{view.job.name}</span>
    <span class="shrink-0 text-fg-subtle">{view.stageName}</span>
    <span class="shrink-0 {ciStatusTextClass(view.job.status)}">{view.job.status}</span>
    {#if formatCIDuration(view.job.durationSeconds)}
      <span class="shrink-0 tabular-nums text-fg-subtle">{formatCIDuration(view.job.durationSeconds)}</span>
    {/if}
    <span class="min-w-0 flex-1"></span>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle p-1 text-fg-muted hover:text-fg disabled:opacity-50"
      title="Refresh log"
      aria-label="Refresh log"
      disabled={loading}
      onclick={onRefresh}
    >
      <Icon icon={RefreshCw} size={12} class={loading ? 'animate-spin' : ''} />
    </button>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      data-testid="review-ci-log-save"
      onclick={onSave}
    >
      Save to file
    </button>
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      data-testid="review-ci-log-send"
      onclick={onSend}
    >
      Send to chat
    </button>
    {#if view.job.url}
      <button
        type="button"
        class="shrink-0 text-fg-subtle hover:text-fg"
        title="Open job in browser"
        aria-label="Open job in browser"
        onclick={() => { if (view.job.url) void OpenExternalURL(view.job.url); }}
      >
        <Icon icon={ExternalLink} size={12} />
      </button>
    {/if}
    <button
      type="button"
      class="shrink-0 rounded border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:text-fg"
      onclick={onBack}
    >
      Back
    </button>
  </div>

  {#if steps.length > 0}
    <div class="flex flex-wrap items-center gap-1 border-b border-border-subtle bg-surface-0/60 px-3 py-1.5 text-[0.6875rem]" data-testid="review-ci-steps">
      {#each steps as step (step.number)}
        <span class="flex items-center gap-1 rounded bg-surface-2/40 px-1.5 py-0.5" title="{step.name}: {step.status}">
          <span class="h-1.5 w-1.5 rounded-full {ciStatusDotClass(step.status)}"></span>
          <span class="max-w-48 truncate text-fg-muted">{step.name}</span>
        </span>
      {/each}
    </div>
  {/if}

  {#if savedPath}
    <div class="border-b border-border-subtle bg-surface-0/60 px-3 py-1.5 font-mono text-[0.6875rem] text-fg-muted" data-testid="review-ci-log-saved">
      Saved to {savedPath}
    </div>
  {/if}
  {#if error}
    <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-ci-log-error">
      {error}
    </div>
  {/if}
  {#if log?.truncated}
    <div class="border-b border-warning/30 bg-warning/10 px-3 py-1.5 text-[0.6875rem] text-warning" data-testid="review-ci-log-truncated">
      Showing the tail of a {totalMB} MB log — Save to file for the full log.
    </div>
  {/if}

  {#if loading && !log}
    <div class="px-4 py-3 text-xs text-fg-muted">Loading log…</div>
  {:else if !log || chunks.length === 0}
    {#if !error}
      <div class="px-4 py-3 text-xs text-fg-muted" data-testid="review-ci-log-empty">Log is empty.</div>
    {/if}
  {:else}
    <!-- Scrollable region: tabindex makes keyboard scrolling reachable
         (the axe scrollable-region-focusable pattern). -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
      bind:this={scrollEl}
      class="min-h-0 flex-1 overflow-y-auto focus:outline-none"
      style:overflow-anchor="none"
      tabindex="0"
      role="region"
      aria-label="CI job log"
      data-testid="review-ci-log-scroll"
    >
      <TimelineVirtualizer
        bind:this={listRef}
        data={chunks}
        {getKey}
        scrollRef={scrollEl}
        {estimate}
        renderAll={IS_TEST}
        applyScrollTarget={scroll.applyScrollTarget}
        onCompensation={scroll.applyCompensation}
      >
        {#snippet children(chunk: LogChunk)}
          <AnsiText source={chunk.text} class="whitespace-pre-wrap break-all px-3 font-mono text-xs leading-[18px] text-text-secondary" />
        {/snippet}
      </TimelineVirtualizer>
    </div>
  {/if}
</div>
