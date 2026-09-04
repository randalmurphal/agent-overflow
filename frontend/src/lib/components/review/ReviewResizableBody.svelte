<script lang="ts">
  import { onDestroy } from 'svelte';
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

  // The PR header sections' scrollable body plus its bottom resize
  // handle. Until the user drags, `fallbackClass` caps the height and the
  // body shrinks to content; a drag replaces the cap with a remembered
  // pixel height (stores/reviewSectionSizes), so how much of the pane the
  // diff keeps is the user's call.

  interface Props {
    section: ReviewSectionId;
    /** Default max-height utility applied until a height is dragged. */
    fallbackClass: string;
    children: Snippet;
  }

  let { section, fallbackClass, children }: Props = $props();

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
>
  {@render children()}
</div>
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  role="separator"
  aria-orientation="horizontal"
  aria-label="Resize section"
  class={[
    'h-1.5 w-full cursor-row-resize select-none touch-none',
    'border-t border-border-subtle',
    'hover:bg-accent/30 transition-colors',
    resize.dragging ? 'bg-accent/50' : '',
  ].join(' ')}
  data-testid="review-section-resizer"
  onpointerdown={resize.onPointerDown}
  onpointermove={resize.onPointerMove}
  onpointerup={resize.endDrag}
  onpointercancel={resize.endDrag}
></div>
