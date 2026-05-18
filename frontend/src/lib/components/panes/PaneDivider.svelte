<script lang="ts">
  import { onDestroy } from 'svelte';
  import { getMinPaneWidth } from '../../stores/paneDensity.svelte';
  import { getPaneWidth } from '../../stores/layoutMetrics.svelte';
  import { resizeAdjacentPaneLayoutItems } from '../../stores/paneLayout.svelte';
  import { createResizeGesture } from '../../utils/resizeGesture.svelte';

  interface Props {
    leftPaneId: string;
    rightPaneId: string;
  }

  let { leftPaneId, rightPaneId }: Props = $props();
  let startLeftWidth = 0;
  let startRightWidth = 0;

  const resize = createResizeGesture(() => {
    startLeftWidth = getPaneWidth(leftPaneId);
    startRightWidth = getPaneWidth(rightPaneId);
    return {
      axis: 'x',
      cursor: 'col-resize',
      currentSize: startLeftWidth,
      minSize: getMinPaneWidth(),
      maxSize: Math.max(getMinPaneWidth(), startLeftWidth + startRightWidth - getMinPaneWidth()),
      direction: 1,
      onResizeLive: (next) => {
        resizeAdjacentPaneLayoutItems(
          leftPaneId,
          rightPaneId,
          startLeftWidth,
          startRightWidth,
          next - startLeftWidth,
          getMinPaneWidth(),
        );
      },
      onResizeEnd: () => {},
    };
  });

  onDestroy(() => {
    resize.destroy();
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="vertical"
  aria-label="Resize Panes"
  class={[
    'relative z-20 w-1 shrink-0 cursor-col-resize select-none touch-none',
    'bg-border-subtle/30 hover:bg-accent/40 transition-colors',
    resize.dragging ? 'bg-accent/60' : '',
  ].join(' ')}
  onpointerdown={resize.onPointerDown}
  onpointermove={resize.onPointerMove}
  onpointerup={resize.endDrag}
  onpointercancel={resize.endDrag}
  data-testid="pane-divider"
></div>
