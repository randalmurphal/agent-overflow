<script lang="ts">
  import { onDestroy } from 'svelte';
  import { getSettings } from '../../../stores/settings.svelte';
  import { parseDiffLines, type DiffLine } from '../../../utils/diff';

  interface Props {
    /** The unified diff text to render. Empty string -> empty-state message. */
    diff: string;
    /** Overrides the word-wrap setting; defaults to the user's setting. */
    wordWrap?: boolean;
    /** Max number of lines to render synchronously before switching to batched rendering. */
    syncLimit?: number;
    /** Batch size for progressive rendering. */
    batchSize?: number;
    /** Fallback message when `diff` is empty. */
    emptyMessage?: string;
  }

  const DEFAULT_SYNC_LIMIT = 500;
  const DEFAULT_BATCH = 200;

  let {
    diff,
    wordWrap,
    syncLimit = DEFAULT_SYNC_LIMIT,
    batchSize = DEFAULT_BATCH,
    emptyMessage = 'No changes.',
  }: Props = $props();

  let allLines = $derived(parseDiffLines(diff));
  let visibleCount = $state(0);
  let timerHandle: ReturnType<typeof setTimeout> | null = null;

  // Whenever `diff` changes, start rendering from the top. The effect sets
  // visibleCount synchronously for small diffs; for larger ones it schedules
  // batched appends via setTimeout(0) so the event loop stays responsive.
  $effect(() => {
    const total = allLines.length;
    if (timerHandle !== null) {
      clearTimeout(timerHandle);
      timerHandle = null;
    }
    if (total <= syncLimit) {
      visibleCount = total;
      return;
    }
    visibleCount = Math.min(syncLimit, total);
    appendBatches(total);
  });

  function appendBatches(total: number): void {
    if (visibleCount >= total) {
      timerHandle = null;
      return;
    }
    timerHandle = setTimeout(() => {
      const next = Math.min(visibleCount + batchSize, total);
      visibleCount = next;
      if (next < total) {
        appendBatches(total);
      } else {
        timerHandle = null;
      }
    }, 0);
  }

  onDestroy(() => {
    if (timerHandle !== null) {
      clearTimeout(timerHandle);
      timerHandle = null;
    }
  });

  let displayLines = $derived<DiffLine[]>(allLines.slice(0, visibleCount));
  let wrapActive = $derived(wordWrap ?? getSettings().diffWordWrap);
  let wrapClass = $derived(wrapActive ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');

  function classFor(type: DiffLine['type']): string {
    switch (type) {
      case 'added': return 'bg-success/10 text-success';
      case 'removed': return 'bg-error/10 text-error';
      case 'header': return 'text-accent/70';
      case 'context': return 'text-text-secondary';
    }
  }

  let progressVisible = $derived(visibleCount < allLines.length);
</script>

{#if diff.length === 0}
  <div class="px-3 py-6 text-sm text-text-secondary text-center" data-testid="diff-viewer-empty">
    {emptyMessage}
  </div>
{:else}
  <div class="overflow-x-auto bg-surface-0 px-3 py-2" data-testid="diff-viewer">
    <pre class="font-mono text-xs leading-tight {wrapClass}">{#each displayLines as line, i (i)}<span
        class={classFor(line.type)}
      >{line.content}
</span>{/each}</pre>
    {#if progressVisible}
      <div
        class="mt-2 text-[11px] text-text-secondary/80"
        role="status"
        aria-live="polite"
        data-testid="diff-viewer-progress"
      >
        Rendering large diff… {visibleCount.toLocaleString()} / {allLines.length.toLocaleString()} lines
      </div>
    {/if}
  </div>
{/if}
