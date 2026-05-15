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
    DESIGN_CHAT_MIN_PX,
    DESIGN_PREVIEW_MIN_PX,
    persistChatPx,
    setChatPx,
  } from '../../stores/designLayout.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { createResizeGesture } from '../../utils/resizeGesture.svelte';

  interface Props {
    /** Current chat-pane width in pixels (already clamped). */
    width: number;
    /** Pixel width of the surrounding split container. */
    containerWidth: number;
    /** Pane whose timeline should suspend auto-follow during the drag. */
    pane: ThreadPane;
  }

  let { width, containerWidth, pane }: Props = $props();

  const RESIZER_PX = 4;
  const resize = createResizeGesture(() => ({
    axis: 'x',
    cursor: 'col-resize',
    currentSize: width,
    minSize: DESIGN_CHAT_MIN_PX,
    // Freeze the container width at drag-start: a window resize mid-drag
    // shouldn't yank the handle into the wrong half of the new viewport.
    maxSize: Math.max(DESIGN_CHAT_MIN_PX, containerWidth - DESIGN_PREVIEW_MIN_PX - RESIZER_PX),
    direction: 1,
    onResizeLive: (next) => setChatPx(next, pane.paneId),
    onResizeEnd: () => persistChatPx(pane.paneId),
    acquireLease: () => pane.scrollController?.pauseAutoScroll() ?? null,
  }));

  onDestroy(() => {
    resize.destroy();
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
    resize.dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  onpointerdown={resize.onPointerDown}
  onpointermove={resize.onPointerMove}
  onpointerup={resize.endDrag}
  onpointercancel={resize.endDrag}
  data-testid="design-split-resizer"
></div>
