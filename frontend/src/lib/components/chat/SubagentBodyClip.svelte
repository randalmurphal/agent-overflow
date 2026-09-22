<script lang="ts" generics="T">
  import { tick, untrack, type Snippet } from 'svelte';
  import { createRowEstimate } from '../../utils/virtual/priors';
  import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import { createContentGeometryNotifier } from '../../utils/scroll/contentGeometryNotifier';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import TimelineVirtualizer from '../virtual/TimelineVirtualizer.svelte';
  import OverlayScrollbar from '../shared/OverlayScrollbar.svelte';
  import type { PaneScrollController } from '../../stores/threadPaneShared';
  import { createClipWindowController } from './clipWindowController';

  let {
    nodes,
    getKey,
    depth,
    live,
    maxHeight = 'min(50vh, 20rem)',
    renderNode,
    windowOwner,
  }: {
    nodes: T[];
    getKey: (node: T) => string;
    depth: number;
    live: boolean;
    /** CSS max-height of the clip's viewport. */
    maxHeight?: string;
    renderNode: Snippet<[T, number]>;
    windowOwner?: {
      attach(controller: PaneScrollController): void;
      detach(controller: PaneScrollController): void;
      itemIds(node: T): Iterable<string>;
    };
  } = $props();

  const IS_HAPPY_DOM =
    import.meta.env.MODE === 'test' && typeof window !== 'undefined' && 'happyDOM' in window;
  const estimate = createRowEstimate({ defaultSize: 36 });
  const scrollbarGeometry = createContentGeometryNotifier();
  const stick = createUseStickToBottomController({
    liveContentActive: () => live,
    externalContentGeometry: true,
    onContentGeometryProcessed: scrollbarGeometry.notify,
    onScrollTopWritten: (top) => listRef?.noteScrollTopWritten(top),
  });

  let scrollEl = $state<HTMLElement | undefined>();
  let contentEl = $state<HTMLDivElement | undefined>();
  let listRef = $state<TimelineVirtualizerHandle | undefined>();
  let fadedTop = $state(false);

  $effect(() => {
    const owner = windowOwner;
    const list = listRef;
    if (!owner || !list) return;
    const controller = createClipWindowController({ stick, list, nodes: () => nodes, itemIds: owner.itemIds });
    untrack(() => owner.attach(controller));
    return () => owner.detach(controller);
  });

  $effect(() => {
    const scroll = scrollEl;
    const content = contentEl;
    const list = listRef;
    if (!scroll || !content || !list) return;
    // Geometry is a subscription taken AFTER attach — never a
    // fire-and-forget wire — so a sample the virtualizer published
    // before this effect ran replays into an attached controller
    // instead of being dropped and deduped away (the 2026-08-29
    // populated-first-mount class; deliverContentGeometry throws on a
    // detached delivery in dev).
    const unsubscribe = untrack(() => {
      stick.attach(scroll, content);
      // A newly expanded digest starts at its newest activity. Claiming the
      // bottom before rows finish measuring also makes later live growth use
      // the same spring as the main timeline.
      stick.forceStick();
      return list.subscribeContentGeometry(stick.deliverContentGeometry);
    });
    void tick().then(() => {
      if (scrollEl === scroll) stick.observe('content');
    });
    return () => {
      unsubscribe();
      stick.detach();
    };
  });

  function handleScroll(offset: number): void {
    fadedTop = offset > 1;
  }
</script>

<div class="relative" data-testid="subagent-group-scroll-host">
  <div
    bind:this={scrollEl}
    class="activity-run-clip pane-scroll-surface overflow-y-auto overflow-x-hidden [overflow-anchor:none]"
    use:nestedScroll
    data-testid="subagent-group-scroll"
    data-scroll-owner="controller"
  >
    <TimelineVirtualizer
      bind:this={listRef}
      bind:renderPlane={contentEl}
      scrollRef={scrollEl}
      intrinsicViewportMaxHeight={maxHeight}
      data={nodes}
      {getKey}
      {estimate}
      bufferSize={480}
      renderAll={IS_HAPPY_DOM}
      onscroll={handleScroll}
      onCompensation={stick.applyEngineCompensation}
      applyScrollTarget={stick.applyScrollTarget}
      trackReadingAnchor={() => !stick.isAtBottom || stick.escapedFromLock}
    >
      {#snippet children(node)}
        {@render renderNode(node, depth)}
      {/snippet}
    </TimelineVirtualizer>
  </div>
  <div
    aria-hidden="true"
    class="scroll-top-fade right-0"
    class:opacity-0={!fadedTop}
    style:--scroll-top-fade-depth="24px"
    data-testid="subagent-group-top-fade"
  ></div>
  <OverlayScrollbar
    target={scrollEl}
    contentGeometry={scrollbarGeometry}
    ariaLabel="Scroll agent activity"
    ownerDrivenPosition={() => stick.positionOwnerDriven}
    onUserScrollStart={() => stick.markEscaped()}
    onUserScrollEnd={(atBottom) => {
      if (atBottom) stick.markAtBottom();
    }}
  />
</div>
