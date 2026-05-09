<script lang="ts">
  // Horizontal resize handle for the chat ↔ preview split in design
  // threads. Mirrors SidebarResizer's pointer-capture pattern. The
  // chat-pane width (in pixels) is the source of truth; we set it on the
  // wrapping flex container as a `width: Npx` style and let the preview
  // pane fill the remainder via flex-1.
  //
  // While dragging we acquire a pauseAutoScroll lease on the chat
  // timeline so a streaming chunk arriving mid-drag doesn't fire the
  // controller's content-RO sync-pin and yank the user. Released on
  // pointerup or component teardown.

  import { onDestroy } from 'svelte';
  import {
    clampChatWidth,
    persistChatPx,
    setChatPx,
  } from '../../stores/designLayout.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';

  interface Props {
    /** Current chat-pane width in pixels (already clamped). */
    width: number;
    /** Pixel width of the surrounding split container. */
    containerWidth: number;
    /** Pane whose timeline should suspend auto-follow during the drag. */
    pane: ThreadPane;
  }

  let { width, containerWidth, pane }: Props = $props();

  let dragging = $state(false);
  let startPointer = 0;
  let startWidth = 0;
  let lockedContainer = 0;
  let releasePause: (() => void) | null = null;

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
    // Freeze the container width at drag-start: a window resize mid-drag
    // shouldn't yank the handle into the wrong half of the new viewport.
    lockedContainer = containerWidth;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    document.body.style.cursor = 'col-resize';
    document.body.style.userSelect = 'none';
    releasePause = pane.scrollController?.pauseAutoScroll() ?? null;
  }

  function onPointerMove(e: PointerEvent): void {
    if (!dragging) return;
    const next = clampChatWidth(
      startWidth + (e.clientX - startPointer),
      lockedContainer,
    );
    if (next !== width) setChatPx(next);
  }

  function endDrag(e: PointerEvent): void {
    if (!dragging) return;
    dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
    restoreBodyStyles();
    releasePause?.();
    releasePause = null;
    persistChatPx();
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
  aria-label="Resize Design Split"
  aria-valuenow={width}
  class={[
    'shrink-0 w-1 cursor-col-resize select-none touch-none',
    'bg-border-subtle/40 hover:bg-accent/30 transition-colors',
    dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={onPointerDown}
  onpointermove={onPointerMove}
  onpointerup={endDrag}
  onpointercancel={endDrag}
  data-testid="design-split-resizer"
></div>
