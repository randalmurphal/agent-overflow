<script lang="ts">
  // A called run inside its caller (RUN-MAP §3, §7). Two shapes, and which one
  // it takes is the map's whole reading rule:
  //
  //   - OFF the frontier path (the default, at every depth): one summary row —
  //     glyph, workflow, duration, subtree counts — with a click that opens it.
  //     A settled child workflow with forty laps of adjudication behind it is
  //     one line, because none of it is where the run IS.
  //   - ON the frontier path: a compact bordered sub-card holding the flow, its
  //     finished laps folded to the same rows a top-level lap folds to, and an
  //     amber blocker line when it is waiting on a person (R1's one hue here).
  //
  // The sub-card is what makes the live path legible: it is a frame around
  // "here is what is happening now", nested inside the frame around "here is
  // the wave that is happening now".
  //
  // Recursion arrives as a SNIPPET (`segments`) rather than an import: a lap's
  // segments are run-map nodes, and a node is what renders this component. One
  // direction of import, no cycle.

  import type { Snippet } from 'svelte';
  import WorkflowRunMapFold from './WorkflowRunMapFold.svelte';
  import WorkflowRunMapSummaryRow from './WorkflowRunMapSummaryRow.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import type { RunMapCompositionNode, RunMapSegmentNode } from '../../utils/workflowRunMap';
  import {
    runMapNodeStyle,
    RUN_MAP_CARD,
    RUN_MAP_LABEL_MAX,
    RUN_MAP_NODE_BOX,
  } from '../../utils/workflowRunMapStyle';

  interface Props {
    composition: RunMapCompositionNode;
    onToggleWave: (waveItemId: string) => void;
    onToggleComposition: (itemId: string) => void;
    /** How a lap's own nodes are drawn — the recursion, passed rather than imported. */
    segments: Snippet<[RunMapSegmentNode[]]>;
  }
  let { composition, onToggleWave, onToggleComposition, segments }: Props = $props();

  let style = $derived(runMapNodeStyle(composition.signal));
  let blocker = $derived(runMapNodeStyle('parked'));
  // The counts belong on the row that stands IN for the subtree. Open, the
  // subtree is on screen and repeating its arithmetic beside it is noise.
  let meta = $derived([composition.collapsed ? composition.summary.label : '', composition.duration]
    .filter((part) => part !== '')
    .join(' · '));
</script>

<!--
  `data-composition-item-id`, not `data-item-id`: three surfaces already carry
  the latter with three different referents (the run detail's run, the map's
  run, a sidebar row's run), and `uiRenderTrace` walks `[data-item-id]`
  app-wide as "one timeline row". A called run inside a map node is none of
  those.
-->
<div
  class={composition.collapsed ? 'max-w-full' : `${RUN_MAP_CARD} w-full border-border-subtle bg-surface-1/30`}
  data-testid="workflow-map-composition"
  data-composition-item-id={composition.itemId}
  data-collapsed={composition.collapsed}
>
  <button
    type="button"
    class={[
      composition.collapsed ? RUN_MAP_NODE_BOX : 'flex w-full items-baseline gap-2 px-0.5 text-xs',
      // Border and glow belong to the COLLAPSED row, which is a node on the
      // spine and has to read as one. Open, the row is the sub-card's title:
      // the card is already the frame, and an amber glow on a borderless line
      // drew a phantom box that duplicated the blocker line right beneath it.
      composition.collapsed ? `${style.border} ${style.glow}` : '',
      composition.toggleable ? 'hover:bg-surface-2/50' : 'cursor-default',
    ].filter(Boolean).join(' ')}
    disabled={!composition.toggleable}
    aria-expanded={!composition.collapsed}
    onclick={() => onToggleComposition(composition.itemId)}
    data-testid="workflow-map-composition-row"
  >
    {#if style.spinner}
      <SteppedSpinner size={10} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
    {:else}
      <span class={['shrink-0', style.tone].join(' ')} aria-hidden="true">{style.glyph}</span>
    {/if}
    <span class="min-w-0 truncate text-fg-muted" title={composition.label}>
      {truncateMiddle(composition.label, RUN_MAP_LABEL_MAX)}
    </span>
    {#if meta}
      <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-hint">{meta}</span>
    {/if}
  </button>

  {#if composition.blockerLabel && !composition.collapsed}
    <!-- R1's one hue on this surface: a person is blocked, and the sub-card
         says so where the reader is already looking. -->
    <p
      class={['mt-1 rounded border px-1.5 py-0.5 text-[0.6875rem]', blocker.border, blocker.glow, blocker.tone]
        .join(' ')}
      data-testid="workflow-map-composition-blocker"
    >
      {blocker.glyph} {composition.blockerLabel}
    </p>
  {/if}

  {#if !composition.collapsed}
    <ul class="run-map-spine run-map-spine-wide mt-1.5">
      {#each composition.waves as wave (wave.key)}
        <li data-testid="workflow-map-composition-wave" data-wave-item-id={wave.itemId}>
          <!--
            A lap is a lap: the same row a top-level wave folds to, off the same
            expansion set. A composition that opened its whole history would put
            the reader back in front of the wall this fold exists to prevent —
            the live lap is the flow, everything before it is one line.

            The row is drawn only when the composition has more than one lap, or
            when that lap is folded. A single live lap needs no header: the
            sub-card above it already names the run.
          -->
          {#if composition.waves.length > 1 || wave.folded}
            <WorkflowRunMapSummaryRow
              summary={wave.summary}
              signal={wave.signal}
              ordinal={wave.ordinal}
              expanded={wave.segments !== null}
              toggleable={wave.folded}
              onToggle={() => onToggleWave(wave.itemId)}
            />
          {/if}
          <WorkflowRunMapFold open={wave.segments !== null} testId="workflow-map-composition-fold">
            {#if wave.segments !== null}
              {@render segments(wave.segments)}
            {/if}
          </WorkflowRunMapFold>
        </li>
      {/each}
    </ul>
  {/if}
</div>
