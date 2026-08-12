<script lang="ts">
  // One node on the spine (RUN-MAP §2, §8): a phase, a fan-out, a call, or the
  // loop foot. A node with records renders one row per attempt in place
  // (`audit`, `fix`, `audit ·2`); a node with none is a ghost row — a real
  // element from first render, so a status change is a class swap and never a
  // DOM insertion (§10).
  //
  // Composition (a called run that is not a wave) recurses through this
  // component's own self-import, and the fan borrows that recursion as a
  // snippet: a branch column's chain is the same chain a phase node draws, so
  // there is one renderer for it rather than two that drift.

  import Self from './WorkflowRunMapNode.svelte';
  import WorkflowRunMapFan from './WorkflowRunMapFan.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import { ghostAttempt } from '../../utils/workflowRunMap';
  import type {
    RunMapAttempt,
    RunMapCompositionNode,
    RunMapSegmentNode,
  } from '../../utils/workflowRunMap';
  import { runMapNodeStyle, RUN_MAP_LABEL_MAX } from '../../utils/workflowRunMapStyle';

  interface Props {
    node: RunMapSegmentNode;
    /** The follow target's key — the one row that carries the `now ▸` tag. */
    nowKey: string;
    onOpenThread: (threadId: string) => void;
    onToggleComposition: (itemId: string) => void;
  }
  let { node, nowKey, onOpenThread, onToggleComposition }: Props = $props();

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

{#snippet chainList(nodes: RunMapCompositionNode[])}
  <ul class="mt-1 ml-3 space-y-1 border-l border-border-subtle pl-2">
    {#each nodes as composition (composition.key)}
      {@const style = runMapNodeStyle(composition.signal)}
      <!--
        `data-composition-item-id`, not `data-item-id`: three surfaces already
        carry the latter with three different referents (the run detail's run,
        the map's run, a sidebar row's run), and `uiRenderTrace` walks
        `[data-item-id]` app-wide as "one timeline row". A called run inside a
        map node is none of those.
      -->
      <li data-testid="workflow-map-composition" data-composition-item-id={composition.itemId}>
        <button
          type="button"
          class="flex w-full items-baseline gap-2 rounded px-1 py-0.5 text-left text-xs hover:bg-surface-2/50 disabled:cursor-default disabled:hover:bg-transparent"
          disabled={!composition.toggleable}
          aria-expanded={!composition.collapsed}
          onclick={() => onToggleComposition(composition.itemId)}
        >
          <span class={['shrink-0', style.tone].join(' ')}>{style.spinner ? '' : style.glyph}</span>
          {#if style.spinner}
            <SteppedSpinner size={10} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
          {/if}
          <span class="min-w-0 flex-1 truncate text-fg-muted" title={composition.label}>
            {truncateMiddle(composition.label, RUN_MAP_LABEL_MAX)}
          </span>
          <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-hint">
            {[composition.collapsed ? composition.summary.label : '', composition.duration]
              .filter((part) => part !== '').join(' · ')}
          </span>
        </button>
        {#if !composition.collapsed}
          {#each composition.waves as wave (wave.key)}
            {#if composition.waves.length > 1}
              <p class="px-1 pt-1 text-[0.625rem] uppercase tracking-wider text-fg-hint">{wave.summary.label}</p>
            {/if}
            <ul class="space-y-1 pl-1">
              {#each wave.segments as segment (segment.key)}
                <Self node={segment} {nowKey} {onOpenThread} {onToggleComposition} />
              {/each}
            </ul>
          {/each}
        {/if}
      </li>
    {/each}
  </ul>
{/snippet}

<li
  class="relative"
  data-testid="workflow-map-node"
  data-phase-id={node.phaseId}
  data-node-kind={node.kind}
  data-ghost={node.ghost}
  data-signal={node.signal}
>
  {#each rows as row (row.key)}
    {@const style = runMapNodeStyle(row.signal, skipped)}
    {@const isNow = row.key === nowKey}
    <div
      class={[
        'rounded-md border px-2 py-1',
        style.border,
        style.glow,
        isNow ? 'bg-surface-1/60' : '',
      ].filter(Boolean).join(' ')}
      data-run-map-now={isNow ? 'true' : undefined}
      data-attempt-key={row.key}
    >
      <div class="flex items-baseline gap-2">
        {#if isNow}
          <span class="shrink-0 text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
        {/if}
        {#if style.spinner}
          <SteppedSpinner size={11} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
        {:else}
          <span class={['shrink-0 text-xs', style.tone].join(' ')} aria-hidden="true">{style.glyph}</span>
        {/if}
        <button
          type="button"
          class={[
            'min-w-0 flex-1 truncate text-left text-xs hover:underline disabled:cursor-default disabled:no-underline',
            style.label,
          ].join(' ')}
          disabled={!row.threadId}
          title={row.label}
          onclick={() => onOpenThread(row.threadId)}
          data-testid="workflow-map-node-label"
        >
          {truncateMiddle(row.label, RUN_MAP_LABEL_MAX)}
        </button>
        <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-hint">{metaOf(row)}</span>
      </div>

      {#if row.cause}
        <button
          type="button"
          class={[
            'mt-0.5 w-full text-left text-[0.6875rem]',
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
      <WorkflowRunMapFan fan={row.fan} {nowKey} {onOpenThread} chain={chainList} />
    {/if}
    {#if row.chain.length > 0}
      {@render chainList(row.chain)}
    {/if}
  {/each}

  {#if note}
    <p class="px-2 text-[0.625rem] text-fg-hint" data-testid="workflow-map-node-note">{note}</p>
  {/if}

  {#if node.kind === 'decision'}
    {@const loop = node.loop}
    <div class="mt-1 rounded-md border border-dashed border-border-subtle px-2 py-1" data-testid="workflow-map-decision">
      <div class="flex items-baseline gap-2 text-xs">
        <span class="min-w-0 flex-1 truncate text-fg-muted" title={loop.label}>{loop.label}</span>
        <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-hint">{loop.lapLabel}</span>
      </div>
      {#if loop.softStopNote}
        <!--
          Neutral, not amber: R1 reserves `--warning` for a human being BLOCKED.
          A standing stop request is a fact about what the loop will do next,
          and nothing is waiting on anyone.
        -->
        <p class="text-[0.6875rem] text-fg-muted" data-testid="workflow-map-soft-stop">{loop.softStopNote}</p>
      {/if}
      {#if loop.showOutcomeStubs}
        <ul class="mt-0.5 space-y-0.5 text-[0.6875rem] text-fg-hint">
          <li>issues → wave {loop.lapCount + 1}</li>
          <li>clean → done</li>
        </ul>
      {:else if loop.decided !== null}
        <p class="mt-0.5 text-[0.6875rem] text-fg-muted">
          {loop.decided === 'loop' ? `looped → wave ${loop.lapCount + 1}` : 'clean → done'}
        </p>
      {/if}
    </div>
  {/if}
</li>
