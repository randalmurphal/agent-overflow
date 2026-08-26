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
  // has to announce itself through `onUserScrollStart` / `onUserScrollEnd`, which
  // matches the package's own rule that intent is event-sourced, never
  // geometry-inferred.

  import { canConsumeDelta } from '../../utils/scroll/wheelAttribution';
  import {
    readScrollMetrics,
    scrollTopForDrag,
    scrollTopForTrackClick,
    scrollTopForWheel,
    thumbMetrics,
    type DragOrigin,
    type ScrollMetrics,
  } from '../../utils/scroll/overlayScrollbar';

  let {
    target,
    content,
    ariaLabel,
    placement = 'inset-y-0 -right-3 w-1.5',
    ownerDrivenPosition,
    onUserScrollStart,
    onUserScrollEnd,
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
    /**
     * Positioning utilities for the track within its `relative` host. The
     * default hangs the strip in the activity-run column's padding; a
     * full-pane scroller keeps it inside its own right edge instead.
     */
    placement?: string;
    /**
     * True while the position is the OWNER's to move rather than the reader's
     * — a bottom-following controller pinning to content as it streams in.
     * Geometry still samples (the thumb has to stay honest); only the
     * activity fade is suppressed, so a surface that auto-scrolls for a whole
     * turn does not hold a permanent bar beside content nobody touched.
     * Omit for surfaces only the reader ever scrolls.
     */
    ownerDrivenPosition?: () => boolean;
    /**
     * Fired when the user takes manual control of the position and when they
     * give it back, with where it landed. Every gesture this control offers
     * reports through the pair — a thumb drag, a track-click page, a wheel
     * notch — because the owner cares that the position is now the user's,
     * not which gesture said so.
     */
    onUserScrollStart?: () => void;
    onUserScrollEnd?: (atBottom: boolean) => void;
  } = $props();

  const IDLE_HIDE_MS = 900;
  const EMPTY_METRICS: ScrollMetrics = { scrollTop: 0, scrollHeight: 0, clientHeight: 0 };

  let trackEl = $state<HTMLElement | undefined>();
  let metrics = $state<ScrollMetrics>(EMPTY_METRICS);
  let trackPx = $state(0);
  let recentlyActive = $state(false);

  // The drag in progress, or null. Its owner and its origin are ONE value
  // because a touch surface can put a second finger on the thumb while the
  // first still holds it: separate fields let that press overwrite the origin
  // and announce a second start, after which the first pointer's moves drive —
  // and its release ends — a gesture that belongs to the second, leaving it
  // captured with no end callback and the owner free to re-stick mid-gesture.
  let drag = $state<{ pointerId: number; origin: DragOrigin } | null>(null);

  let dragging = $derived(drag !== null);
  let thumb = $derived(thumbMetrics(metrics, trackPx));

  let idleHandle: ReturnType<typeof setTimeout> | undefined;

  function markActive(): void {
    // Reveal transition: geometry went stale while the bar was hidden
    // (hidden bars skip every sample — see onScroll), so the thumb must
    // be repositioned before this frame shows it.
    if (!recentlyActive && !dragging) sample();
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
      // A streaming surface pins itself to new content on every chunk, and
      // each pin is a scroll event. Fading on those would mean never fading.
      if (!ownerDrivenPosition?.()) markActive();
      // A hidden bar is inert: sampling here writes `style:top` on an
      // opacity-0 thumb on EVERY glide frame (× every mounted bar), which
      // alone kept style→paint→layerize→raster running for whole streaming
      // turns (measured 2026-08-25: the dominant renderer garbage source).
      // markActive() re-samples on the hidden→visible transition, so the
      // thumb is fresh the frame it can first be seen.
      if (recentlyActive || dragging) sample();
    }
    el.addEventListener('scroll', onScroll, { passive: true });
    // Both edges move the thumb: the scroller resizing (cap inflation,
    // window resize) and the content growing inside it (streaming, a
    // mounted chunk). Neither implies the other. Same hidden gate as
    // onScroll — content growth per streamed chunk fires this too.
    const sizes = new ResizeObserver(() => {
      if (recentlyActive || dragging) sample();
    });
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

  function offsetWithinTrack(clientY: number): number {
    return trackEl ? clientY - trackEl.getBoundingClientRect().top : 0;
  }

  // One handler for the whole control: a press on the thumb is a drag, a
  // press anywhere else on the track pages toward it. Splitting them
  // across two elements would need the inner one to carry its own ARIA
  // role, and the scrollbar is one widget, not two.
  function onPointerDown(event: PointerEvent): void {
    // A drag owns the control until it ends: a second pointer must neither
    // take it over nor page the surface out from under it.
    if (!target || !trackEl || event.button !== 0 || drag) return;
    sample();
    if (!thumb.visible) return;
    const offsetY = offsetWithinTrack(event.clientY);
    const onThumb = offsetY >= thumb.topPx && offsetY <= thumb.topPx + thumb.heightPx;
    markActive();

    if (!onThumb) {
      // A track click moves the position deliberately, so it states the same
      // intent a drag does — as a complete gesture, since a click has no
      // release to wait for. Without it a bottom-following surface never
      // learns the reader left, and its next growth pulls them back down.
      onUserScrollStart?.();
      target.scrollTop = scrollTopForTrackClick(offsetY, metrics, trackPx);
      sample();
      onUserScrollEnd?.(atBottom());
      return;
    }

    event.preventDefault();
    drag = {
      pointerId: event.pointerId,
      origin: { scrollTop: metrics.scrollTop, pointerY: event.clientY },
    };
    // Capture keeps the drag alive once the pointer leaves the 6px strip,
    // which it will immediately — nobody tracks a thin bar precisely.
    trackEl.setPointerCapture(event.pointerId);
    onUserScrollStart?.();
  }

  function onPointerMove(event: PointerEvent): void {
    if (!drag || !target || event.pointerId !== drag.pointerId) return;
    target.scrollTop = scrollTopForDrag(drag.origin, event.clientY, metrics, trackPx);
    sample();
    markActive();
  }

  // Every way a drag can end funnels through here, including the ones the
  // user did not ask for: `lostpointercapture` fires when the capture is
  // taken away (the element leaves the DOM, the browser cancels the pointer),
  // and without it the drag would outlive the gesture — holding the bar
  // visible forever, leaving an origin the next pointermove would scroll
  // against, and never telling the owner it may
  // re-stick. Idempotent: the first call clears the state the rest test, and
  // only the pointer that started the drag can end it.
  function endDrag(event: PointerEvent): void {
    if (!drag || event.pointerId !== drag.pointerId) return;
    drag = null;
    // Already released when this ran FROM `lostpointercapture`.
    if (trackEl?.hasPointerCapture(event.pointerId)) {
      trackEl.releasePointerCapture(event.pointerId);
    }
    sample();
    onUserScrollEnd?.(atBottom());
  }

  // The bar sits BESIDE the surface it drives, so a wheel over the strip never
  // reaches that surface: it bubbles to whatever scroller contains the bar —
  // for an activity run, the conversation — which scrolls instead, and its
  // intent machine reads the gesture as the reader leaving the bottom. Two
  // wrong outcomes from one notch. So the bar applies the delta itself and
  // takes the event out of the tree, stating the same intent a drag does.
  //
  // At the surface's own edge it does neither: the event chains outward and the
  // outer machine reacts normally. That is the same rule attribution applies to
  // a nested box, which is why the edge test is the same function.
  function onWheel(event: WheelEvent): void {
    if (!target || event.deltaY === 0) return;
    // Browser zoom, not a scroll.
    if (event.ctrlKey) return;
    // Metrics can be stale while the bar is hidden (hidden bars skip
    // every sample); both the visibility test and the wheel target read
    // from them.
    sample();
    if (!thumb.visible) return;
    if (drag) {
      // The drag owns the position until it ends. A wheel would move the
      // surface out from under an origin that still describes the old offset.
      event.preventDefault();
      event.stopPropagation();
      return;
    }
    if (!canConsumeDelta(target, event.deltaY)) return;
    event.preventDefault();
    event.stopPropagation();
    onUserScrollStart?.();
    target.scrollTop = scrollTopForWheel(metrics, event.deltaY, event.deltaMode);
    sample();
    markActive();
    onUserScrollEnd?.(atBottom());
  }
</script>

<!-- Inert whenever there is nothing to scroll: an invisible 6px strip that
     still swallowed clicks would be a worse bug than the one this fixes.
     While it IS scrollable the strip sits in column padding that holds no
     content, so being interactive costs nothing — and hovering it brings
     the faded thumb back, which is what makes a drag discoverable. -->
<!-- `touch-none`: a touch drag on the strip must be this control's gesture, not
     a native pan of whatever scroller is behind it. `preventDefault` on
     pointerdown cannot suppress that — only `touch-action` can. -->
<div
  bind:this={trackEl}
  class={`absolute ${placement} touch-none transition-opacity duration-150`}
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
  onlostpointercapture={endDrag}
  onpointerenter={markActive}
  onwheel={onWheel}
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
