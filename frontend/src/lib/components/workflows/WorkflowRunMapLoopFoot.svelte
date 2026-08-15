<script lang="ts">
  // The loop decision at the foot of a wave (RUN-MAP §3): the terminal
  // tail-self-call phase does not render as a phase node — it renders as the
  // fork the campaign is about to take.
  //
  // Undecided, that is literally a fork: one dashed stem splitting into the two
  // outcomes side by side, because they are alternatives and a stacked list
  // reads as two things that will both happen. Decided, only the branch that
  // was taken is drawn — there is no longer an alternative to show.

  import type { RunMapLoop } from '../../utils/workflowRunMap';

  interface Props {
    loop: RunMapLoop;
  }
  let { loop }: Props = $props();
</script>

<!--
  No label box of its own: the node above already drew the tail phase's row,
  with its status, its glyph and its duration. Repeating `loop.label` here put
  the phase's name on the page twice in a row, which read as two steps.
-->
<div class="run-map-spine w-full" data-testid="workflow-map-decision">
  <span class="text-[0.625rem] tabular-nums text-fg-hint">{loop.lapLabel}</span>

  {#if loop.softStopNote}
    <!--
      Neutral, not amber: R1 reserves `--warning` for a human being BLOCKED.
      A standing stop request is a fact about what the loop will do next,
      and nothing is waiting on anyone.
    -->
    <p class="text-[0.6875rem] text-fg-muted" data-testid="workflow-map-soft-stop">{loop.softStopNote}</p>
  {/if}

  {#if loop.showOutcomeStubs}
    <div class="run-map-loop-fork"></div>
    <!--
      The stubs are ghosts and render like every other ghost (§2): bare quiet
      text under the fork's two branches, no boxes. Two full-width dashed
      bars made a footnote about the future the biggest elements on the map.
      Each stub sits centered under its branch (the fork spans 25%–75%, so
      the branch tips are at 25% and 75% — two half-width columns center
      the stubs on exactly those tips).
    -->
    <div class="flex w-full items-baseline" data-testid="workflow-map-decision-stubs">
      {#each [`↺ issues → wave ${loop.lapCount + 1}`, '✓ clean → done'] as stub (stub)}
        <span
          class="min-w-0 flex-1 break-words px-2 text-center text-[0.6875rem] text-fg-hint"
          data-testid="workflow-map-decision-stub"
        >
          {stub}
        </span>
      {/each}
    </div>
  {:else if loop.decided !== null}
    <span class="text-[0.6875rem] text-fg-muted">
      {loop.decided === 'loop' ? `↺ looped → wave ${loop.lapCount + 1}` : '✓ clean → done'}
    </span>
  {/if}

</div>
