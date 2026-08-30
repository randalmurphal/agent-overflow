<script lang="ts">
  // One node on the spine (RUN-MAP §2, §8): a phase, a fan-out, a call, or the
  // loop foot. A node with records renders one row per attempt in place
  // (`audit`, `fix`, `audit ·2`); a node with none is a ghost row — a real
  // element from first render, so a status change is a class swap and never a
  // DOM insertion (§10).
  //
  // Every row is an INTRINSIC-width box centered on the spine, not a full-width
  // bar: the map is a flow, and a column of edge-to-edge bars reads as a list
  // whatever the glyphs say. The connectors between the boxes are pseudo-
  // elements (`app.css`, `.run-map-*`), so each box stays an ordinary
  // block-level child of the scroller's row flow and §9.7's anchor descent can
  // still find it.
  //
  // Recursion goes through SNIPPETS rather than imports. A called run renders
  // through `WorkflowRunMapComposition`, which needs to draw its laps' segments
  // — which are these nodes. Passing `segmentList` down keeps that a one-way
  // import instead of a cycle, and the fan borrows the same `chainList` so a
  // lane's chain and a phase's chain are drawn by one renderer rather than two
  // that drift.

  import Self from './WorkflowRunMapNode.svelte';
  import WorkflowRunMapComposition from './WorkflowRunMapComposition.svelte';
  import WorkflowRunMapFan from './WorkflowRunMapFan.svelte';
  import WorkflowRunMapLoopFoot from './WorkflowRunMapLoopFoot.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import { ghostAttempt } from '../../utils/workflowRunMap';
  import type {
    RunMapAttempt,
    RunMapCompositionNode,
    RunMapSegmentNode,
  } from '../../utils/workflowRunMap';
  import {
    runMapNodeStyle,
    RUN_MAP_GHOST_ROW,
    RUN_MAP_LABEL_MAX,
    RUN_MAP_NODE_BOX,
  } from '../../utils/workflowRunMapStyle';

  interface Props {
    node: RunMapSegmentNode;
    /** The follow target's key — the one row that carries the `now ▸` tag. */
    nowKey: string;
    onOpenThread: (threadId: string) => void;
    onToggleWave: (waveItemId: string) => void;
    onToggleComposition: (itemId: string) => void;
    onToggleLane: (branchKey: string) => void;
  }
  let { node, nowKey, onOpenThread, onToggleWave, onToggleComposition, onToggleLane }: Props = $props();

  // A ghost node has no attempt to render from, so the node supplies one — a
  // real `RunMapAttempt` rather than a row shape of this component's own, so
  // there is one thing a row IS and the two cannot drift.
  let rows = $derived<RunMapAttempt[]>(node.attempts.length > 0 ? node.attempts : [ghostAttempt(node)]);
  // `skipped` is a fact about the NODE (§5.5, declared and never recorded), so
  // it can only ever describe the ghost row: a node with attempts ran.
  let skipped = $derived(node.attempts.length === 0 && node.skipped);

  // Node-level, so they render ONCE beneath the attempts rather than on each:
  // "skipped" is the past, not the future (§5.5) — position already says it
  // will not happen — and a record whose phase left the definition on a rerun
  // is appended with a note, never dropped (§5.1). A check phase gets no note
  // at all: §7 says checks render by normal node rules.
  let note = $derived([
    node.skipped ? 'skipped — the run looped back past it' : '',
    node.notInDefinition ? 'not in current definition' : '',
  ].filter((part) => part !== '').join(' · '));

  // Per-visit and in-memory by design: which causes the reader expanded is a
  // fact about this look at this node, not about the run — deliberately not
  // lifted into the overlay store beside wave/composition expansion, which
  // survives a remount because navigating away and back is not a decision to
  // re-fold. A cause is one click to reopen.
  let expandedCauses = $state(new Set<string>());

  function toggleCause(key: string): void {
    const next = new Set(expandedCauses);
    if (!next.delete(key)) next.add(key);
    expandedCauses = next;
  }

  function metaOf(row: RunMapAttempt): string {
    return [
      row.touched ? `touched by hand${row.interventionKind ? ` — ${row.interventionKind}` : ''}` : '',
      row.duration,
    ].filter((part) => part !== '').join(' · ');
  }
</script>

{#snippet segmentList(nodes: RunMapSegmentNode[])}
  <ul class="run-map-spine run-map-spine-wide w-full">
    {#each nodes as segment (segment.key)}
      <Self node={segment} {nowKey} {onOpenThread} {onToggleWave} {onToggleComposition} {onToggleLane} />
    {/each}
  </ul>
{/snippet}

{#snippet chainList(nodes: RunMapCompositionNode[])}
  {#each nodes as composition (composition.key)}
    <WorkflowRunMapComposition
      {composition}
      {onToggleWave}
      {onToggleComposition}
      segments={segmentList}
    />
  {/each}
{/snippet}

<li
  class="run-map-node run-map-spine"
  data-testid="workflow-map-node"
  data-phase-id={node.phaseId}
  data-node-kind={node.kind}
  data-ghost={node.ghost}
  data-signal={node.signal}
>
  {#each rows as row (row.key)}
    {@const style = runMapNodeStyle(row.signal, skipped)}
    {@const isNow = row.key === nowKey}
    {@const boxed = row.signal !== 'ghost'}
    <!--
      A record is a BOX — a quiet fill for what happened, fill + border for
      what is live or wrong — and a ghost is a bare line on the spine (§2):
      surface encodes reality, so border style stops being the only signal.
      The `now ▸` row swaps its fill for the accent tint: position is the
      surface's one accent, and it should be findable at a glance.
    -->
    <div
      class={[
        // run-map-node: spine children need a class feature for the
        // connector selector (see the app.css `.run-map-spine` comment).
        'run-map-node',
        boxed ? RUN_MAP_NODE_BOX : RUN_MAP_GHOST_ROW,
        boxed ? `${style.border} ${style.glow}` : '',
        isNow ? 'rounded-md bg-accent/10' : boxed ? style.fill : '',
      ].filter(Boolean).join(' ')}
      data-run-map-now={isNow ? 'true' : undefined}
      data-attempt-key={row.key}
    >
      <!--
        ONE button holding glyph, label and meta as inline content: the row
        reads as a sentence that wraps mid-text wherever it must. Anything
        atomic beside the label (a separate glyph box, a separate button)
        wraps as a UNIT when space runs out, stranding a lone `·` or spinner
        on the first line — which reads as a rendering bug. The glyph and
        meta are `inline-block` so the hover underline stays on the words.
      -->
      <button
        type="button"
        class="max-w-full break-words text-left hover:underline disabled:cursor-default disabled:no-underline"
        disabled={!row.threadId}
        title={row.label}
        onclick={() => onOpenThread(row.threadId)}
        data-testid="workflow-map-node-label"
      >
        {#if isNow}
          <span class="mr-1 inline-block text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
        {/if}
        {#if style.spinner}
          <SteppedSpinner size={11} class="mr-1 inline-block align-middle" animate={!getSettings().lowPowerMode} />
        {:else}
          <span class={['mr-1 inline-block', style.glyphTone].join(' ')} aria-hidden="true">{style.glyph}</span>
        {/if}
        <!-- Labels WRAP (§2): a phase name is the node's whole meaning, and a
             map of `Implement …` rows says nothing. `RUN_MAP_LABEL_MAX` is
             the runaway guard, not a line budget. -->
        <span class={style.label}>{truncateMiddle(row.label, RUN_MAP_LABEL_MAX)}</span>
        {#if metaOf(row)}
          <span class="ml-1 inline-block text-[0.6875rem] tabular-nums text-fg-hint">{metaOf(row)}</span>
        {/if}
      </button>

      {#if row.cause}
        <button
          type="button"
          class={[
            'block w-full text-left text-[0.6875rem]',
            expandedCauses.has(row.key) ? '' : 'line-clamp-2',
            style.tone,
          ].filter(Boolean).join(' ')}
          aria-expanded={expandedCauses.has(row.key)}
          onclick={() => toggleCause(row.key)}
          data-testid="workflow-map-node-cause"
        >
          {row.cause}
        </button>
      {/if}
    </div>

    {#if row.fan}
      <WorkflowRunMapFan fan={row.fan} {nowKey} {onOpenThread} {onToggleLane} chain={chainList} />
    {/if}
    {#if row.chain.length > 0}
      {@render chainList(row.chain)}
    {/if}
  {/each}

  {#if note}
    <p class="run-map-node text-[0.625rem] text-fg-hint" data-testid="workflow-map-node-note">{note}</p>
  {/if}

  {#if node.kind === 'decision'}
    <WorkflowRunMapLoopFoot loop={node.loop} />
  {/if}
</li>
