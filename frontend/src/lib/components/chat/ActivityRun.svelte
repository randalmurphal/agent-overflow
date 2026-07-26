<script lang="ts">
  // One maximal run of consecutive activity rows (tool calls, completions,
  // thinking, and the group containers that sit on the same rail), rendered
  // as a single timeline row.
  //
  // Two states, no third. Expanded, the run is a height-capped clip that
  // scrolls in place — a run shorter than the cap renders exactly as it
  // did before this component existed, which is why no row-count threshold
  // is needed. Collapsed, it is one chip line.
  //
  // The rail belongs to the run, not to its rows: one continuous border for
  // the whole block, doubling as the collapse control. Per-row borders would
  // draw the same line but could not be clicked as one thing.
  //
  // Only the run holding the live tail gets a scroll controller. Historical
  // runs never chase, so they need no ResizeObserver, no intent listeners,
  // no warm gate, and no composited layer — a controller each would tax
  // every run in the buffer for physics only one of them can use.

  import type { Snippet } from 'svelte';
  import { tick } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
  import { timelineNodeKey } from '../../utils/subagentGrouping';
  import {
    activityRunRowIndexOfItem,
    activityRunWindowGrownNewer,
    activityRunWindowGrownOlder,
  } from '../../utils/activityRunWindow';
  import {
    activityRunCenteredScrollTop,
    activityRunChildElement,
    activityRunClipMaxHeight,
    observeActivityRunExpansion,
  } from '../../utils/activityRunClip';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import {
    createUseStickToBottomController,
    type UseStickToBottomController,
  } from '../../utils/scroll/index.svelte';
  import {
    isLiveContentActive,
    LIVE_CONTENT_ACTIVE_HOLD_MS,
  } from '../../utils/liveContentActivity';
  import OverlayScrollbar from '../shared/OverlayScrollbar.svelte';
  import ActivityRunBoundary from './ActivityRunBoundary.svelte';
  import ActivityRunChip from './ActivityRunChip.svelte';

  let {
    pane,
    run,
    depth,
    live,
    renderNode,
  }: {
    pane: ThreadPane;
    run: ActivityRunNode;
    depth: number;
    /** This run holds the timeline's tail, so new activity lands here. */
    live: boolean;
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  let clipEl = $state<HTMLElement | undefined>();
  let contentEl = $state<HTMLElement | undefined>();
  let expandedPx = $state(0);

  // Built only for the live run, and only while it is live — a run that a
  // later one displaces has no use for a spring, and a controller per run
  // in the buffer would be a spring, an observer set, and intent listeners
  // each for physics only one of them can use.
  //
  // Same factory, spring constants, and glide compositing as the main pane,
  // so a streaming run feels identical to a streaming thread. It NEVER
  // calls `pane.attachScrollController`: that slot is single-occupancy and
  // belongs to the timeline.
  //
  // `externalContentGeometry` is deliberately unset. There is no
  // virtualizer inside a run, so the controller's own contentEl
  // ResizeObserver is the right geometry source (the ChannelView
  // precedent); the timeline leaves it set because its engine reports
  // geometry the RO would otherwise have to re-derive.
  let stick = $state<UseStickToBottomController | null>(null);

  function createStick(): UseStickToBottomController {
    return createUseStickToBottomController({
      liveContentActive: () =>
        isLiveContentActive(
          performance.now(),
          pane.lastLiveContentAt,
          LIVE_CONTENT_ACTIVE_HOLD_MS,
        ),
    });
  }

  // Mount window: only `mountedRows` children starting at `mountedFrom` are
  // in the DOM. Without it a 200-row run would mount 200 rows the moment its
  // single timeline row entered the virtualizer's buffer — the DOM bound the
  // virtualizer provides at top level has to be re-established inside. The
  // window rests on the run's tail and relocates when a jump resolves into
  // the run (utils/activityRunWindow.ts).
  let mountedChildren = $derived(
    run.children.slice(run.mountedFrom, run.mountedFrom + run.mountedRows),
  );
  let hiddenEarlier = $derived(run.mountedFrom);
  let hiddenLater = $derived(
    run.children.length - run.mountedFrom - run.mountedRows,
  );

  let maxHeight = $derived(activityRunClipMaxHeight(expandedPx));
  let toggleLabel = $derived(
    run.collapsed ? 'Expand activity run' : 'Collapse activity run',
  );

  function toggle(): void {
    pane.activityRuns.toggleCollapsed(run.runId);
  }

  async function mountEarlier(): Promise<void> {
    const clip = clipEl;
    const beforeHeight = clip?.scrollHeight ?? 0;
    const beforeTop = clip?.scrollTop ?? 0;

    pane.activityRuns.setMountWindow(run.runId, activityRunWindowGrownOlder(run));
    await tick();

    // Manual prepend compensation. WebKit has no `overflow-anchor`, so
    // rows added ABOVE the viewport would otherwise push the reading
    // position down by exactly their height. Two reads and a write, after
    // the DOM has the new rows and before the user can see the frame.
    if (!clip) return;
    const grew = clip.scrollHeight - beforeHeight;
    if (grew > 0) clip.scrollTop = beforeTop + grew;
  }

  // No compensation on this edge: rows appended BELOW the reading position
  // move nothing above it.
  function mountLater(): void {
    pane.activityRuns.setMountWindow(run.runId, activityRunWindowGrownNewer(run));
  }

  // Expanded payloads lift the cap by their own height (see
  // utils/activityRunClip.ts). Reading `mountedChildren` re-targets the
  // observers when the mounted set changes: a row can remount already
  // expanded from its lease, which mutates no attribute for the observer
  // inside to see. Streaming text growth changes neither, so this stays off
  // the hot path.
  $effect(() => {
    const clip = clipEl;
    if (!clip || run.collapsed) {
      expandedPx = 0;
      return;
    }
    mountedChildren;
    return observeActivityRunExpansion(clip, (px) => {
      expandedPx = px;
    });
  });

  // Persisted per frame, not only at teardown: a thread switch clears the
  // registry synchronously with the data change, well before Svelte tears
  // this row down, so a teardown-only save would archive a position several
  // reads stale. One small write against a Map — the overlay scrollbar
  // already samples geometry on the same event.
  function saveInnerScroll(): void {
    const clip = clipEl;
    if (!clip) return;
    pane.activityRuns.saveScrollSnapshot(run.runId, {
      scrollTop: clip.scrollTop,
      escaped: stick?.escapedFromLock ?? false,
    });
  }

  // Controller lifetime and scroll-position persistence are ONE effect on
  // purpose. The saved snapshot carries the controller's escape flag, so
  // splitting them would make the saved value depend on which teardown
  // Svelte happened to run first.
  //
  // Inner position has to survive the virtualizer evicting this row:
  // without it a run the user had scrolled up inside snaps back to its
  // tail every time it leaves the buffer.
  $effect(() => {
    const clip = clipEl;
    const content = contentEl;
    if (!clip) return;

    const controller = live && content ? createStick() : null;
    stick = controller;
    if (controller && content) controller.attach(clip, content);

    const snapshot = pane.activityRuns.scrollSnapshot(run.runId);
    if (snapshot) {
      clip.scrollTop = snapshot.scrollTop;
      // Escape is event-sourced, so it is carried across the remount
      // rather than re-derived from the geometry just written.
      if (controller && snapshot.escaped) controller.setEscapedFromLock(true);
    } else {
      // A run that has never been scrolled rests at its newest row — the
      // latest activity is the reason it is on screen.
      clip.scrollTop = clip.scrollHeight;
    }

    return () => {
      pane.activityRuns.saveScrollSnapshot(run.runId, {
        scrollTop: clip.scrollTop,
        escaped: controller?.escapedFromLock ?? false,
      });
      controller?.detach();
      if (stick === controller) stick = null;
    };
  });

  // Jump resolution, inner half: a search hit, review jump, or tray row
  // whose item lives in this run has already relocated the mount window and
  // expanded the run from its chip (`revealActivityRunItem`) — what is left
  // is the scroll, which needs the mounted DOM this effect runs after.
  //
  // Declared AFTER the snapshot effect so it wins: a run mounting with both
  // a saved position and a pending jump has to land on the jump target.
  $effect(() => {
    const clip = clipEl;
    if (!clip) return;
    // The request is the dependency, not the node: a jump can target an item
    // the current window already holds, which changes nothing on the node.
    pane.activityRuns.revision;
    const itemId = pane.activityRuns.takeFocus(run.runId);
    if (!itemId) return;
    const row = activityRunRowIndexOfItem(run, itemId);
    // The item left the run between the request and this flush (a live-window
    // prune). Nothing to scroll to; the outer jump still put the run on
    // screen.
    if (row < run.mountedFrom || row >= run.mountedFrom + run.mountedRows) return;
    const el = activityRunChildElement(clip, row);
    if (!el) return;
    const target = activityRunCenteredScrollTop(clip, el);
    // Explicit navigation inside the run, so it escapes bottom-follow the
    // same way the outer timeline does — otherwise the next streamed chunk
    // would yank the reader off the item they jumped to.
    stick?.setEscapedFromLock(true);
    if (target !== null) clip.scrollTop = target;
  });

  // No head trim, deliberately. Growth cannot accumulate DOM on its own —
  // the slice above is a window, not a high-water mark, so a run that
  // streams to 500 rows still mounts `mountedRows` of them, and a jump
  // relocates the window without enlarging it. The only way past the window
  // is the user asking for another chunk, and trimming that back would revert
  // an explicit action: a short run whose boundary is visible without
  // scrolling would flash the rows in and drop them in the same frame. What
  // they asked for stays until the run unmounts.
</script>

<!-- Rail offsets match what the per-row wrapper used to apply:
     ml-[14px] places the border 14px into the row column (under the
     chevron gutter) and pl-[18px] shifts the body past the icon and
     label gutters. Applying them once here rather than per row is what
     makes the border one continuous line instead of N abutting ones. -->
<div
  class="relative ml-[14px] border-l border-border-subtle pl-[18px]"
  data-testid="activity-run"
  data-rail="true"
  data-run-id={run.runId}
  data-collapsed={run.collapsed ? 'true' : 'false'}
>
  <!-- The rail itself is the collapse control: a hit strip straddling the
       border, in the gutter where no content sits. Absolutely positioned
       so it consumes no width and cannot shift the row. -->
  <button
    type="button"
    class="absolute inset-y-0 -left-2 w-4 cursor-pointer bg-transparent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    onclick={toggle}
    aria-label={toggleLabel}
    aria-expanded={!run.collapsed}
    aria-controls="activity-run-{run.runId}"
    data-testid="activity-run-rail"
  ></button>

  {#if run.collapsed}
    <ActivityRunChip {pane} {run} onExpand={toggle} />
  {:else}
    <!-- overflow-x is hidden on purpose: a wide preview inside a tool row
         must not raise a horizontal bar at run level, which would consume
         HEIGHT and shift every row below. overscroll-behavior stays auto —
         chaining out at the inner edge is wanted, and gesture correctness
         comes from attribution (utils/scroll/wheelAttribution.ts), not
         from blocking the chain. -->
    <div
      bind:this={clipEl}
      id="activity-run-{run.runId}"
      class="activity-run-clip overflow-y-auto overflow-x-hidden [overflow-anchor:none]"
      style:max-height={maxHeight}
      use:nestedScroll
      onscroll={saveInnerScroll}
      data-testid="activity-run-clip"
    >
      <div bind:this={contentEl}>
        {#if hiddenEarlier > 0}
          <ActivityRunBoundary count={hiddenEarlier} edge="earlier" onclick={mountEarlier} />
        {/if}
        <!-- The wrapper carries the row's index because that is the only
             handle a jump has on a non-leaf row: only leaves emit
             `data-item-id`, and a hit inside a subagent card resolves to the
             card. A plain div, so row margins keep collapsing exactly as they
             did when these rows were the virtualizer's own. -->
        {#each mountedChildren as child, i (timelineNodeKey(child))}
          <div data-run-child={run.mountedFrom + i}>
            {@render renderNode(child, depth)}
          </div>
        {/each}
        {#if hiddenLater > 0}
          <ActivityRunBoundary count={hiddenLater} edge="later" onclick={mountLater} />
        {/if}
      </div>
    </div>
    <!-- A zero-width native bar makes the scroll package's geometric
         scrollbar-gutter hit test impossible, so a drag states its intent
         instead of having it inferred. -->
    <OverlayScrollbar
      target={clipEl}
      content={contentEl}
      ariaLabel="Scroll activity run"
      onDragStart={stick ? () => stick?.setEscapedFromLock(true) : undefined}
      onDragEnd={stick ? (atBottom) => { if (atBottom) stick?.markAtBottom(); } : undefined}
    />
  {/if}
</div>
