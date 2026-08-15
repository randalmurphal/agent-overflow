<script lang="ts">
  // A fan-out phase (RUN-MAP §6, fan scale; §7, lane summarization), in the
  // model's choice of two layouts:
  //
  //   - `columns` — the full-width idiom: a fork bar into side-by-side lanes,
  //     each wide enough to read prose in (`--run-map-lane-min` floor), the
  //     row WRAPPING into further ranks when the card runs out of width.
  //     Nothing scrolls sideways: a scrollbar hid whole lanes; a second rank
  //     hides nothing.
  //   - `stacked` — every fan inside a lane. It renders its branches as
  //     full-width blocks in its parent's flow, because columns inside a
  //     column can only subdivide a width that was already minimal.
  //
  // Lanes come in three shapes and the difference is what the reader can DO:
  //
  //   - actionable (running / failed / taken-over / unknown, or the frontier):
  //     an open lane carrying its chain.
  //   - settled with structure under it: the header alone — glyph, name,
  //     duration — and one click puts the whole subtree back (a sole child
  //     arrives open, so the click really is one).
  //   - scalar: a done group renders its chips IN THE FLOW up to eight
  //     (`RUN_MAP_INLINE_DONE_MAX`) — "what completed" is not behind a click —
  //     and folds behind its labelled count past that; queued lanes are ONE
  //     node named by the range they cover, non-interactive because a queued
  //     lane has nothing a click would show.

  import WorkflowRunMapFold from './WorkflowRunMapFold.svelte';
  import WorkflowRunMapLaneHeader from './WorkflowRunMapLaneHeader.svelte';
  import WorkflowRunMapUnitChip from './WorkflowRunMapUnitChip.svelte';
  import { truncateMiddle } from '../../utils/format';
  import { motionReduced } from '../../utils/reducedMotion';
  import type { RunMapCompositionNode, RunMapFan } from '../../utils/workflowRunMap';
  import {
    runMapNodeStyle,
    RUN_MAP_LABEL_MAX,
    RUN_MAP_LANE_MAX,
    RUN_MAP_LANE_MIN,
    RUN_MAP_NODE_BOX,
  } from '../../utils/workflowRunMapStyle';
  import type { Snippet } from 'svelte';
  import type { TransitionConfig } from 'svelte/transition';

  interface Props {
    fan: RunMapFan;
    nowKey: string;
    onOpenThread: (threadId: string) => void;
    onToggleLane: (branchKey: string) => void;
    /** The composition renderer, borrowed from the node so a branch chain and a
        phase chain are drawn by the same code. */
    chain?: Snippet<[RunMapCompositionNode[]]>;
  }
  let { fan, nowKey, onOpenThread, onToggleLane, chain }: Props = $props();

  /** The two horizontal alignments the fan's snippets take: block flow inside
      a stacked fan, centered under the fork in a columns one. */
  type Justify = 'justify-start' | 'justify-center';

  let stacked = $derived(fan.layout === 'stacked');
  let hasBigDone = $derived(!fan.done.inline && (fan.done.count > 0 || fan.done.droppedCount > 0));

  // Per-visit and in-memory by design: which oversized done group the reader
  // opened is a fact about this look at this fan, not about the run. It is
  // deliberately NOT lifted into the overlay store next to
  // wave/composition/lane expansion — those survive a detail remount because
  // the reader navigated away and back, and a group node is one click to
  // reopen. Inline groups (≤8) never fold, so this only ever gates the big
  // ones.
  let doneOpen = $state(false);

  // A group that outgrows the inline bound mid-watch folds — but the chips the
  // reader was just looking at must not vanish behind a closed count, so the
  // flip seeds the fold open. Closing it again is theirs. `null` = first look:
  // a fan that MOUNTS oversized folds closed, which is the resting default.
  let wasInline: boolean | null = null;
  $effect(() => {
    const inline = fan.done.inline;
    if (wasInline === true && !inline) doneOpen = true;
    wasInline = inline;
  });

  // §10: every structural motion gates on the full `motionReduced()` — the
  // app.css reset silences the OS half, and the app's low-power setting is the
  // half only a JS read can see. The enter animation is a CSS class, so the
  // gate drops the class; the leave is imperative and returns a zero duration.
  let animated = $derived(!motionReduced());

  // The queued group's pending styling — the one vocabulary the fan draws
  // with that belongs to no chip.
  let queuedStyle = $derived(runMapNodeStyle('pending'));

  /**
   * §10, case two: a finished branch folds into the done node. A leaving
   * element has no CSS from-value to transition from, so this is the one
   * imperative motion on the surface — and it measures the lane it is leaving
   * rather than assuming the open-column width, because a SETTLED lane is
   * intrinsically sized and would otherwise jump wide on its first frame just
   * to shrink from there. A zero-width read means the lane was never laid
   * out (display:none ancestor), and a slide from nowhere animates nothing.
   */
  function foldIntoChip(node: Element): TransitionConfig {
    if (motionReduced()) return { duration: 0 };
    const width = node.getBoundingClientRect().width;
    if (width === 0) return { duration: 0 };
    return {
      duration: 180,
      css: (t: number) => `opacity: ${t}; flex-basis: ${t * width}px; min-width: 0`,
    };
  }
</script>

{#snippet doneChips(justify: Justify, built: boolean = true)}
  <!-- `built` is the fold's build gate: a closed oversized group keeps its
       container (the fold animates its height) without mounting forty chips
       nobody can see. -->
  <div class={['flex flex-wrap gap-1 pb-1', justify].join(' ')} data-testid="workflow-map-done-chips">
    {#if built}
      {#each fan.done.entries as entry (entry.key)}
        <WorkflowRunMapUnitChip chip={entry} {nowKey} {onOpenThread} />
      {/each}
    {/if}
  </div>
{/snippet}

{#snippet queuedNode()}
  <!--
    Non-interactive by construction, not by a disabled button: the model
    carries no entries for a queued group (§7), so there is nothing a click
    could reveal and no affordance is offered.
  -->
  <p
    class={[RUN_MAP_NODE_BOX, queuedStyle.border, queuedStyle.fill, queuedStyle.label].join(' ')}
    data-testid="workflow-map-group"
    data-group-kind="queued"
  >
    <span class={['mr-1.5', queuedStyle.glyphTone].join(' ')} aria-hidden="true">{queuedStyle.glyph}</span>
    <!-- Same rule as a lane name: the range label IS this node's whole
         identity ("ports 2–4"), so it WRAPS and never ellipsizes — the range
         is built from the phase's display name, which can be a sentence —
         with the same runaway guard every label renderer applies. -->
    <span class="break-words" title={fan.queued.label}>
      {truncateMiddle(fan.queued.label, RUN_MAP_LABEL_MAX)}
    </span>
  </p>
{/snippet}

{#snippet bigDoneButton()}
  {@const doneStyle = runMapNodeStyle('done')}
  <!-- Only a done group PAST the inline bound folds (§7): the button carries
       the count it hides — wearing the done treatment (quiet fill, green ✓)
       because it STANDS FOR done work — and the chips land beneath on
       demand. -->
  <button
    type="button"
    class={[RUN_MAP_NODE_BOX, doneStyle.border, doneStyle.fill,
      'text-[0.6875rem] text-fg-subtle hover:bg-surface-2'].join(' ')}
    aria-expanded={doneOpen}
    onclick={() => { doneOpen = !doneOpen; }}
    data-testid="workflow-map-group"
    data-group-kind="done"
  >
    <span class={['mr-1.5', doneStyle.glyphTone].join(' ')} aria-hidden="true">{doneStyle.glyph}</span>
    {fan.done.label}{fan.done.droppedCount > 0 ? ` · ${fan.done.droppedCount} dropped` : ''}
  </button>
{/snippet}

{#snippet bigDoneFold(justify: Justify)}
  <WorkflowRunMapFold open={doneOpen} testId="workflow-map-group-fold">
    {@render doneChips(justify, doneOpen)}
  </WorkflowRunMapFold>
{/snippet}

{#if stacked}
  <!--
    Inside a lane there is no width to divide, so there are no bars and no
    columns: each branch is a full-width block — header line, then its chain
    behind an indent guide — and the scalar groups are wrapping rows in the
    same flow. Nothing here can create horizontal overflow.
  -->
  <div class="w-full" data-testid="workflow-map-fan" data-fan-layout={fan.layout}>
    {#each fan.columns as column (column.key)}
      <div
        class="w-full pt-1"
        data-testid="workflow-map-branch"
        data-unit-id={column.unit.unitId}
        data-collapsed={column.collapsed}
      >
        <WorkflowRunMapLaneHeader lane={column} stacked {nowKey} {onOpenThread} {onToggleLane} />
        {#if column.chain.length > 0 && chain}
          <div class="run-map-spine run-map-spine-wide run-map-indent mt-1">
            {@render chain(column.chain)}
          </div>
        {/if}
      </div>
    {/each}

    {#if fan.done.inline}
      <div class="pt-1">{@render doneChips('justify-start')}</div>
    {:else if hasBigDone}
      <div class="pt-1">
        {@render bigDoneButton()}
        {@render bigDoneFold('justify-start')}
      </div>
    {/if}

    {#if fan.queued.count > 0}
      <div class="flex justify-start pt-1">
        {@render queuedNode()}
      </div>
    {/if}

    {#if fan.join}
      <div class="flex justify-start pt-1" data-testid="workflow-map-join">
        <WorkflowRunMapUnitChip chip={fan.join} {nowKey} {onOpenThread} />
      </div>
    {/if}
  </div>
{:else}
  <div
    class="w-full"
    style:--run-map-lane-min={RUN_MAP_LANE_MIN}
    style:--run-map-lane-max={RUN_MAP_LANE_MAX}
    data-testid="workflow-map-fan"
    data-fan-layout={fan.layout}
  >
    <!-- No count line here: the wave's summary row already states its unit
         tally, and a second one under the same node was a number the reader had
         to reconcile with the first. The fork itself says a split happened. -->
    <div class="run-map-fork"></div>

    <!-- Centering (and its overflow escape) is `.run-map-lane-row`'s: the row
         is `justify-content: safe center`, so an overflowing rank falls back
         to start alignment instead of bleeding out of both edges. -->
    <div class="run-map-lane-row flex items-start gap-x-3 gap-y-1 pb-1">
      {#each fan.columns as column (column.key)}
        <div
          class={[
            'run-map-lane',
            // A COLLAPSED lane is a summary node, not a column: `flex: none`,
            // exactly its header's content. An OPEN lane gets a READABLE
            // floor and grows to share the row — and the ROW wraps when the
            // card runs out, rather than scrolling lanes out of sight.
            column.collapsed
              ? 'flex-none'
              : 'min-w-[var(--run-map-lane-min)] max-w-[var(--run-map-lane-max)] flex-[1_1_var(--run-map-lane-min)]',
            animated && !column.collapsed ? 'run-map-column' : '',
          ].filter(Boolean).join(' ')}
          out:foldIntoChip
          data-testid="workflow-map-branch"
          data-unit-id={column.unit.unitId}
          data-collapsed={column.collapsed}
        >
          <WorkflowRunMapLaneHeader lane={column} stacked {nowKey} {onOpenThread} {onToggleLane} />
          {#if column.chain.length > 0 && chain}
            <div class="run-map-spine run-map-spine-wide mt-1.5">
              {@render chain(column.chain)}
            </div>
          {/if}
        </div>
      {/each}

      {#if hasBigDone}
        <!-- The button rides the lane row; its chips do NOT — they land below
             as a full-width block, because a forty-chip row inside a
             `flex-none` lane is wider than any card and dragged the whole row
             past the edge. -->
        <div class="run-map-lane shrink-0 self-start">
          {@render bigDoneButton()}
        </div>
      {/if}

      {#if fan.queued.count > 0}
        <div class="run-map-lane shrink-0 self-start">
          {@render queuedNode()}
        </div>
      {/if}
    </div>

    {#if hasBigDone}
      {@render bigDoneFold('justify-center')}
    {/if}

    {#if fan.done.inline}
      {@render doneChips('justify-center')}
    {/if}

    {#if fan.join}
      <div class="run-map-rejoin"></div>
      <div class="flex justify-center" data-testid="workflow-map-join">
        <WorkflowRunMapUnitChip chip={fan.join} {nowKey} {onOpenThread} />
      </div>
    {/if}
  </div>
{/if}

<style>
  /*
   * §10, case one: a branch column enters from the queued group node — a width
   * slide local to the fan. One owned animation. The class is applied only
   * while `motionReduced()` is false, so low power drops the animation
   * outright; app.css's reduced-motion reset covers the OS half on top.
   *
   * The lane widths come from the container's custom properties rather than
   * literals, so the resting column and its enter frame cannot disagree.
   */
  .run-map-column {
    animation: run-map-column-enter 200ms ease-out;
  }

  @keyframes run-map-column-enter {
    from {
      flex-basis: 0;
      min-width: 0;
      opacity: 0;
    }
    to {
      flex-basis: var(--run-map-lane-min);
      min-width: var(--run-map-lane-min);
      opacity: 1;
    }
  }
</style>
