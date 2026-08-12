<script lang="ts">
  // One wave, header and all (RUN-MAP §2, §6, §8): the corrupt-definition
  // notice, the summary row, and — when the wave is open — the card that holds
  // its flow.
  //
  // The card is the map's one structural emphasis. A settled lap is a single
  // bordered line in the centered column; the wave the run is IN is a framed
  // body with the spine running through it. That contrast is what makes the map
  // read as a flow with one open path rather than as a wall of rows, and it is
  // why the row lives in here rather than beside it: a card that wrapped the
  // body but not the header would put the wave's name outside its own frame.
  //
  // Render-only: the model arrives built. Laziness is still §6's — the
  // projection leaves `segments` null for a folded wave nobody opened, so a
  // 40-wave campaign pays for the lists that are actually on screen — but the
  // decision and the walk are the projection's, not this component's. A
  // derivation here meant one full tree index and one full frontier collection
  // PER OPEN WAVE, once a second, off the shared clock.
  //
  // OPENNESS IS THE MODEL'S, not a prop. `segments === null` IS "the projection
  // did not build this wave", which is the same question a separate `open` flag
  // answered — and two answers to one question is one caller away from an open
  // wave with no segments, which renders "Nothing recorded in this wave yet."
  // about a wave full of records. `runMapWaveIsOpen` is the one spelling.

  import WorkflowRunMapFold from './WorkflowRunMapFold.svelte';
  import WorkflowRunMapNode from './WorkflowRunMapNode.svelte';
  import WorkflowRunMapSummaryRow from './WorkflowRunMapSummaryRow.svelte';
  import { runMapWaveIsOpen } from '../../utils/workflowRunMap';
  import { runMapNodeStyle, RUN_MAP_CARD } from '../../utils/workflowRunMapStyle';
  import type { RunMapWave } from '../../utils/workflowRunMap';

  interface Props {
    wave: RunMapWave;
    nowKey: string;
    /** Also threaded down: a composition's finished laps fold by the same rule. */
    onToggleWave: (waveItemId: string) => void;
    onOpenThread: (threadId: string) => void;
    onToggleComposition: (itemId: string) => void;
    onToggleLane: (branchKey: string) => void;
  }
  let {
    wave,
    nowKey,
    onToggleWave,
    onOpenThread,
    onToggleComposition,
    onToggleLane,
  }: Props = $props();

  let open = $derived(runMapWaveIsOpen(wave));

  // The wave the run is actually in gets the emphasis, and it is position +
  // weight rather than a hue (§2): a live wave on the frontier path is the one
  // whose frame reads as the current line.
  let current = $derived(!wave.folded && wave.onFrontierPath);

  // §5.8 has TWO causes and they are not the same news. An ABSENT snapshot is
  // ordinary history — a run that failed before it ever froze one — and
  // records-only mode renders it silently. A snapshot that would not DECODE is
  // a defect in a stored record, so it is stated, in the hue R1 gives a defect.
  // The decode failure itself stays off the surface (R2): what the reader can
  // act on is that the definition is unreadable, not the bytes that were in it.
  let corrupt = $derived(runMapNodeStyle('failed'));
</script>

<!--
  Outside the card on purpose: a corrupt wave is usually a terminal one, so its
  body is folded by default, and news the reader has to open a fold to find is
  news they do not get.
-->
{#if wave.skeletonError}
  <p
    class={['mb-1 rounded border px-2 py-1 text-[0.6875rem]', corrupt.border, corrupt.tone].join(' ')}
    data-testid="workflow-map-wave-skeleton-error"
  >
    This wave’s stored definition could not be read, so only what it recorded is shown.
  </p>
{/if}

<div
  class={[
    RUN_MAP_CARD,
    current ? 'border-border-strong bg-surface-1/40' : 'border-border-subtle',
    open ? '' : 'py-1',
  ].filter(Boolean).join(' ')}
  data-testid="workflow-map-wave-card"
  data-current={current}
>
  <WorkflowRunMapSummaryRow
    summary={wave.summary}
    signal={wave.signal}
    ordinal={wave.ordinal}
    expanded={open}
    toggleable={wave.folded}
    onToggle={() => onToggleWave(wave.itemId)}
  />

  <WorkflowRunMapFold {open} testId="workflow-map-wave-fold">
    <div class="pt-1.5" data-testid="workflow-map-wave-body">
      <!--
        Nothing at all while closed. The fold WRAPPER stays mounted in both
        states (it needs a from-value to transition), but what it wraps must be
        cheap to leave mounted — and, more than cheap, it must not be a sentence
        that is false. A closed wave has no segments because nobody asked for
        them, which is not the same statement as "this wave recorded nothing".
      -->
      {#if wave.segments !== null}
        {#if wave.segments.length === 0}
          <p class="py-1 text-center text-[0.6875rem] text-fg-hint" data-testid="workflow-map-wave-empty">
            Nothing recorded in this wave yet.
          </p>
        {:else}
          <!--
            `-wide`: each node OWNS the column's width even though its box sits
            centered inside it. A shrink-to-fit row would make a fan's split bar
            and the loop foot's two branches as narrow as the text above them,
            and the spine connector would hang off whatever that width happened
            to be. The box is centered by the row's own nested spine, not by
            being narrow.
          -->
          <ul class="run-map-spine run-map-spine-wide">
            {#each wave.segments as node (node.key)}
              <WorkflowRunMapNode {node} {nowKey} {onOpenThread} {onToggleWave} {onToggleComposition} {onToggleLane} />
            {/each}
          </ul>
        {/if}
      {/if}
    </div>
  </WorkflowRunMapFold>
</div>
