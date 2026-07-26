<script lang="ts">
  // A scrollbar that consumes zero layout width, in every state.
  //
  // Absolutely positioned, so it cannot shift content when the surface
  // starts or stops overflowing — the reason it exists (see
  // utils/scroll/overlayScrollbar.ts for why the native bar can't do this
  // here). The track is `pointer-events: none`; only the thumb is
  // interactive, so the live region is a thin strip in padding that holds
  // no text.
  //
  // Intent is stated, not inferred. A zero-width native bar makes
  // `offsetWidth - clientWidth === 0`, so the scroll package's geometric
  // scrollbar-gutter hit test can never fire for this surface. That is the
  // correct outcome — there is no bar there to hit — but it means a drag
  // has to announce itself through `onDragStart` / `onDragEnd`, which
  // matches the package's own rule that intent is event-sourced, never
  // geometry-inferred.

  import {
    readScrollMetrics,
    scrollTopForDrag,
    scrollTopForTrackClick,
    thumbMetrics,
    type DragOrigin,
    type ScrollMetrics,
  } from '../../utils/scroll/overlayScrollbar';

  let {
    target,
    content,
    ariaLabel,
    onDragStart,
    onDragEnd,
  }: {
    /** The scrolling element this bar drives. */
    target: HTMLElement | undefined;
    /**
     * The element inside `target` whose growth changes `scrollHeight`.
     * Observed separately because a scroller can keep its own size while
     * its content grows — which is exactly the streaming case.
     */
    content?: HTMLElement | undefined;
    ariaLabel: string;
    /** Fired on grab / release so the owner can state scroll intent. */
    onDragStart?: () => void;
    onDragEnd?: (atBottom: boolean) => void;
  } = $props();

  const IDLE_HIDE_MS = 900;
  const EMPTY_METRICS: ScrollMetrics = { scrollTop: 0, scrollHeight: 0, clientHeight: 0 };

  let trackEl = $state<HTMLElement | undefined>();
  let metrics = $state<ScrollMetrics>(EMPTY_METRICS);
  let trackPx = $state(0);
  let dragging = $state(false);
  let recentlyActive = $state(false);

  let thumb = $derived(thumbMetrics(metrics, trackPx));

  let idleHandle: ReturnType<typeof setTimeout> | undefined;

  function markActive(): void {
    recentlyActive = true;
    if (idleHandle !== undefined) clearTimeout(idleHandle);
    idleHandle = setTimeout(() => {
      recentlyActive = false;
      idleHandle = undefined;
    }, IDLE_HIDE_MS);
  }

  function sample(): void {
    if (target) metrics = readScrollMetrics(target);
    if (trackEl) trackPx = trackEl.clientHeight;
  }

  $effect(() => {
    const el = target;
    if (!el) {
      metrics = EMPTY_METRICS;
      return;
    }
    function onScroll(): void {
      sample();
      markActive();
    }
    el.addEventListener('scroll', onScroll, { passive: true });
    // Both edges move the thumb: the scroller resizing (cap inflation,
    // window resize) and the content growing inside it (streaming, a
    // mounted chunk). Neither implies the other.
    const sizes = new ResizeObserver(sample);
    sizes.observe(el);
    if (content) sizes.observe(content);
    if (trackEl) sizes.observe(trackEl);
    sample();
    return () => {
      el.removeEventListener('scroll', onScroll);
      sizes.disconnect();
    };
  });

  $effect(() => () => {
    if (idleHandle !== undefined) clearTimeout(idleHandle);
  });

  function atBottom(): boolean {
    return metrics.scrollHeight - metrics.scrollTop - metrics.clientHeight <= 1;
  }

  let scrolledPercent = $derived.by(() => {
    const scrollable = metrics.scrollHeight - metrics.clientHeight;
    if (scrollable <= 0) return 0;
    return Math.round((metrics.scrollTop / scrollable) * 100);
  });

  let origin: DragOrigin | null = null;

  function offsetWithinTrack(clientY: number): number {
    return trackEl ? clientY - trackEl.getBoundingClientRect().top : 0;
  }

  // One handler for the whole control: a press on the thumb is a drag, a
  // press anywhere else on the track pages toward it. Splitting them
  // across two elements would need the inner one to carry its own ARIA
  // role, and the scrollbar is one widget, not two.
  function onPointerDown(event: PointerEvent): void {
    if (!target || !trackEl || event.button !== 0) return;
    sample();
    if (!thumb.visible) return;
    const offsetY = offsetWithinTrack(event.clientY);
    const onThumb = offsetY >= thumb.topPx && offsetY <= thumb.topPx + thumb.heightPx;
    markActive();

    if (!onThumb) {
      target.scrollTop = scrollTopForTrackClick(offsetY, metrics, trackPx);
      sample();
      return;
    }

    event.preventDefault();
    origin = { scrollTop: metrics.scrollTop, pointerY: event.clientY };
    dragging = true;
    // Capture keeps the drag alive once the pointer leaves the 6px strip,
    // which it will immediately — nobody tracks a thin bar precisely.
    trackEl.setPointerCapture(event.pointerId);
    onDragStart?.();
  }

  function onPointerMove(event: PointerEvent): void {
    if (!dragging || !origin || !target) return;
    target.scrollTop = scrollTopForDrag(origin, event.clientY, metrics, trackPx);
    sample();
    markActive();
  }

  function endDrag(event: PointerEvent): void {
    if (!dragging) return;
    dragging = false;
    origin = null;
    trackEl?.releasePointerCapture(event.pointerId);
    sample();
    onDragEnd?.(atBottom());
  }
</script>

<!-- Inert whenever there is nothing to scroll: an invisible 6px strip that
     still swallowed clicks would be a worse bug than the one this fixes.
     While it IS scrollable the strip sits in column padding that holds no
     content, so being interactive costs nothing — and hovering it brings
     the faded thumb back, which is what makes a drag discoverable. -->
<div
  bind:this={trackEl}
  class="absolute inset-y-0 -right-3 w-1.5 transition-opacity duration-150"
  class:pointer-events-none={!thumb.visible}
  class:opacity-0={!thumb.visible || (!recentlyActive && !dragging)}
  class:opacity-100={thumb.visible && (recentlyActive || dragging)}
  role="scrollbar"
  aria-label={ariaLabel}
  aria-orientation="vertical"
  aria-controls={target?.id}
  aria-valuemin={0}
  aria-valuemax={100}
  aria-valuenow={scrolledPercent}
  tabindex="-1"
  data-testid="overlay-scrollbar"
  data-visible={thumb.visible ? 'true' : 'false'}
  data-dragging={dragging ? 'true' : 'false'}
  onpointerdown={onPointerDown}
  onpointermove={onPointerMove}
  onpointerup={endDrag}
  onpointercancel={endDrag}
  onpointerenter={markActive}
>
  {#if thumb.visible}
    <!-- Presentational: the widget is the track, and a second focusable
         element inside a `scrollbar` role would be a second widget. -->
    <div
      class="absolute left-0 w-full rounded-full bg-border/80"
      style:top="{thumb.topPx}px"
      style:height="{thumb.heightPx}px"
      aria-hidden="true"
      data-testid="overlay-scrollbar-thumb"
    ></div>
  {/if}
</div>
