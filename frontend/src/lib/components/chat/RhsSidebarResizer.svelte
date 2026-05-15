<script lang="ts">
  /*
   * Shared left-edge resize handle for any right-side panel. Every
   * RHS sidebar (diff, plan, checkpoint diff, and any future
   * addition) composes this so resize behavior is identical and
   * tunable in one place.
   *
   * Direction is inverted vs the threads sidebar: handle anchors on
   * the LEFT edge of a RIGHT-anchored panel, so a leftward drag
   * (negative delta) GROWS the panel.
   *
   * Width bounds are passed in by the shell. The parent owns the live
   * width + persistence; this component only emits move/end callbacks.
  */
  import { onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { createResizeGesture } from '../../utils/resizeGesture.svelte';

  interface Props {
    width: number;
    minWidth: number;
    /** Recomputed at drag-start so window-resize between drags is
     *  honoured without needing to subscribe to resize events. */
    getMaxWidth: () => number;
    onResizeLive: (next: number) => void;
    onResizeEnd: () => void;
    ariaLabel: string;
    testId?: string;
    /**
     * Active pane whose timeline scroll-controller should suspend
     * auto-follow during the drag. RHS panel resize narrows/widens the
     * chat column, reflowing every paragraph row. Without this lease a
     * concurrent stream chunk would fire the controller's content-RO
     * and sync-pin scrollTop mid-drag, yanking the user. Idempotent —
     * no-op when the pane has no registered controller.
     */
    pane?: ThreadPane;
  }

  let {
    width,
    minWidth,
    getMaxWidth,
    onResizeLive,
    onResizeEnd,
    ariaLabel,
    testId,
    pane,
  }: Props = $props();

  const resize = createResizeGesture(() => ({
    axis: 'x',
    cursor: 'col-resize',
    currentSize: width,
    minSize: minWidth,
    maxSize: getMaxWidth(),
    // Inverted direction: handle is on the LEFT edge of a RIGHT-anchored
    // panel, so a leftward drag (negative delta) grows the panel.
    direction: -1,
    onResizeLive,
    onResizeEnd: () => onResizeEnd(),
    acquireLease: () => pane?.scrollController?.pauseAutoScroll() ?? null,
  }));

  onDestroy(() => {
    resize.destroy();
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="vertical"
  aria-label={ariaLabel}
  aria-valuenow={width}
  aria-valuemin={minWidth}
  class={[
    'absolute top-0 bottom-0 left-0 w-1 cursor-col-resize z-20',
    'select-none touch-none',
    'hover:bg-accent/30 transition-colors',
    resize.dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={resize.onPointerDown}
  onpointermove={resize.onPointerMove}
  onpointerup={resize.endDrag}
  onpointercancel={resize.endDrag}
  data-testid={testId}
></div>
