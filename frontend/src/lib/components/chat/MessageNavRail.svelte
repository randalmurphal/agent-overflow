<script lang="ts">
  // Left-edge message navigation rail: one tick pill per user message in
  // the WHOLE thread (not just the loaded window), a fisheye hover that
  // magnifies the ticks near the pointer, a hover card previewing that
  // turn (the ask + how it resolved), ONE accent-filled "current" tick —
  // the on-screen message closest to the visible-band center — and a
  // position dot that sits in the GAP between the two ticks the reader
  // is between, shown only while NO message is on screen. Exactly one
  // position claim at a time (fill when a message is visible, dot
  // mid-gap). Clicking a tick jumps to that message instantly — the
  // jump path pages unloaded history in.
  //
  // Tick sourcing: the store baseline (GetThreadUserMessageTicks, one
  // read per thread switch) spliced under the loaded window's
  // live-derived ticks (`mergeNavTicks`), so sends, reverts, and
  // streaming reveals track with no refetch while unloaded history
  // stays on the map.
  //
  // Placement contract (C24): this component mounts OUTSIDE the scroll
  // container, as a sibling overlay in MessageTimeline's non-scrolling
  // wrapper — absolute positioning inside the scroller would render in
  // scroll-content space. Being outside the scroller is also what makes
  // its hover transitions legal (the timeline transition kill rule stops
  // at the scroller's subtree).
  //
  // Perf contract: the tick list derives from the projection's node
  // array (structural passes only, never streaming deltas); the
  // per-scroll work lives in messageNavRailSync.ts (rAF-coalesced,
  // diff-only DOM writes, no reactivity). The fisheye is transform-only
  // on fixed-size boxes — hover costs no layout, and the in-view fill
  // flips instantly (print-like) because a background-color transition
  // started from the scroll path would keep animations active exactly
  // while the scroller commits compensated moves.
  //
  // The hit strip is pointer-only (aria-hidden + tabindex -1, the
  // activity-run rail precedent); the first/last arrows are real
  // buttons and the keyboard-reachable surface.

  import { untrack } from 'svelte';
  import ChevronsUp from '@lucide/svelte/icons/chevrons-up';
  import ChevronsDown from '@lucide/svelte/icons/chevrons-down';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { TimelineNode } from '../../utils/subagentGrouping';
  import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
  import { GetThreadTurnPreview, GetThreadUserMessageTicks } from '../../stores/bindings';
  import Icon from '../primitives/Icon.svelte';
  import {
    NAV_RAIL_MIN_TICKS,
    deriveNavTicks,
    itemWindowBounds,
    mergeNavTicks,
    naturalRailHeightPx,
    previewTranslateYPercent,
    railHeightPx,
    railOverflows,
    tickDistanceScale,
    tickFraction,
    tickIndexFromPointer,
    turnPreview,
    type BaselineTick,
    type NavTickPreview,
  } from './messageNavRail';
  import { createNavRailViewportSync } from './messageNavRailSync';

  let {
    pane,
    nodes,
    getListRef,
    onJumpToItem,
  }: {
    pane: ThreadPane;
    nodes: TimelineNode[];
    getListRef(): TimelineVirtualizerHandle | undefined;
    onJumpToItem(id: string): void;
  } = $props();

  const ARROW_SIZE_PX = 24;
  const ARROW_GAP_PX = 14;
  // Vertical px the rail column reserves for the two arrow slots and
  // breathing room: 2 × (ARROW_SIZE_PX + ARROW_GAP_PX) + 20 slack.
  // A CONSTANT on purpose: deriving it from whether the arrows
  // currently render would couple available height → overflow
  // → arrows → available height into a layout feedback loop.
  const RAIL_VERTICAL_RESERVE_PX = 96;
  // The clip wrapper's vertical grace beyond the rail window: edge
  // ticks and the dot render whole instead of losing their half past
  // the window boundary. The wrapper's negative insets and the strip's
  // compensating top offset BOTH render from this constant (px, never
  // a rem utility — a root font-size change must not desync them), so
  // strip coordinates stay rail-anchored.
  const CLIP_GRACE_PX = 4;
  // The hit strip extends this far past the rail's ends — enough grace
  // to catch a near-miss, small enough (< ARROW_GAP_PX) not to overlap
  // the arrows, and bounded so a click in the wider left gutter never
  // teleports the reader to the first/last message.
  const STRIP_PAD_PX = 12;
  // Hover dwell before an unloaded tick's preview RPC fires — a fisheye
  // sweep across the rail must not fan out one request per tick.
  const REMOTE_PREVIEW_DEBOUNCE_MS = 120;
  // The gap dot's x: centered on the RESTING tick's visual span. Ticks
  // are left-3 (12px) + w-6 (24px) scaled from the left edge by the
  // resting fisheye scale, so their at-rest center is 12 + 24·rest/2.
  // Rest-state on purpose — the magnify effect is transient and the dot
  // deliberately does not track it.
  const MARKER_CENTER_X_PX = 12 + (24 * tickDistanceScale(null)) / 2;

  // ============================================================
  // Tick sourcing: store baseline + loaded-window splice
  // ============================================================

  let baseline: BaselineTick[] = $state.raw([]);

  // One read per thread switch (switchGeneration covers same-thread
  // reloads). The loaded-window splice keeps the merged list truthful
  // between reads, so no event plumbing is needed for sends/reverts.
  $effect(() => {
    const threadId = pane.threadId;
    const generation = pane.switchGeneration;
    void generation;
    baseline = [];
    remotePreviews = {};
    if (!threadId) return;
    void (async () => {
      try {
        const ticks = (await GetThreadUserMessageTicks(threadId)) as BaselineTick[];
        if (pane.threadId !== threadId) return;
        baseline = ticks ?? [];
      } catch (err) {
        // The rail degrades to loaded-window ticks; jumping and the
        // fill still work, only unloaded history is off the map.
        console.error('Failed to load nav rail ticks:', err);
      }
    })();
  });

  // The window-bounds read touches pane.items' edge rows, which the
  // streaming apply replaces per flush — deriving the bounds down to
  // PRIMITIVES puts Svelte's equality cutoff between those deltas and
  // the tick merge, so `merged` (and the O(ticks) structural reset it
  // drives) only recomputes when the window's span actually moves.
  // -1 is the "nothing loaded" sentinel; real turn indices are >= 0.
  let windowBounds = $derived(itemWindowBounds(pane.items));
  let windowFirstTurn = $derived(windowBounds?.first.turnIndex ?? -1);
  let windowFirstItem = $derived(windowBounds?.first.itemIndex ?? -1);
  let windowLastTurn = $derived(windowBounds?.last.turnIndex ?? -1);
  let windowLastItem = $derived(windowBounds?.last.itemIndex ?? -1);
  let merged = $derived.by(() =>
    mergeNavTicks(
      baseline,
      deriveNavTicks(nodes),
      windowFirstTurn < 0 ? null : { turnIndex: windowFirstTurn, itemIndex: windowFirstItem },
      windowLastTurn < 0 ? null : { turnIndex: windowLastTurn, itemIndex: windowLastItem },
      pane.hasMoreHistory,
      pane.hasMoreNewer,
    ),
  );
  let ticks = $derived(merged.ticks);
  let visible = $derived(ticks.length >= NAV_RAIL_MIN_TICKS);

  // The container's top-8 inset in px — kept in sync with the class on
  // the container div below. With the bottom inset (composer + 1rem)
  // the container spans exactly the VISIBLE band of the scroller, so
  // its extent is what "center of the visible screen" means (the raw
  // scroll viewport extends under the composer overlay).
  const TOP_INSET_PX = 32;

  let containerEl: HTMLDivElement | undefined = $state(undefined);
  let markerEl: HTMLDivElement | undefined = $state(undefined);
  let stripEl: HTMLDivElement | undefined = $state(undefined);
  let firstArrowEl: HTMLButtonElement | undefined = $state(undefined);
  let latestArrowEl: HTMLButtonElement | undefined = $state(undefined);
  let availableHeightPx = $state(0);
  // Raw container height for the visible-band center. Plain (not
  // $state) on purpose: only the sync module reads it, per frame,
  // through a getter — nothing renders from it.
  let containerHeightPx = 0;

  let naturalH = $derived(naturalRailHeightPx(ticks.length));
  let railH = $derived(railHeightPx(ticks.length, availableHeightPx));
  // Spacing never compresses: a strip taller than the column is CLIPPED
  // to a window that slides with the reader (the sync module writes the
  // translate). The arrows exist only in that state, and each shows
  // only while ITS end tick is clipped out (also the sync module's
  // per-frame call). The availableHeightPx > 0 gate stops a one-frame
  // flash before the ResizeObserver's first delivery (height 0 reads
  // as overflowing).
  let overflowing = $derived(availableHeightPx > 0 && railOverflows(ticks.length, availableHeightPx));

  // Available height for the rail comes from the container's own RO —
  // async-only, no synchronous layout seed (the C20 width-oscillation
  // lesson: never mix a sync gBCR read into an RO-fed value).
  $effect(() => {
    const el = containerEl;
    if (!el) {
      availableHeightPx = 0;
      containerHeightPx = 0;
      return;
    }
    const ro = new ResizeObserver((entries) => {
      const h = entries[entries.length - 1]?.contentRect.height ?? 0;
      containerHeightPx = h;
      availableHeightPx = Math.max(0, h - RAIL_VERTICAL_RESERVE_PX);
    });
    ro.observe(el);
    return () => ro.disconnect();
  });

  // ============================================================
  // Hover: fisheye + preview card
  // ============================================================

  let activeIndex: number | null = $state(null);
  // Clamp against the live tick list — a structural pass can shrink it
  // mid-hover, and resetting state on data change is jumpier than
  // resolving defensively.
  let resolvedActive = $derived(
    activeIndex === null || ticks.length === 0
      ? null
      : Math.min(activeIndex, ticks.length - 1),
  );

  // Previews for unloaded ticks, fetched on hover dwell and cached for
  // the thread's lifetime in this pane (reset with the baseline).
  let remotePreviews: Record<string, NavTickPreview> = $state.raw({});
  const remotePreviewInFlight = new Set<string>();
  let remotePreviewTimer: ReturnType<typeof setTimeout> | undefined;

  function fetchRemotePreview(threadId: string, id: string): void {
    if (remotePreviewInFlight.has(id)) return;
    remotePreviewInFlight.add(id);
    void (async () => {
      try {
        const p = (await GetThreadTurnPreview(threadId, id)) as NavTickPreview;
        if (pane.threadId !== threadId) return;
        remotePreviews = { ...remotePreviews, [id]: p };
      } catch (err) {
        console.error('Failed to load nav rail preview:', err);
      } finally {
        remotePreviewInFlight.delete(id);
      }
    })();
  }

  // Hover-dwell trigger for unloaded ticks. An $effect (not part of the
  // preview derivation) because it starts an RPC.
  $effect(() => {
    const idx = resolvedActive;
    if (remotePreviewTimer !== undefined) {
      clearTimeout(remotePreviewTimer);
      remotePreviewTimer = undefined;
    }
    if (idx === null) return;
    const tick = ticks[idx];
    if (!tick || tick.nodeIndex !== null) return;
    if (remotePreviews[tick.id]) return;
    const threadId = pane.threadId;
    if (!threadId) return;
    remotePreviewTimer = setTimeout(() => {
      remotePreviewTimer = undefined;
      fetchRemotePreview(threadId, tick.id);
    }, REMOTE_PREVIEW_DEBOUNCE_MS);
    return () => {
      if (remotePreviewTimer !== undefined) {
        clearTimeout(remotePreviewTimer);
        remotePreviewTimer = undefined;
      }
    };
  });

  // Guard BEFORE touching pane.items so an idle rail tracks only the
  // hover index, not every streaming upsert batch. A loaded tick derives
  // locally (tracks live edits); an unloaded one reads the RPC cache.
  let preview = $derived.by(() => {
    if (resolvedActive === null) return null;
    const tick = ticks[resolvedActive];
    if (!tick) return null;
    if (tick.nodeIndex !== null) return turnPreview(pane.items, tick.id);
    return remotePreviews[tick.id] ?? null;
  });

  function tickScale(index: number): number {
    return tickDistanceScale(resolvedActive === null ? null : Math.abs(index - resolvedActive));
  }

  // The hit strip spans exactly the rail window plus STRIP_PAD_PX each
  // end, so the event's own offsetY maps onto the tick strip with plain
  // arithmetic — no getBoundingClientRect on the pointer-move path
  // (forced layout at pointer frequency, precisely while streaming
  // keeps layout dirty). The sync module's clip offset converts window
  // y into strip y; 0 whenever the strip fits.
  function indexFromStripOffsetY(offsetY: number): number {
    if (ticks.length === 0) return -1;
    return tickIndexFromPointer(
      offsetY - STRIP_PAD_PX + sync.getClipOffsetPx(),
      naturalH,
      ticks.length,
    );
  }

  function handleStripMove(event: MouseEvent): void {
    const idx = indexFromStripOffsetY(event.offsetY);
    activeIndex = idx >= 0 ? idx : null;
  }

  function handleHoverLeave(event: MouseEvent): void {
    // Moving between the strip and the preview card keeps the hover; the
    // card adjoins the strip's right edge so the pointer never crosses
    // dead space.
    const next = event.relatedTarget;
    if (next instanceof Node && containerEl?.contains(next)) return;
    activeIndex = null;
  }

  function handleStripClick(event: MouseEvent): void {
    // Re-resolve from the click's own position — the hover index can be
    // stale if the pointer moved during the press.
    const idx = indexFromStripOffsetY(event.offsetY);
    const tick = ticks[idx];
    if (!tick) return;
    onJumpToItem(tick.id);
  }

  // ============================================================
  // Viewport sync: in-view fill + position dot (imperative)
  // ============================================================

  const sync = createNavRailViewportSync({
    // Wrapped rather than passed by reference: the prop is a reactive
    // getter, and capturing its init-time value would pin the first
    // listRef forever (svelte/state_referenced_locally).
    getListRef: () => getListRef(),
    getTicks: () => merged,
    getMarkerEl: () => markerEl,
    getStripEl: () => stripEl,
    getFirstArrowEl: () => firstArrowEl,
    getLatestArrowEl: () => latestArrowEl,
    getAvailableHeightPx: () => availableHeightPx,
    // 0 before the RO's first delivery; the sync module falls back to
    // the raw viewport center then.
    getVisibleCenterY: () => (containerHeightPx > 0 ? TOP_INSET_PX + containerHeightPx / 2 : 0),
    isEnabled: () => visible,
    // The strip slid under the pointer (a scroll the strip's own wheel
    // handler never saw — bottom-follow streaming, keyboard scroll, a
    // landing jump): drop the hover rather than let the preview lie
    // about which tick it points at. Guarded so the rAF path writes
    // state only when a hover actually exists.
    onClipChange: () => {
      if (activeIndex !== null) activeIndex = null;
    },
  });

  /**
   * Public entry: MessageTimeline calls this from the virtualizer's
   * scroll callback, scroll-end, and content-geometry deliveries. rAF
   * coalesced so a burst costs one recompute per frame.
   */
  export function scheduleViewportSync(): void {
    sync.schedule();
  }

  // A structural pass replaces the tick list; the applied fill is stale
  // by construction (the keyed list reuses surviving elements, so it
  // must be cleared by hand) — reset and resync. untrack because
  // schedule() reads ctx getters (isEnabled → `visible`): these effects
  // must re-run on exactly their named dependency, not on whatever a
  // getter happens to touch.
  $effect(() => {
    ticks;
    untrack(() => sync.reset());
  });

  // A column resize changes the window's clip math without a scroll
  // event to schedule the sync — re-sync on the RO-fed height.
  $effect(() => {
    availableHeightPx;
    untrack(() => sync.schedule());
  });

  $effect(() => () => sync.cancel());

  let railTop = $derived(`calc(50% - ${railH / 2}px)`);

  // Preview anchor: the hovered tick's y within the rail WINDOW (strip
  // y minus the slide). getClipOffsetPx is a plain read of the last
  // synced value, recomputed per hover-index change — the cadence the
  // card moves at anyway (wheel over the strip drops the hover).
  let previewAnchor = $derived.by(() => {
    if (resolvedActive === null) return null;
    const y = tickFraction(resolvedActive, ticks.length) * naturalH - sync.getClipOffsetPx();
    return { y, translatePercent: previewTranslateYPercent(railH > 0 ? y / railH : 0.5) };
  });

  const ARROW_CLASSES = [
    'pointer-events-auto absolute left-1.5 z-30',
    'inline-flex items-center justify-center',
    'rounded-full border border-border-subtle bg-card text-text-secondary',
    'shadow-sheet transition-[background-color,color] duration-150 motion-reduce:transition-none',
    'hover:bg-surface-2/80 hover:text-text-primary',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    'cursor-pointer',
  ].join(' ');
</script>

{#if visible}
  <!-- Overlay column in the row padding at the pane's left edge. Insets
       clear the top fade and the composer overlay (same lift math as
       ScrollToBottomButton). pointer-events-none at the container;
       the strip, arrows, and preview card opt back in. -->
  <div
    bind:this={containerEl}
    class="pointer-events-none absolute left-0 top-8 z-30 w-10"
    style:bottom={`calc(var(--composer-height, 0px) + 1rem)`}
    data-testid="message-nav-rail"
  >
    <!-- Hit strip: one pointer-only surface for hover + click, spanning
         the rail plus a small grace pad — never the whole column, so
         gutter clicks and selection drags outside the rail stay inert.
         Pointer-only deliberately (aria-hidden + tabindex -1 together,
         the activity-run rail precedent): the arrows are the accessible
         controls, and a focus ring on an invisible strip helps nobody. -->
    <button
      type="button"
      class="pointer-events-auto absolute left-0 w-full cursor-pointer bg-transparent"
      style:top={`calc(${railTop} - ${STRIP_PAD_PX}px)`}
      style:height={`${railH + STRIP_PAD_PX * 2}px`}
      tabindex="-1"
      aria-hidden="true"
      data-testid="nav-rail-strip"
      onmousemove={handleStripMove}
      onmouseleave={handleHoverLeave}
      onclick={handleStripClick}
      onmousedown={(e) => e.preventDefault()}
      onwheel={() => (activeIndex = null)}
    ></button>

    {#if overflowing}
      <!-- Jump arrows exist only while the strip is clipped, and each is
           born hidden: the sync module reveals one only while ITS end
           tick is clipped out of the window (at the strip's end the
           tick itself is visible and the arrow yields). -->
      <button
        bind:this={firstArrowEl}
        type="button"
        class={ARROW_CLASSES}
        style:width={`${ARROW_SIZE_PX}px`}
        style:height={`${ARROW_SIZE_PX}px`}
        style:top={`calc(${railTop} - ${ARROW_GAP_PX + ARROW_SIZE_PX}px)`}
        style:visibility="hidden"
        aria-label="Jump to first message"
        title="Jump to first message"
        data-testid="nav-rail-jump-first"
        onclick={() => onJumpToItem(ticks[0].id)}
      >
        <Icon icon={ChevronsUp} size={13} strokeWidth={2.5} />
      </button>
      <button
        bind:this={latestArrowEl}
        type="button"
        class={ARROW_CLASSES}
        style:width={`${ARROW_SIZE_PX}px`}
        style:height={`${ARROW_SIZE_PX}px`}
        style:top={`calc(${railTop} + ${railH + ARROW_GAP_PX}px)`}
        style:visibility="hidden"
        aria-label="Jump to latest message"
        title="Jump to latest message"
        data-testid="nav-rail-jump-latest"
        onclick={() => onJumpToItem(ticks[ticks.length - 1].id)}
      >
        <Icon icon={ChevronsDown} size={13} strokeWidth={2.5} />
      </button>
    {/if}

    <!-- The rail proper: a vertically centered WINDOW onto the tick
         strip. The strip lays every tick out at natural spacing; when
         it outgrows the window the clip wrapper cuts it and the sync
         module slides it (translateY) with the reader's position. The
         wrapper's CLIP_GRACE_PX insets let edge ticks render whole;
         the strip's matching static top offset keeps strip y
         rail-anchored. Ticks are decorative — the hit strip
         owns the events. The tick transition is transform-ONLY: the
         current fill flips from the scroll path and must not start
         animations there. -->
    <div
      class="absolute left-0 w-full"
      style:top={railTop}
      style:height={`${railH}px`}
      aria-hidden="true"
    >
      <div
        class="absolute inset-x-0 overflow-hidden"
        style:top={`${-CLIP_GRACE_PX}px`}
        style:bottom={`${-CLIP_GRACE_PX}px`}
      >
        <div
          bind:this={stripEl}
          class="absolute inset-x-0"
          style:top={`${CLIP_GRACE_PX}px`}
          style:height={`${naturalH}px`}
          data-testid="nav-rail-strip-track"
        >
          {#each ticks as tick, i (tick.id)}
            <div
              use:sync.registerTick={i}
              data-current="false"
              class="absolute left-3 h-0.5 w-6 origin-left rounded-full bg-border-strong transition-transform duration-150 motion-reduce:transition-none data-[current=true]:bg-accent"
              style:top={`${tickFraction(i, ticks.length) * 100}%`}
              style:transform={`translateY(-50%) scaleX(${tickScale(i)})`}
            ></div>
          {/each}

          {#if ticks.length > 0}
            <!-- Position dot: sits IN the tick column, x-centered on the
                 resting tick lines, centered in the gap between the two
                 ticks the reader is between, and hops to the next gap on
                 reaching the next message. Shows ONLY while no user message
                 is on screen — whenever one is, the current tick's fill is
                 the position claim and the dot yields (the sync module owns
                 that exclusivity). Smaller and dimmer than the fill on
                 purpose: it is the between-messages state, not the primary
                 marker. style.top and visibility are written imperatively
                 by the sync module; born hidden so it never paints before
                 the first sync. -->
            <div
              bind:this={markerEl}
              class="absolute h-[3px] w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-accent/75"
              style:left={`${MARKER_CENTER_X_PX}px`}
              style:top="0%"
              style:visibility="hidden"
              data-testid="nav-rail-marker"
            ></div>
          {/if}
        </div>
      </div>
    </div>

    {#if previewAnchor !== null && preview && preview.userText}
      <!-- Turn preview. Anchored to the hovered tick's position within
           the rail window; cards near the window's edges flip instead
           of clipping. Selectable on purpose — mouseleave keeps the
           hover while the pointer is anywhere in the container, so text
           can be selected without the card vanishing. -->
      <div
        role="tooltip"
        class="pointer-events-auto absolute left-10 z-40 w-80 max-w-[min(20rem,60vw)] cursor-text select-text rounded-lg border border-border-subtle bg-card p-3 shadow-sheet"
        style:top={`calc(${railTop} + ${previewAnchor.y}px)`}
        style:transform={`translateY(${previewAnchor.translatePercent}%)`}
        onmouseleave={handleHoverLeave}
        data-testid="nav-rail-preview"
      >
        <p class="line-clamp-2 text-xs font-medium text-text-primary">{preview.userText}</p>
        {#if preview.assistantText}
          <p class="mt-1.5 line-clamp-3 text-xs text-text-secondary">{preview.assistantText}</p>
        {/if}
      </div>
    {/if}
  </div>
{/if}
