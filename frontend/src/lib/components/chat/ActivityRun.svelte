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

  import type { Snippet } from 'svelte';
  import { tick } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
  import { timelineNodeKey } from '../../utils/subagentGrouping';
  import { ACTIVITY_RUN_OLDER_CHUNK_ROWS } from '../../utils/activityRunGrouping';
  import {
    activityRunClipMaxHeight,
    activityRunExpandedBodies,
    activityRunExpandedHeight,
  } from '../../utils/activityRunClip';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import ActivityRunChip from './ActivityRunChip.svelte';

  let {
    pane,
    run,
    depth,
    renderNode,
  }: {
    pane: ThreadPane;
    run: ActivityRunNode;
    depth: number;
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  let clipEl = $state<HTMLElement | undefined>();
  let expandedPx = $state(0);

  // Tail window: only the newest `mountedRows` children are in the DOM.
  // Without it a 200-row run would mount 200 rows the moment its single
  // timeline row entered the virtualizer's buffer — the DOM bound the
  // virtualizer provides at top level has to be re-established inside.
  let firstMounted = $derived(Math.max(0, run.children.length - run.mountedRows));
  let mountedChildren = $derived(run.children.slice(firstMounted));
  let hiddenCount = $derived(firstMounted);

  let maxHeight = $derived(activityRunClipMaxHeight(expandedPx));
  let toggleLabel = $derived(
    run.collapsed ? 'Expand activity run' : 'Collapse activity run',
  );

  function toggle(): void {
    pane.activityRuns.toggleCollapsed(run.runId);
  }

  async function mountOlder(): Promise<void> {
    const clip = clipEl;
    const beforeHeight = clip?.scrollHeight ?? 0;
    const beforeTop = clip?.scrollTop ?? 0;

    pane.activityRuns.setMountedRows(
      run.runId,
      run.mountedRows + ACTIVITY_RUN_OLDER_CHUNK_ROWS,
    );
    await tick();

    // Manual prepend compensation. WebKit has no `overflow-anchor`, so
    // rows added ABOVE the viewport would otherwise push the reading
    // position down by exactly their height. Two reads and a write, after
    // the DOM has the new rows and before the user can see the frame.
    if (!clip) return;
    const grew = clip.scrollHeight - beforeHeight;
    if (grew > 0) clip.scrollTop = beforeTop + grew;
  }

  // Expanded payloads lift the cap by their own height (see
  // utils/activityRunClip.ts). Re-query on two triggers and no others: an
  // `aria-expanded` flip anywhere in the run, and a change to the mounted
  // set — a row can remount already-expanded from its lease, which mutates
  // no attribute. Streaming text growth touches neither, so this stays off
  // the hot path.
  $effect(() => {
    const clip = clipEl;
    if (!clip || run.collapsed) {
      expandedPx = 0;
      return;
    }
    mountedChildren;

    const sizes = new ResizeObserver(() => {
      expandedPx = activityRunExpandedHeight(activityRunExpandedBodies(clip));
    });
    function retarget(): void {
      const bodies = activityRunExpandedBodies(clip!);
      sizes.disconnect();
      for (const body of bodies) sizes.observe(body);
      expandedPx = activityRunExpandedHeight(bodies);
    }
    const disclosures = new MutationObserver(retarget);
    disclosures.observe(clip, {
      subtree: true,
      attributes: true,
      attributeFilter: ['aria-expanded'],
    });
    retarget();

    return () => {
      disclosures.disconnect();
      sizes.disconnect();
    };
  });
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
      data-testid="activity-run-clip"
    >
      {#if hiddenCount > 0}
        <button
          type="button"
          class="mb-1 flex w-full cursor-pointer items-center gap-2 bg-transparent py-1 text-left text-[0.6875rem] text-fg-hint hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          onclick={mountOlder}
          data-testid="activity-run-older"
        >
          <span aria-hidden="true">· · ·</span>
          {hiddenCount} earlier
        </button>
      {/if}
      {#each mountedChildren as child (timelineNodeKey(child))}
        {@render renderNode(child, depth)}
      {/each}
    </div>
  {/if}
</div>
