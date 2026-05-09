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

  let dragging = $state(false);
  let startPointer = 0;
  let startWidth = 0;
  let maxWidth = Number.POSITIVE_INFINITY;
  let releasePause: (() => void) | null = null;

  function clamp(value: number): number {
    return Math.max(SIDEBAR_MIN_WIDTH, Math.min(maxWidth, value));
  }

  function restoreBodyStyles(): void {
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  }

  function onPointerDown(e: PointerEvent): void {
    // preventDefault on pointerdown cancels the associated mousedown's
    // default action — which, for mouse input, is what kicks off a
    // browser text selection anchored on the handle. Without this,
    // body.style.userSelect='none' (set below) only prevents NEW
    // selections; an in-progress one that started on the mousedown
    // would continue to extend as the pointer swept left/right into
    // the sidebar or the chat pane.
    e.preventDefault();
    // Clear any stray selection (e.g. user had text highlighted before
    // grabbing the handle) so the drag starts from a clean slate.
    window.getSelection()?.removeAllRanges();
    dragging = true;
    startPointer = e.clientX;
    startWidth = width;
    // Freeze the maximum at drag-start so a viewport resize mid-drag
    // doesn't yank the handle.
    maxWidth = getSidebarMaxWidth();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    // Defense-in-depth: if preventDefault doesn't catch every browser
    // edge case (Cmd-click, PenInput, etc) this still blocks selection
    // for the rest of the drag.
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    // Suspend auto-follow on the active pane's timeline so a streaming
    // chunk arriving mid-drag does not call scrollToIndex and yank the
    // user. Released in endDrag (and as a safety net in onDestroy).
    releasePause = pane?.scrollController?.pauseAutoScroll() ?? null;
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
    releasePause?.();
    releasePause = null;
    onResizeEnd();
  }

  // If the resizer is torn down mid-drag (window close, sidebar swap,
  // HMR in dev) the body would otherwise stay stuck on col-resize +
  // userSelect:none, AND the timeline's pause-lease would never release.
  // Restore defensively.
  onDestroy(() => {
    if (dragging) restoreBodyStyles();
    releasePause?.();
    releasePause = null;
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
    dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={onPointerDown}
  onpointermove={onPointerMove}
  onpointerup={endDrag}
  onpointercancel={endDrag}
  data-testid="sidebar-resizer"
></div>
