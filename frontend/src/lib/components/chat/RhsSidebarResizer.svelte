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
     * concurrent stream chunk could call `scrollToIndex(last, 'end')`
     * mid-drag and yank the user. Idempotent — no-op when the pane has
     * no registered controller.
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

  let dragging = $state(false);
  let startPointer = 0;
  let startWidth = 0;
  let maxWidth = Number.POSITIVE_INFINITY;
  let releasePause: (() => void) | null = null;

  function clamp(value: number): number {
    return Math.max(minWidth, Math.min(maxWidth, value));
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
    maxWidth = getMaxWidth();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    // Suspend auto-follow on the active pane's timeline. Released in
    // endDrag (and as a safety net in onDestroy).
    releasePause = pane?.scrollController?.pauseAutoScroll() ?? null;
  }

  function onPointerMove(e: PointerEvent): void {
    if (!dragging) return;
    // Inverted direction: handle is on the LEFT edge of a RIGHT-anchored
    // panel, so a leftward drag (negative delta) grows the panel.
    const next = clamp(startWidth - (e.clientX - startPointer));
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
  aria-label={ariaLabel}
  aria-valuenow={width}
  aria-valuemin={minWidth}
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
  data-testid={testId}
></div>
