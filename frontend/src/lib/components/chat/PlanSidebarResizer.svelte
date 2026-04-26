<script lang="ts">
  /*
   * Left-edge resize handle for the right-side plan sidebar. Mirrors
   * components/sidebar/SidebarResizer.svelte but the handle anchors
   * left and the drag direction is inverted: dragging LEFT widens the
   * panel (it grows toward the chat), dragging RIGHT shrinks it.
   *
   * Width bounds + persistence live in stores/planSidebarLayout; live
   * setter on every pointermove, flush to localStorage once on pointerup.
   */
  import { onDestroy } from 'svelte';
  import {
    PLAN_SIDEBAR_MIN_WIDTH,
    getPlanSidebarMaxWidth,
  } from '../../stores/planSidebarLayout.svelte';

  interface Props {
    width: number;
    onResizeLive: (width: number) => void;
    onResizeEnd: () => void;
  }

  let { width, onResizeLive, onResizeEnd }: Props = $props();

  let dragging = $state(false);
  let startPointer = 0;
  let startWidth = 0;
  let maxWidth = Number.POSITIVE_INFINITY;

  function clamp(value: number): number {
    return Math.max(PLAN_SIDEBAR_MIN_WIDTH, Math.min(maxWidth, value));
  }

  function restoreBodyStyles(): void {
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  }

  function onPointerDown(e: PointerEvent): void {
    e.preventDefault();
    window.getSelection()?.removeAllRanges();
    dragging = true;
    startPointer = e.clientX;
    startWidth = width;
    maxWidth = getPlanSidebarMaxWidth();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
  }

  function onPointerMove(e: PointerEvent): void {
    if (!dragging) return;
    // Inverted vs the threads sidebar: handle is on the LEFT edge of a
    // RIGHT-anchored panel, so a leftward drag (negative delta) grows it.
    const next = clamp(startWidth - (e.clientX - startPointer));
    if (next !== width) onResizeLive(next);
  }

  function endDrag(e: PointerEvent): void {
    if (!dragging) return;
    dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    restoreBodyStyles();
    onResizeEnd();
  }

  onDestroy(() => {
    if (dragging) restoreBodyStyles();
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="vertical"
  aria-label="Resize Plan Sidebar"
  aria-valuenow={width}
  aria-valuemin={PLAN_SIDEBAR_MIN_WIDTH}
  class={[
    'absolute top-0 bottom-0 left-0 w-1 cursor-col-resize z-20',
    'select-none touch-none',
    'hover:bg-accent/30 transition-colors',
    dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={onPointerDown}
  onpointermove={onPointerMove}
  onpointerup={endDrag}
  onpointercancel={endDrag}
  data-testid="plan-sidebar-resizer"
></div>
