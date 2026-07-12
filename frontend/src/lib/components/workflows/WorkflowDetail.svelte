<script lang="ts">
  import type { WorkItem, WorkflowPaneLevel } from '../../types/workflow';
  import { parseWorkflowDisposition } from '../../types/workflow';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';
  import { WorkflowCancelItem, WorkflowOpenStudioThread } from '../../stores/bindings';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    getWorkflowArmedAction,
    getWorkflowCosts,
    getWorkflowDefinitions,
    getWorkflowItems,
    pushWorkflowLevel,
    setWorkflowArmedAction,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import WorkflowJobNotes from './WorkflowJobNotes.svelte';

  interface Props { level: Extract<WorkflowPaneLevel, { kind: 'workflow' }> }
  let { level }: Props = $props();
  let allItems = $derived(getWorkflowItems());
  let items = $derived(allItems.filter((item) => item.projectId === level.projectId && item.workflowId === level.workflowId));
  let live = $derived(items.filter((item) => !['done', 'cancelled'].includes(item.state)));
  let history = $derived(items.filter((item) => ['done', 'cancelled'].includes(item.state)).sort((a, b) => (b.endedAt || b.createdAt) - (a.endedAt || a.createdAt)));
  let costs = $derived(getWorkflowCosts());
  let definition = $derived(getWorkflowDefinitions().find((entry) => entry.projectId === level.projectId && entry.definition.id === level.workflowId));
  let project = $derived(getProjects().find((entry) => entry.project.id === level.projectId)?.project);
  let armed = $derived(getWorkflowArmedAction());
  const hasAutomation = false;

  function openRun(item: WorkItem): void {
    pushWorkflowLevel({
      kind: 'run', projectId: level.projectId, workflowId: level.workflowId,
      workflowLabel: level.label, itemId: item.id, label: item.goal, sweep: false,
    });
  }

  function meta(item: WorkItem): string {
    const parts = [item.state];
    if (item.reason) parts.push(item.reason);
    if (item.state === 'queued') parts.push(`#${item.sortPosition + 1}`);
    if (costs[item.id]) parts.push(`$${costs[item.id].toFixed(2)}`);
    return parts.join(' · ');
  }

  function age(timestamp: number): string {
    const hours = Math.max(0, Math.round((Date.now() - timestamp) / 3_600_000));
    if (hours < 1) return 'just now';
    if (hours < 24) return `${hours}h`;
    if (hours < 48) return 'yesterday';
    return `${Math.round(hours / 24)}d`;
  }

  function historyMeta(item: WorkItem): string {
    const disposition = parseWorkflowDisposition(item.disposition);
    let receipt = item.state;
    if (disposition?.action === 'merged') receipt = 'merged';
    else if (disposition?.action === 'pr') receipt = disposition.prRef ? `PR ${disposition.prRef}` : 'PR created';
    else if (disposition?.action === 'discarded') receipt = 'discarded';
    else if (item.state === 'done') receipt = 'done · to dispose';
    else if (item.state === 'cancelled') receipt = `cancelled · worktree ${item.worktreePath ? 'kept' : 'not created'}`;
    const parts = [receipt, age(item.endedAt || item.createdAt)];
    if (costs[item.id] !== undefined) parts.push(`$${costs[item.id].toFixed(2)}`);
    return parts.join(' · ');
  }

  async function editWorkflow(): Promise<void> {
    try {
      const thread = await WorkflowOpenStudioThread(level.projectId, level.workflowId);
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow studio.'));
    }
  }

  async function cancelRun(item: WorkItem, event: MouseEvent): Promise<void> {
    event.stopPropagation();
    const key = `cancel:${item.id}`;
    if (armed !== key) { setWorkflowArmedAction(key); return; }
    try {
      await WorkflowCancelItem(item.id);
      setWorkflowArmedAction(null);
      addToast('info', 'Teardown — turn stopped, locks released, worktree kept');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not stop the run.'));
    }
  }
</script>

<div class="space-y-5 p-4" data-testid="wf-workflow-detail">
  <div class="flex items-start gap-3">
    <div class="min-w-0 flex-1">
      <p class="truncate text-xs text-fg-muted">{project?.name ?? level.projectId} · {definition?.definition.scope ?? 'workflow'} · {definition?.definition.phaseCount ?? 0} phases · {definition?.definition.phases.map((phase) => phase.id).join(' → ')}</p>
    </div>
    <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs hover:bg-surface-2" onclick={editWorkflow} data-testid="wf-edit-workflow">Edit</button>
  </div>

  <section class="space-y-2" data-testid="wf-live-runs">
    <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Runs · {live.length}</h2>
    {#if live.length === 0}
      <p class="py-2 text-xs text-fg-muted" data-testid="wf-no-live-runs">no live runs</p>
    {/if}
    {#each live as item (item.id)}
      {@const signal = workflowRunSignal(item.state, item.reason)}
      <div class={["group flex items-center gap-2 rounded-lg border border-border-subtle px-3 py-2.5 hover:bg-surface-2/50", signal.glowClass ?? ''].join(' ')} data-testid="wf-run-row">
        {#if signal.signal !== 'none'}
          <span class={`h-2 w-2 rounded-full ${signal.dotClass} ${signal.pulse ? 'animate-pulse' : ''}`}></span>
          <span class={`text-xs font-medium ${signal.tone}`}>{signal.label}</span>
        {/if}
        <button class="min-w-0 flex-1 truncate text-left text-sm font-medium" onclick={() => openRun(item)} data-testid="wf-run-open">{item.goal}</button>
        <span class="shrink-0 text-xs text-fg-muted">{meta(item)}</span>
        {#if item.state === 'running'}
          <button class="rounded px-1.5 py-1 text-xs text-error opacity-0 group-hover:opacity-100" onclick={(event) => cancelRun(item, event)} data-testid="wf-run-cancel">{armed === `cancel:${item.id}` ? 'stop this run?' : '✕'}</button>
        {/if}
      </div>
    {/each}
  </section>

  <section class="space-y-2" data-testid="wf-history">
    <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">History · {history.length}</h2>
    {#if history.length === 0}<p class="py-2 text-xs text-fg-muted">No history yet.</p>{/if}
    {#each history as item (item.id)}
      <button class="flex w-full items-center gap-2 rounded-md border border-border-subtle px-3 py-2 text-left hover:bg-surface-2/50" onclick={() => openRun(item)} data-testid="wf-history-row">
        <span class="min-w-0 flex-1 truncate text-sm">{item.goal}</span>
        <span class="text-xs text-fg-muted">{historyMeta(item)}</span>
      </button>
    {/each}
  </section>

  {#if hasAutomation}
    <WorkflowJobNotes automationId="" />
  {/if}
</div>
