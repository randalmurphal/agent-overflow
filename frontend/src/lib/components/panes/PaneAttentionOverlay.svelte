<script lang="ts">
  import type { PaneLayoutItem } from '../../stores/paneLayout.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { paneDotAnchorX, resolvePaneAttentionDot } from './paneAttention';

  interface Props {
    layoutItems: PaneLayoutItem[];
    paneOffsetLeftById: Map<string, number>;
  }

  let { layoutItems, paneOffsetLeftById }: Props = $props();

  function offsetLeftFor(paneId: string): number {
    return paneOffsetLeftById.get(paneId) ?? 0;
  }
</script>

<div
  class="pointer-events-none absolute inset-x-0 top-0 z-30 h-[var(--pane-header-height)]"
  data-testid="pane-attention-overlay"
>
  {#each layoutItems as item (item.id)}
    {@const pane = getPane(item.paneId)}
    {@const dot = resolvePaneAttentionDot(pane?.thread ?? null)}
    {#if dot}
      <!--
        Positioned at the pane's leading edge inside the host's scrolled
        coordinate space. When the pane is scrolled offscreen the dot
        moves out of view with it; the surrounding pane host clips
        anything past its overflow edge. The sidebar still surfaces
        attention for offscreen threads, so we don't need a sticky
        "parked" affordance here.
      -->
      <span
        aria-label={dot.pill.label}
        title={dot.pill.label}
        class={[
          'absolute top-2 h-2.5 w-2.5 rounded-full',
          dot.pill.dotClass,
          dot.pill.pulse ? 'animate-pulse' : '',
          dot.pill.glowClass ?? '',
        ].join(' ')}
        style:left={`${paneDotAnchorX(offsetLeftFor(item.paneId))}px`}
        data-testid="pane-attention-dot"
        data-pane-id={item.paneId}
        data-status={dot.status}
      ></span>
    {/if}
  {/each}
</div>
