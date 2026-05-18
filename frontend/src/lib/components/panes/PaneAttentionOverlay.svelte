<script lang="ts">
  import type { PaneLayoutItem } from '../../stores/paneLayout.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import {
    clampDotLeft,
    paneDotAnchorX,
    PANE_ATTENTION_DOT_WIDTH,
    resolvePaneAttentionDot,
  } from './paneAttention';

  interface Props {
    layoutItems: PaneLayoutItem[];
    scrollLeft: number;
    scrollClientWidth: number;
    paneOffsetLeftById: Map<string, number>;
    paneOffsetLeftFallback(paneId: string): number;
    onParkedClick(paneId: string): void;
  }

  let {
    layoutItems,
    scrollLeft,
    scrollClientWidth,
    paneOffsetLeftById,
    paneOffsetLeftFallback,
    onParkedClick,
  }: Props = $props();

  function offsetLeftFor(paneId: string): number {
    const cached = paneOffsetLeftById.get(paneId);
    if (cached !== undefined) return cached;
    return paneOffsetLeftFallback(paneId);
  }
</script>

<div class="pointer-events-none absolute inset-x-0 top-0 z-30 h-[var(--pane-header-height)]" data-testid="pane-attention-overlay">
  {#each layoutItems as item (item.id)}
    {@const pane = getPane(item.paneId)}
    {@const dot = resolvePaneAttentionDot(pane?.thread ?? null)}
    {#if dot}
      {@const geometry = clampDotLeft(
        paneDotAnchorX(offsetLeftFor(item.paneId)),
        scrollLeft,
        scrollLeft + scrollClientWidth,
        PANE_ATTENTION_DOT_WIDTH,
      )}
      <button
        type="button"
        aria-label={dot.pill.label}
        title={dot.pill.label}
        class={[
          'pointer-events-auto absolute top-2 h-2.5 w-2.5 rounded-full transition-[left,box-shadow,transform] duration-150',
          dot.pill.dotClass,
          dot.pill.pulse ? 'animate-pulse' : '',
          dot.pill.glowClass ?? '',
          geometry.parked ? 'ring-2 ring-surface-0/90 shadow-sm hover:scale-110' : '',
        ].join(' ')}
        style:left={`${geometry.left}px`}
        data-testid="pane-attention-dot"
        data-pane-id={item.paneId}
        data-status={dot.status}
        data-parked={geometry.parked}
        onclick={(event) => {
          event.stopPropagation();
          if (geometry.parked) onParkedClick(item.paneId);
        }}
      ></button>
    {/if}
  {/each}
</div>
