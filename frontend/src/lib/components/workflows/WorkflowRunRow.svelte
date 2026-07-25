<script lang="ts">
  // One run row on home (UI-SPEC §3.2). R1: a row shows at most one signal —
  // the amber dot + state word for a run that needs a human, red for failed,
  // nothing at all for running / done / cancelled. Meta is `·`-separated and
  // truncates end-first.

  import { workflowAge, formatWorkflowCost, workflowMetaLine } from '../../stores/workflowData';
  import { parseWorkflowDisposition } from '../../types/workflow';
  import type { WorkItem } from '../../types/workflow';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';
  import { getWorkflowCosts, getWorkflowLivePhase } from '../../stores/workflowRuns.svelte';

  interface Props {
    run: WorkItem;
    onOpen: (itemId: string) => void;
  }
  let { run, onOpen }: Props = $props();

  let signal = $derived(workflowRunSignal(run.state, run.reason));
  let cost = $derived(formatWorkflowCost(getWorkflowCosts()[run.id]));
  let disposition = $derived(parseWorkflowDisposition(run.disposition));
  let live = $derived(getWorkflowLivePhase(run.id));

  // Ages are computed at render time. There is deliberately no per-row timer:
  // every workflow transition pushes an event that re-renders these rows, and
  // a resting age that is a minute stale reads identically ("parked 7h").
  let meta = $derived.by(() => {
    const rested = run.endedAt || run.startedAt || run.createdAt;
    if (run.state === 'running') {
      const progress = run.phaseCount ? `phase ${run.currentPhaseOrdinal}/${run.phaseCount}` : '';
      return workflowMetaLine([progress, workflowAge(run.startedAt || run.createdAt), cost]);
    }
    if (disposition) {
      const receipt = disposition.action === 'merged'
        ? 'merged'
        : disposition.action === 'pr' ? (disposition.prRef || 'pull request') : 'discarded — branch dropped, record kept';
      return workflowMetaLine([receipt, workflowAge(rested), cost]);
    }
    if (run.state === 'cancelled') {
      return workflowMetaLine(['cancelled · worktree kept', workflowAge(rested), cost]);
    }
    const parked = run.state === 'needs-human' ? `parked ${workflowAge(rested)}` : workflowAge(rested);
    return workflowMetaLine([run.currentPhaseId, parked, cost]);
  });

  let activity = $derived(
    run.state === 'running' && live?.phaseId
      ? workflowMetaLine([live.unitId || live.phaseId, live.status])
      : '',
  );
</script>

<button
  class={[
    'flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-surface-2/50',
    signal.glowClass ?? '',
  ].join(' ')}
  onclick={() => onOpen(run.id)}
  data-testid="workflow-run-row"
  data-item-id={run.id}
  data-run-state={run.state}
>
  {#if signal.signal !== 'none'}
    <span
      class={['mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full', signal.dotClass, signal.pulse ? 'animate-pulse' : ''].join(' ')}
      data-testid="workflow-run-signal"
    ></span>
  {:else}
    <span class="mt-1.5 h-1.5 w-1.5 shrink-0"></span>
  {/if}
  <span class="min-w-0 flex-1">
    <span class="flex min-w-0 items-baseline gap-2">
      {#if signal.label}
        <span class={['shrink-0 text-xs font-medium', signal.tone].join(' ')}>{signal.label}</span>
      {/if}
      <span class="min-w-0 flex-1 truncate text-sm text-fg">{run.goal || run.workflowId}</span>
      <span class="shrink-0 truncate text-[0.6875rem] text-fg-muted">{meta}</span>
    </span>
    {#if activity}
      <span class="block truncate text-[0.6875rem] italic text-fg-subtle" data-testid="workflow-run-activity">{activity}</span>
    {/if}
  </span>
</button>
