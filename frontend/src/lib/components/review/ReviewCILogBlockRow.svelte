<script lang="ts">
  import AnsiText from '../chat/AnsiText.svelte';
  import { CI_LOG_BLOCK_LINES, type CILogBlockCache } from '../../stores/ciLogBlocks.svelte';

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

  const ESC = String.fromCharCode(27);
  // Dim, as AnsiText renders SGR 2; reset after so the next line starts
  // clean.
  const CLIPPED_MARK = `${ESC}[2m … line clipped at 64 KB; Save to file has the whole line${ESC}[0m`;

  const block = $derived(blocks.block(index));
  const shown = $derived.by(() => {
    const lines = block?.lines;
    if (!lines) return null;
    const start = index * CI_LOG_BLOCK_LINES;
    const out: string[] = [];
    for (let line = from; line < to && line - start < lines.length; line += 1) {
      const text = lines[line - start] ?? '';
      out.push(block?.clipped.has(line) ? text + CLIPPED_MARK : text);
    }
    return out;
  });
  const missing = $derived(to - from - (shown?.length ?? 0));
</script>

{#if shown && shown.length > 0}
  <AnsiText source={shown.join('\n')} class="whitespace-pre-wrap break-all px-3 font-mono text-xs leading-[18px] text-text-secondary" />
{/if}
{#if missing > 0}
  {#if block?.error && !block.loading}
    <div
      class="px-3 py-1 text-xs text-error"
      style:min-height="{missing * linePx}px"
      data-testid="review-ci-log-block-error"
    >
      Lines {from + (shown?.length ?? 0) + 1} to {to} failed to load: {block.error}. Scroll away and back to retry.
    </div>
  {:else}
    <div
      class="mx-3 rounded-[var(--radius-control)] bg-surface-1/60"
      style:height="{missing * linePx}px"
      aria-hidden="true"
      data-testid="review-ci-log-block-placeholder"
    ></div>
  {/if}
{/if}
