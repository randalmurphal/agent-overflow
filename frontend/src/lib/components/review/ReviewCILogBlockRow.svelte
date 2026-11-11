<script lang="ts">
  import AnsiText from '../chat/AnsiText.svelte';
  import { CI_LOG_BLOCK_LINES, type CILogBlockCache } from '../../stores/ciLogBlocks.svelte';
  import { findCILogKey } from '../../utils/ciLogFind';

  // One row of log lines in the CI log view: lines [from, to) of the log,
  // all inside one block of the cache. Mounting holds the block, which
  // loads it when it is not current; unmounting lets the cache evict it.
  // Lines the block does not hold yet are a placeholder of their height,
  // or the block's load failure in their place; a stale block's lines
  // stay shown while its reload runs.

  interface Props {
    blocks: CILogBlockCache;
    from: number;
    to: number;
    /** The height one line is laid out at, which a placeholder keeps. */
    linePx: number;
  }

  let { blocks, from, to, linePx }: Props = $props();

  const index = $derived(Math.floor(from / CI_LOG_BLOCK_LINES));
  $effect(() => blocks.acquire(index));

  // After a line the backend clipped. Chrome, not log text: find neither
  // counts nor paints it (data-find-skip). A clipped line's color ends
  // with it, so the next line starts clean.
  const CLIPPED_NOTE = ' … line clipped at 64 KB; Save to file has the whole line';

  const block = $derived(blocks.block(index));
  // The shown lines as runs that each end at a clipped line or the last
  // line, so each clip note follows its line.
  const shown = $derived.by(() => {
    const lines = block?.lines;
    if (!lines) return null;
    const start = index * CI_LOG_BLOCK_LINES;
    const runs: { text: string; clipped: boolean }[] = [];
    let run: string[] = [];
    let count = 0;
    for (let line = from; line < to && line - start < lines.length; line += 1) {
      run.push(lines[line - start] ?? '');
      count += 1;
      if (block?.clipped.has(line)) {
        runs.push({ text: run.join('\n'), clipped: true });
        run = [];
      }
    }
    if (run.length > 0) runs.push({ text: run.join('\n'), clipped: false });
    return { runs, count };
  });
  const missing = $derived(to - from - (shown?.count ?? 0));
</script>

{#if shown && shown.count > 0}
  <div class="whitespace-pre-wrap break-all px-3 font-mono text-xs leading-[18px] text-text-secondary" data-find-key={findCILogKey(from)}>{#each shown.runs as run, at (at)}<AnsiText source={at > 0 ? `\n${run.text}` : run.text} class="inline" />{#if run.clipped}<span class="text-fg-subtle" data-find-skip>{CLIPPED_NOTE}</span>{/if}{/each}</div>
{/if}
{#if missing > 0}
  {#if block?.error && !block.loading}
    <div
      class="px-3 py-1 text-xs text-error"
      style:min-height="{missing * linePx}px"
      data-testid="review-ci-log-block-error"
    >
      Lines {from + (shown?.count ?? 0) + 1} to {to} failed to load: {block.error}. Scroll away and back to retry.
    </div>
  {:else}
    <div
      class="mx-3 rounded-[var(--radius-control)] bg-surface-2/50 px-2 py-1 text-xs text-fg-subtle"
      style:height="{missing * linePx}px"
      data-testid="review-ci-log-block-placeholder"
    >
      Loading lines {from + (shown?.count ?? 0) + 1} to {to}…
    </div>
  {/if}
{/if}
