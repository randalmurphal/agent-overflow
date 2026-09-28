<script lang="ts" module>
  export const WORKFLOW_DIFF_BLOCK_LINES = 200;
  export const WORKFLOW_DIFF_LINE_PX = 20;
  export const WORKFLOW_DIFF_PAD_PX = 8;
</script>

<script lang="ts">
  import { untrack } from 'svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import WorkflowDiffBlock from './WorkflowDiffBlock.svelte';
  import type { ReviewFile } from '../../utils/patchStore';
  import type { RowEstimate } from '../../utils/virtual/types';

  // An expanded file of the gate diff: its raw patch lines in a bounded
  // box, virtualized in blocks so a file of any length opens at once and
  // stays reachable. Blocks read their text from the diff's compact
  // storage (WorkflowDiffBlock); nothing materializes the whole file.

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  interface Props {
    file: ReviewFile;
  }
  let { file }: Props = $props();

  interface Block {
    start: number;
    end: number;
  }

  const blocks = $derived.by(() => {
    const out: Block[] = [];
    for (let start = 0; start < file.body.lineCount; start += WORKFLOW_DIFF_BLOCK_LINES) {
      out.push({ start, end: Math.min(file.body.lineCount, start + WORKFLOW_DIFF_BLOCK_LINES) });
    }
    return out;
  });

  // The first and last blocks carry the box's vertical padding.
  function padTop(block: Block): number {
    return block.start === 0 ? WORKFLOW_DIFF_PAD_PX : 0;
  }

  function padBottom(block: Block): number {
    return block.end === file.body.lineCount ? WORKFLOW_DIFF_PAD_PX : 0;
  }

  // Stable identity for the virtualizer's constructor; reads the current
  // blocks. Every block's height is exact.
  const estimate: RowEstimate = {
    at: (index) => {
      const block = blocks[index];
      if (!block) return WORKFLOW_DIFF_LINE_PX;
      return (block.end - block.start) * WORKFLOW_DIFF_LINE_PX + padTop(block) + padBottom(block);
    },
    isExact: () => true,
  };

  let scrollEl: HTMLElement | undefined = $state();
  // The widest line rendered so far. Rows contain their layout, so a long
  // line cannot widen the box itself; the rows' container does.
  let contentWidth = $state(0);

  function noteWidth(px: number): void {
    if (px > contentWidth) contentWidth = px;
  }
  let listRef: LongListHandle | undefined = $state();

  // This box is the only writer of its scrollTop.
  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }

  // The engine tail-seeds until its first scroll input; this box opens at
  // its top, so hand it the real offset once mounted.
  $effect(() => {
    const ref = listRef;
    if (ref) untrack(() => ref.revalidate());
  });
</script>

<div
  bind:this={scrollEl}
  class="max-h-72 overflow-auto border-t border-border-subtle bg-surface-0 text-[11px] leading-5"
  style:overflow-anchor="none"
  data-testid="wf-diff-hunks"
>
  <div style:min-width="{contentWidth}px">
  <LongListVirtualizer
    bind:this={listRef}
    data={blocks}
    getKey={(block) => block.start}
    scrollRef={scrollEl}
    {estimate}
    renderAll={IS_TEST}
    {applyScrollTarget}
    onCompensation={(compensation) => applyScrollTarget(compensation.target)}
  >
    {#snippet children(block: Block)}
      <WorkflowDiffBlock
        {file}
        start={block.start}
        end={block.end}
        padTop={padTop(block)}
        padBottom={padBottom(block)}
        onWidth={noteWidth}
      />
    {/snippet}
  </LongListVirtualizer>
  </div>
</div>
