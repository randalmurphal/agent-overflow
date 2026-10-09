<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import type { Snippet } from 'svelte';
  import {
    REVIEW_SECTION_MAX_HEIGHT,
    REVIEW_SECTION_MIN_HEIGHT,
    persistReviewSectionHeights,
    reviewSectionHeight,
    setReviewSectionHeightLive,
    type ReviewSectionId,
  } from '../../stores/reviewSectionSizes.svelte';
  import { createResizeGesture } from '../../utils/resizeGesture.svelte';

  // An overview section's scrollable body plus its bottom resize handle.
  // Until the user drags, `fallbackClass` caps the height and the body
  // shrinks to content; a drag replaces the cap with a remembered pixel
  // height (stores/reviewSectionSizes), so how much of the pane the
  // diff keeps is the user's call. The overview row unmounts while the
  // reader is deep in the diff, so the body's scroll offset is handed to
  // the owner and restored on remount.

  interface Props {
    section: ReviewSectionId;
    /** Default max-height utility applied until a height is dragged. */
    fallbackClass: string;
    /** Scroll offset to restore at mount. */
    scrollTop?: number;
    onScrollTop?: (px: number) => void;
    children: Snippet;
  }

  let { section, fallbackClass, scrollTop = 0, onScrollTop, children }: Props = $props();

  let bodyEl: HTMLElement | undefined = $state();

  const height = $derived(reviewSectionHeight(section));

  const resize = createResizeGesture(() => ({
    axis: 'y',
    cursor: 'row-resize',
    // The RENDERED height, not the stored one: with short content the
    // body ends above the stored cap, and a drag must track from where
    // the handle visually is.
    currentSize: bodyEl?.clientHeight ?? REVIEW_SECTION_MIN_HEIGHT,
    minSize: REVIEW_SECTION_MIN_HEIGHT,
    maxSize: REVIEW_SECTION_MAX_HEIGHT,
    direction: 1,
    onResizeLive: (px) => setReviewSectionHeightLive(section, px),
    onResizeEnd: () => persistReviewSectionHeights(),
  }));

  // Restore once, after the children have laid out (a markdown body
  // reports its height a flush after mount). Initial-value read by
  // design: later offsets come from the reader's own scrolling.
  // svelte-ignore state_referenced_locally
  const initialScrollTop = scrollTop;
  $effect(() => {
    const el = bodyEl;
    if (!el || initialScrollTop <= 0) return;
    void tick().then(() => {
      if (el.isConnected) el.scrollTop = initialScrollTop;
    });
  });

  // Torn down mid-drag (section collapse, pane close, HMR), the body
  // would stay stuck on row-resize + userSelect:none. Restore.
  onDestroy(() => {
    resize.destroy();
  });
</script>

<div
  bind:this={bodyEl}
  class="overflow-y-auto {height === null ? fallbackClass : ''}"
  style:max-height={height !== null ? `${height}px` : undefined}
  onscroll={() => { if (bodyEl) onScrollTop?.(bodyEl.scrollTop); }}
>
  {@render children()}
</div>
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="horizontal"
  aria-label="Resize section"
  class={[
    'group/grip flex h-2 w-full cursor-row-resize select-none touch-none items-center justify-center',
    'border-t border-border-subtle',
    'hover:bg-accent/20 transition-colors',
    resize.dragging ? 'bg-accent/30' : '',
  ].join(' ')}
  data-testid="review-section-resizer"
  onpointerdown={resize.onPointerDown}
  onpointermove={resize.onPointerMove}
  onpointerup={resize.endDrag}
  onpointercancel={resize.endDrag}
>
  <span class="h-0.5 w-8 rounded-full bg-border-strong/70 group-hover/grip:bg-accent" aria-hidden="true"></span>
</div>
