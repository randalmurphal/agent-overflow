<script lang="ts">
  /*
   * Right-edge resize handle for the sidebar. Mirrors the pointer-capture
   * pattern in primitives/Drawer.svelte — we don't reuse Drawer directly
   * because the sidebar is its own aside, not a drawer wrapping a
   * content region.
   *
   * Width bounds + persistence live in stores/sidebarLayout; during a
   * drag we call the live setter on every pointermove and flush to
   * localStorage once on pointerup. That keeps the disk from getting
   * hammered at 60-120 Hz while the user drags.
   */
  import { onDestroy } from 'svelte';
  import {
    SIDEBAR_MIN_WIDTH,
    getSidebarMaxWidth,
  } from '../../stores/sidebarLayout.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { createResizeGesture } from '../../utils/resizeGesture.svelte';

  interface Props {
    width: number;
    onResizeLive: (width: number) => void;
    onResizeEnd: () => void;
    /**
     * Active pane whose timeline scroll-controller should suspend
     * auto-follow during the drag. Width changes here reflow every
     * paragraph in the chat column, so without this lease a concurrent
     * stream chunk would fire the controller's content-RO and sync-pin
     * scrollTop mid-drag, yanking the user.
     * Idempotent — when the pane has no registered controller (timeline
     * not mounted yet, or pane is settings/empty), the lease is a no-op.
     */
    pane?: ThreadPane;
  }

  let { width, onResizeLive, onResizeEnd, pane }: Props = $props();

  const resize = createResizeGesture(() => ({
    axis: 'x',
    cursor: 'col-resize',
    currentSize: width,
    minSize: SIDEBAR_MIN_WIDTH,
    // Freeze the maximum at drag-start so a viewport resize mid-drag
    // doesn't yank the handle.
    maxSize: getSidebarMaxWidth(),
    direction: 1,
    onResizeLive,
    onResizeEnd: () => onResizeEnd(),
    acquireLease: () => pane?.scrollController?.pauseAutoScroll() ?? null,
  }));

  // If the resizer is torn down mid-drag (window close, sidebar swap,
  // HMR in dev) the body would otherwise stay stuck on col-resize +
  // userSelect:none, AND the timeline's pause-lease would never release.
  // Restore defensively.
  onDestroy(() => {
    resize.destroy();
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="vertical"
  aria-label="Resize Sidebar"
  aria-valuenow={width}
  aria-valuemin={SIDEBAR_MIN_WIDTH}
  class={[
    'absolute top-0 bottom-0 right-0 w-1 cursor-col-resize z-20',
    // select-none disables the browser's text-selection anchor on the
    // handle itself; touch-none disables native pan/zoom so a touch
    // drag goes straight to our pointer handlers.
    'select-none touch-none',
    'hover:bg-accent/30 transition-colors',
    resize.dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={resize.onPointerDown}
  onpointermove={resize.onPointerMove}
  onpointerup={resize.endDrag}
  onpointercancel={resize.endDrag}
  data-testid="sidebar-resizer"
></div>
