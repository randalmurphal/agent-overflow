<script lang="ts">
  // A fan-out phase (RUN-MAP §6, fan scale; §7, lane summarization): the fork
  // that splits the spine into lanes, the lanes themselves, and the rejoin the
  // join node sits under.
  //
  // Lanes come in three shapes and the difference is what the reader can DO:
  //
  //   - actionable (running / failed / taken-over / unknown, or the frontier):
  //     an open column carrying its chain.
  //   - settled with structure under it: the header alone — glyph, name,
  //     duration, which is the whole summary — and one click puts the subtree
  //     back. Painting a finished child workflow's whole history in a lane is
  //     what turned a three-lane campaign into sixty rows.
  //   - scalar: arithmetic. Queued lanes become ONE node named by the range
  //     they cover (`ports 2–4 · queued`), non-interactive because a queued
  //     lane has nothing a click would show; finished ones become the `done ·N`
  //     node, which still expands, because the dropped units live in there and
  //     nothing else states them.
  //
  // Columns are the ONLY horizontally elastic element on the surface — the
  // spine never scrolls sideways, so the overflow lives on this row alone.

  import WorkflowRunMapFold from './WorkflowRunMapFold.svelte';
  import WorkflowRunMapUnitChip from './WorkflowRunMapUnitChip.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import { motionReduced } from '../../utils/reducedMotion';
  import type {
    RunMapBranch,
    RunMapCompositionNode,
    RunMapFan,
    RunMapUnitChip,
  } from '../../utils/workflowRunMap';
  import {
    runMapNodeStyle,
    RUN_MAP_LABEL_MAX,
    RUN_MAP_LANE_HEADER,
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

  /**
   * The lane geometry, declared once. A branch column's resting width, its
   * enter animation and its leaving transition are three renderings of the
   * same two numbers, and they were three literals in markup, keyframes and
   * JS — the shapes drift the moment one of them is tuned. The markup and the
   * keyframes read the custom properties set on the fan container below; the
   * transition reads the constant directly, because a leaving element's CSS is
   * generated in JS.
   *
   * `app.css` declares both properties on `:root` as well, at these values.
   * That is not a second source of truth — the container's `style:` always wins
   * for anything the fan renders — it is the guarantee that `var()` RESOLVES.
   * An unresolved custom property makes its whole declaration
   * invalid-at-computed-value-time, so `min-width: var(--run-map-lane-min)`
   * silently becomes `min-width: auto` for any consumer that ever ends up
   * outside this container, which is a layout collapse with nothing to grep for.
   */
  const LANE_MIN_PX = 120;
  const LANE_MAX_PX = 200;

  // Per-visit and in-memory by design: which done group the reader opened is a
  // fact about this look at this fan, not about the run. It is deliberately
  // NOT lifted into the overlay store next to wave/composition/lane expansion —
  // those survive a detail remount because the reader navigated away and back,
  // and a group node is one click to reopen.
  let doneOpen = $state(false);

  // §10: every structural motion gates on the full `motionReduced()` — the
  // app.css reset silences the OS half, and the app's low-power setting is the
  // half only a JS read can see. The enter animation is a CSS class, so the
  // gate drops the class; the leave is imperative and returns a zero duration.
  let animated = $derived(!motionReduced());

  // The two vocabularies the fan draws with that belong to no chip: the queued
  // group's own pending styling, and the ghost border a lane's disclosure and
  // the loop stubs share.
  let queuedStyle = $derived(runMapNodeStyle('pending'));
  let ghostStyle = $derived(runMapNodeStyle('ghost'));

  /**
   * §10, case two: a finished branch folds into the done node. A leaving
   * element has no CSS from-value to transition from, so this is the one
   * imperative motion on the surface — and it measures the lane it is leaving
   * rather than assuming the open-column width, because a SETTLED lane is
   * intrinsically sized and would otherwise jump out to `LANE_MAX_PX` on its
   * first frame just to shrink from there.
   */
  function foldIntoChip(node: Element): TransitionConfig {
    if (motionReduced()) return { duration: 0 };
    const width = node.getBoundingClientRect().width || LANE_MAX_PX;
    return {
      duration: 180,
      css: (t: number) => `opacity: ${t}; flex-basis: ${t * width}px; min-width: 0`,
    };
  }
</script>

{#snippet laneHeader(chip: RunMapUnitChip, lane: RunMapBranch | null)}
  {@const style = runMapNodeStyle(chip.signal)}
  {@const isNow = chip.key === nowKey}
  {@const folded = lane?.collapsed === true}
  <div
    class={[
      'flex items-baseline justify-center gap-1.5',
      folded ? 'whitespace-nowrap' : '',
      style.tone,
    ].filter(Boolean).join(' ')}
    data-run-map-now={isNow ? 'true' : undefined}
    data-unit-id={chip.unitId}
    data-unit-signal={chip.signal}
  >
    {#if isNow}
      <span class="shrink-0 text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
    {/if}
    {#if style.spinner}
      <SteppedSpinner size={9} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
    {:else}
      <span class="shrink-0 text-[0.6875rem]" aria-hidden="true">{style.glyph}</span>
    {/if}
    <button
      type="button"
      class={[
        RUN_MAP_LANE_HEADER,
        'text-left hover:underline disabled:cursor-default disabled:no-underline',
        // A FOLDED lane is one line, so the name is the only identity it has
        // left — it must be the last thing to give, not the first. An OPEN
        // column has its whole chain underneath to say what it is, and it lives
        // inside a 120–200px column, so there the name yields.
        folded ? 'whitespace-nowrap' : 'min-w-0 truncate',
        chip.struck ? 'line-through' : '',
      ].filter(Boolean).join(' ')}
      disabled={!chip.threadId}
      title={chip.label}
      onclick={() => onOpenThread(chip.threadId)}
      data-testid="workflow-map-lane-name"
    >
      {truncateMiddle(chip.label, RUN_MAP_LABEL_MAX)}
    </button>
    {#if chip.duration}
      <span class="shrink-0 text-[0.625rem] tabular-nums text-fg-hint">{chip.duration}</span>
    {/if}
    {#if lane?.toggleable}
      <!--
        A settled lane's subtree is one click away, and the click says how much
        is behind it rather than being a bare chevron. Inline with the name, so
        the lane really is ONE line while it is folded (§7).
      -->
      <button
        type="button"
        class="shrink-0 rounded border border-dashed border-border-subtle px-1 text-[0.625rem]
               text-fg-hint hover:bg-surface-2/50"
        aria-expanded={!lane.collapsed}
        onclick={() => onToggleLane(lane.key)}
        data-testid="workflow-map-lane-toggle"
      >
        {chip.childRunCount === 1 ? '1 run' : `${chip.childRunCount} runs`}
      </button>
    {/if}
  </div>
{/snippet}

<div
  class="w-full"
  style:--run-map-lane-min="{LANE_MIN_PX}px"
  style:--run-map-lane-max="{LANE_MAX_PX}px"
  data-testid="workflow-map-fan"
>
  <!-- No count line here: the wave's summary row already states its unit
       tally, and a second one under the same node was a number the reader had
       to reconcile with the first. The fork itself says a split happened. -->
  <div class="run-map-fork"></div>

  <!--
    `run-map-lane-row` centers via auto margins rather than `justify-center`:
    a fan wider than the card overflows BOTH edges under `justify-center`, and
    the half before the scroll origin cannot be reached (§6, fan width).
  -->
  <div class="run-map-lane-row flex items-start gap-2 overflow-x-auto pb-1">
    {#each fan.columns as column (column.key)}
      <div
        class={[
          'run-map-lane',
          // A COLLAPSED lane is a summary node, not a column: `flex: none`, so
          // it is EXACTLY its header's content and never gives any of it up.
          // A folded lane has collapsed to one line, and the unit's name is the
          // only identity that line has left — the first attempt let it shrink
          // "first, because a finished lane costs the reader nothing", and what
          // that actually cost them was "✓ POR… 2s" beside a fully-named open
          // column. The OPEN columns are what flexes instead (120px floor,
          // 200px preference) and past that the fan region's own horizontal
          // scroll takes over, which is §6's declared escape for fan width.
          // The enter animation is a column opening, so it does not belong to a
          // folded lane either.
          column.collapsed
            ? 'flex-none'
            : 'min-w-[var(--run-map-lane-min)] max-w-[var(--run-map-lane-max)] flex-[1_1_var(--run-map-lane-max)]',
          animated && !column.collapsed ? 'run-map-column' : '',
        ].filter(Boolean).join(' ')}
        out:foldIntoChip
        data-testid="workflow-map-branch"
        data-unit-id={column.unit.unitId}
        data-collapsed={column.collapsed}
      >
        {@render laneHeader(column.unit, column)}
        {#if column.chain.length > 0 && chain}
          <div class="run-map-spine run-map-spine-wide mt-1.5">
            {@render chain(column.chain)}
          </div>
        {/if}
      </div>
    {/each}

    {#if fan.done.count > 0 || fan.done.droppedCount > 0}
      <div class="run-map-lane shrink-0 self-start" data-testid="workflow-map-group" data-group-kind="done">
        <button
          type="button"
          class={[RUN_MAP_NODE_BOX, 'border-border-subtle bg-surface-1/40 text-[0.6875rem] text-fg-subtle',
            'hover:bg-surface-2/50'].join(' ')}
          aria-expanded={doneOpen}
          onclick={() => { doneOpen = !doneOpen; }}
        >
          {fan.done.label}{fan.done.droppedCount > 0 ? ` · ${fan.done.droppedCount} dropped` : ''}
        </button>
      </div>
    {/if}

    {#if fan.queued.count > 0}
      <!--
        Non-interactive by construction, not by a disabled button: the model
        carries no entries for a queued group (§7), so there is nothing a click
        could reveal and no affordance is offered.
      -->
      <div class="run-map-lane shrink-0 self-start" data-testid="workflow-map-group" data-group-kind="queued">
        <p class={[RUN_MAP_NODE_BOX, queuedStyle.border, queuedStyle.label].join(' ')}>
          <span class={['shrink-0', queuedStyle.tone].join(' ')} aria-hidden="true">{queuedStyle.glyph}</span>
          <!-- Same rule as a folded lane: the range label IS this node's whole
               identity ("ports 2–4"), so it never truncates. Its own width is
               what grows, and the fan region scrolls. -->
          <span class="whitespace-nowrap">{fan.queued.label}</span>
        </p>
      </div>
    {/if}
  </div>

  <WorkflowRunMapFold open={doneOpen} testId="workflow-map-group-fold">
    <div class="flex flex-wrap justify-center gap-1 pb-1">
      {#if doneOpen}
        {#each fan.done.entries as entry (entry.key)}
          <WorkflowRunMapUnitChip chip={entry} {nowKey} {onOpenThread} />
        {/each}
      {/if}
    </div>
  </WorkflowRunMapFold>

  {#if fan.join}
    <div class="run-map-rejoin"></div>
    <div class="flex justify-center" data-testid="workflow-map-join">
      <WorkflowRunMapUnitChip chip={fan.join} {nowKey} {onOpenThread} />
    </div>
  {/if}
</div>

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
      flex-basis: var(--run-map-lane-max);
      min-width: var(--run-map-lane-min);
      opacity: 1;
    }
  }
</style>
