<script lang="ts">
  // A fan-out phase (RUN-MAP §6, fan scale): the split bar, one column per
  // branch with structure or actionability, the `queued ·N` / `done ·N` group
  // chips that flank them, and the join node the branches merge back into.
  //
  // Columns are the ONLY horizontally elastic element on the surface — the
  // spine never scrolls sideways, so the overflow lives on this row alone. The
  // group chips are information design, not just space: 32 uniform columns
  // communicate nothing, so the interesting subset gets geometry and the bulk
  // gets arithmetic, expandable inline into a wrapping chip grid.

  import WorkflowRunMapFold from './WorkflowRunMapFold.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import { motionReduced } from '../../utils/reducedMotion';
  import type {
    RunMapCompositionNode,
    RunMapFan,
    RunMapUnitChip,
    RunMapUnitGroup,
  } from '../../utils/workflowRunMap';
  import { runMapNodeStyle, RUN_MAP_LABEL_MAX } from '../../utils/workflowRunMapStyle';
  import type { Snippet } from 'svelte';
  import type { TransitionConfig } from 'svelte/transition';

  interface Props {
    fan: RunMapFan;
    nowKey: string;
    onOpenThread: (threadId: string) => void;
    /** The composition renderer, borrowed from the node so a branch chain and a
        phase chain are drawn by the same code. */
    chain?: Snippet<[RunMapCompositionNode[]]>;
  }
  let { fan, nowKey, onOpenThread, chain }: Props = $props();

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

  // Per-visit and in-memory by design: which group chips the reader opened is
  // a fact about this look at this fan, not about the run. It is deliberately
  // NOT lifted into the overlay store next to wave/composition expansion —
  // those survive a detail remount because the reader navigated away and back,
  // and a group chip is one click to reopen.
  let expandedGroups = $state(new Set<string>());

  function toggleGroup(key: string): void {
    const next = new Set(expandedGroups);
    if (!next.delete(key)) next.add(key);
    expandedGroups = next;
  }

  // §10: every structural motion gates on the full `motionReduced()` — the
  // app.css reset silences the OS half, and the app's low-power setting is the
  // half only a JS read can see. The enter animation is a CSS class, so the
  // gate drops the class; the leave is imperative and returns a zero duration.
  let animated = $derived(!motionReduced());

  /**
   * §10, case two: a finished branch folds into the done chip. A leaving
   * element has no CSS from-value to transition from, so this is the one
   * imperative motion on the surface.
   */
  function foldIntoChip(_node: Element): TransitionConfig {
    if (motionReduced()) return { duration: 0 };
    return {
      duration: 180,
      css: (t: number) => `opacity: ${t}; flex-basis: ${t * LANE_MAX_PX}px; min-width: 0`,
    };
  }
</script>

{#snippet unitChip(chip: RunMapUnitChip, prominent: boolean)}
  {@const style = runMapNodeStyle(chip.signal)}
  {@const isNow = chip.key === nowKey}
  <div
    class={[
      'flex items-baseline gap-1.5 rounded border px-1.5 py-0.5',
      style.border,
      style.glow,
      prominent ? 'bg-surface-1/60' : 'bg-surface-1/30',
    ].filter(Boolean).join(' ')}
    data-run-map-now={isNow ? 'true' : undefined}
    data-unit-id={chip.unitId}
    data-unit-signal={chip.signal}
  >
    {#if isNow}
      <span class="shrink-0 text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
    {/if}
    {#if style.spinner}
      <SteppedSpinner size={10} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
    {:else}
      <span class={['shrink-0 text-[0.6875rem]', style.tone].join(' ')} aria-hidden="true">{style.glyph}</span>
    {/if}
    <button
      type="button"
      class={[
        'min-w-0 flex-1 truncate text-left text-[0.6875rem] hover:underline disabled:cursor-default disabled:no-underline',
        style.label,
        chip.struck ? 'line-through' : '',
      ].filter(Boolean).join(' ')}
      disabled={!chip.threadId}
      title={chip.label}
      onclick={() => onOpenThread(chip.threadId)}
    >
      {truncateMiddle(chip.label, RUN_MAP_LABEL_MAX)}
    </button>
    <span class="shrink-0 text-[0.625rem] tabular-nums text-fg-hint">{chip.meta}</span>
  </div>
{/snippet}

{#snippet groupChip(group: RunMapUnitGroup)}
  <button
    type="button"
    class="shrink-0 self-start rounded border border-border-subtle bg-surface-1/40 px-1.5 py-0.5 text-[0.6875rem] text-fg-subtle hover:bg-surface-2/50"
    aria-expanded={expandedGroups.has(group.key)}
    onclick={() => toggleGroup(group.key)}
    data-testid="workflow-map-group"
    data-group-kind={group.kind}
  >
    {group.label}{group.droppedCount > 0 ? ` · ${group.droppedCount} dropped` : ''}
  </button>
{/snippet}

<div
  class="mt-1"
  style:--run-map-lane-min="{LANE_MIN_PX}px"
  style:--run-map-lane-max="{LANE_MAX_PX}px"
  data-testid="workflow-map-fan"
>
  <div class="flex items-center gap-2 py-0.5 text-[0.625rem] uppercase tracking-wider text-fg-hint">
    <span class="h-px flex-1 bg-border-subtle"></span>
    <span>{fan.totals.total} {fan.totals.total === 1 ? 'unit' : 'units'}</span>
    <span class="h-px flex-1 bg-border-subtle"></span>
  </div>

  <div class="flex items-stretch gap-2 overflow-x-auto pb-1">
    {#if fan.queued.count > 0}
      {@render groupChip(fan.queued)}
    {/if}
    {#each fan.columns as column (column.key)}
      <div
        class={[
          'min-w-[var(--run-map-lane-min)] max-w-[var(--run-map-lane-max)] flex-[1_1_var(--run-map-lane-max)]',
          animated ? 'run-map-column' : '',
        ].filter(Boolean).join(' ')}
        out:foldIntoChip
        data-testid="workflow-map-branch"
        data-unit-id={column.unit.unitId}
      >
        {@render unitChip(column.unit, true)}
        {#if column.chain.length > 0 && chain}
          {@render chain(column.chain)}
        {/if}
      </div>
    {/each}
    {#if fan.done.count > 0 || fan.done.droppedCount > 0}
      {@render groupChip(fan.done)}
    {/if}
  </div>

  {#each [fan.queued, fan.done] as group (group.key)}
    <WorkflowRunMapFold open={expandedGroups.has(group.key)} testId="workflow-map-group-fold">
      <div class="flex flex-wrap gap-1 pb-1">
        {#if expandedGroups.has(group.key)}
          {#each group.entries as entry (entry.key)}
            <div class="w-[calc(50%-0.125rem)] min-w-[var(--run-map-lane-min)] grow">
              {@render unitChip(entry, false)}
            </div>
          {/each}
        {/if}
      </div>
    </WorkflowRunMapFold>
  {/each}

  {#if fan.join}
    <div class="pt-0.5" data-testid="workflow-map-join">
      {@render unitChip(fan.join, false)}
    </div>
  {/if}
</div>

<style>
  /*
   * §10, case one: a branch column enters from the queued group chip — a width
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
