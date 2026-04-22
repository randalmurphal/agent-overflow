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
    return Math.max(SIDEBAR_MIN_WIDTH, Math.min(maxWidth, value));
  }

  function restoreBodyStyles(): void {
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  }

  function onPointerDown(e: PointerEvent): void {
    dragging = true;
    startPointer = e.clientX;
    startWidth = width;
    // Freeze the maximum at drag-start so a viewport resize mid-drag
    // doesn't yank the handle.
    maxWidth = getSidebarMaxWidth();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    // Disable text selection across the whole document while dragging
    // — otherwise the cursor sweep selects random sidebar text.
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
  }

  function onPointerMove(e: PointerEvent): void {
    if (!dragging) return;
    const next = clamp(startWidth + (e.clientX - startPointer));
    if (next !== width) onResizeLive(next);
  }

  function endDrag(e: PointerEvent): void {
    if (!dragging) return;
    dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    restoreBodyStyles();
    onResizeEnd();
  }

  // If the resizer is torn down mid-drag (window close, sidebar swap,
  // HMR in dev) the body would otherwise stay stuck on col-resize +
  // userSelect:none. Restore defensively.
  onDestroy(() => {
    if (dragging) restoreBodyStyles();
  });
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="vertical"
  aria-label="Resize sidebar"
  aria-valuenow={width}
  aria-valuemin={SIDEBAR_MIN_WIDTH}
  class={[
    'absolute top-0 bottom-0 right-0 w-1 cursor-col-resize z-20',
    'hover:bg-accent/30 transition-colors',
    dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={onPointerDown}
  onpointermove={onPointerMove}
  onpointerup={endDrag}
  onpointercancel={endDrag}
  data-testid="sidebar-resizer"
></div>
