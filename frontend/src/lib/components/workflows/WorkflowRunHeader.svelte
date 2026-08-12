<script lang="ts">
  // Run-detail header block (UI-SPEC §4.1). Three rows:
  //   1. project chip · state word · sweep counter when parked
  //   2. the run title
  //   3. hint — `workflow · wave 3 · implement · parked 7h · $3.10`, plus
  //      `spawned by <automation> · <trigger>` on an automation run and
  //      `→ <thread>` on a bound one.
  //
  // R1: the state word carries the only colour on this block, and only for
  // needs-human (amber) or failed (red).

  import type { WorkItem } from '../../types/workflow';
  import { formatWorkflowCost, workflowAge, workflowMetaLine } from '../../stores/workflowData';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';
  import { runMapPosition } from '../../utils/workflowRunMap';
  import { attachWorkflowRunMap, peekWorkflowRunMap } from '../../stores/workflowRunMap.svelte';
  import { getWorkflowAutomations } from '../../stores/workflowRuns.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import { openWorkflowThreadById } from '../../stores/workflowThreads';
  import { getThreadById } from '../../stores/threads.svelte';
  import { workflowSweepCounter } from '../../stores/workflowSweep';

  interface Props {
    item: WorkItem;
    costUsd: number;
  }
  let { item, costUsd }: Props = $props();

  // This header READS the run map, so it HOLDS the run map. Attach is
  // refcounted, so sharing the key with the map mounted below costs one RPC
  // between them and the last release still tears the entry down — whereas a
  // peek at a key nobody here attached is a label that works only for as long
  // as some sibling happens to be mounted, and silently reverts to the stale
  // SQL ordinal the moment one is reordered, lazily loaded, or laid out away.
  // Keyed on the entity id alone (getter-ctx rule) — and on a `$derived` of it
  // rather than on `item.id` read inside the effect. The list cache patches a
  // run by minting a NEW row object (`patchWorkflowItems`), so the prop changes
  // on every transition of the run being read; an effect that tracked the
  // object released and re-attached each time, and an attach landing on a key
  // in retry backoff resets the curve and re-sources immediately — so a failing
  // fetch never backed off for exactly as long as the run kept moving. A string
  // derived stops propagating when the id holds still (the `nowKey` pattern).
  let mapKey = $derived(item.id);
  $effect(() => {
    const attachment = attachWorkflowRunMap(mapKey);
    return () => attachment.release();
  });

  let signal = $derived(workflowRunSignal(item.state, item.reason));
  let project = $derived(getProject(item.projectId)?.project);
  let counter = $derived(workflowSweepCounter(item.id));
  let boundThreadId = $derived(item.originThreadId ?? '');
  // The bound thread's title when the registry happens to hold it; workflow
  // threads are excluded from ListThreads by mode, so a bound chat thread
  // resolves and a bound workflow thread falls back to the neutral word.
  let boundThreadTitle = $derived(boundThreadId ? (getThreadById(boundThreadId)?.title || 'thread') : '');

  let automation = $derived(
    item.source === 'automation' && item.sourceRef
      ? getWorkflowAutomations(item.projectId).find((entry) => entry.id === item.sourceRef)
      : undefined,
  );

  // Where the run actually IS (RUN-MAP §11.4). `phase N/M` is a frozen SQL
  // ordinal joined by started_at with an alphabetical tiebreak, so a retried or
  // looped run reads a lower phase than it is on, and a campaign root reads its
  // own first phase forever while its waves run. The map view knows the answer,
  // so the position comes off the frontier once the attachment above has one;
  // the SQL counter is what the first render, before any answer, falls back to.
  //
  // The wave part plus the deepest part, never the whole breadcrumb: this is
  // one short label on a metadata line, and the map's own frontier strip is
  // where the full path is read. A live run with no frontier is a run that
  // has finished — the state word already says so, and there is no position
  // left to name.
  //
  // `runMapPosition` rather than the whole model: this header needs one label,
  // and building waves, summaries, segments and a loop foot to read two path
  // parts made the map's projection run twice per surface. It is also free of
  // any clock, so this derived reruns when the store applies — an event patch
  // or a refetch — and never once a second.
  let position = $derived.by(() => {
    const view = peekWorkflowRunMap(item.id);
    if (view === null) return item.phaseCount ? `phase ${item.currentPhaseOrdinal}/${item.phaseCount}` : '';
    const where = runMapPosition(view);
    return where === null ? '' : workflowMetaLine([where.wave, where.leaf]);
  });

  let hint = $derived(workflowMetaLine([
    item.workflowId,
    position,
    item.state === 'running'
      ? workflowAge(item.startedAt || item.createdAt)
      : `parked ${workflowAge(item.endedAt || item.startedAt || item.createdAt)}`,
    formatWorkflowCost(costUsd),
    automation ? `spawned by ${automation.name}${automation.triggerSummary ? ` · ${automation.triggerSummary}` : ''}` : '',
  ]));
</script>

<header class="space-y-1 px-4 pb-2 pt-4" data-testid="workflow-run-header">
  <div class="flex min-w-0 items-center gap-2 text-xs">
    <span class="shrink-0 truncate text-fg-muted" data-testid="workflow-run-project">{project?.name || item.projectId}</span>
    {#if signal.label}
      <span class="shrink-0 text-fg-subtle">·</span>
      <span class={['shrink-0 font-medium', signal.tone].join(' ')} data-testid="workflow-run-state">{signal.label}</span>
    {/if}
    {#if counter}
      <span class="ml-auto shrink-0 tabular-nums text-fg-muted" data-testid="workflow-sweep-counter">
        {counter.position} of {counter.total}
      </span>
    {/if}
  </div>

  <h2 class="text-base font-semibold text-fg" data-testid="workflow-run-title">{item.goal || item.workflowId}</h2>

  <p class="text-[0.6875rem] text-fg-muted" data-testid="workflow-run-hint">
    {hint}{#if boundThreadId}<span class="px-1.5 text-fg-subtle">·</span><button
        class="text-fg-muted underline-offset-2 hover:text-fg hover:underline"
        onclick={() => { void openWorkflowThreadById(boundThreadId); }}
        data-testid="workflow-run-bound-thread"
      >→ {boundThreadTitle}</button>{/if}
  </p>
</header>
