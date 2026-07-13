<script lang="ts">
  import { WorkflowOpenStudioThread, WorkflowOpenTriageAgent } from '../../stores/bindings';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import {
    applyWorkflowQueueState,
    getWorkflowProjectFilter,
    getWorkflowQueueState,
    openWorkflowIntake,
    setWorkflowProjectFilter,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import { isViewOnlySession } from '../../transport/runMode';
  import { userFacingError } from '../../utils/userFacingError';

  let filter = $derived(getWorkflowProjectFilter());
  let queue = $derived(getWorkflowQueueState());
  let projects = $derived(getProjects());
  let viewOnly = $derived(isViewOnlySession());
  let runningCount = $derived(queue.runningCount ?? 0);
  let slotCapacity = $derived(queue.slotCapacity ?? queue.globalConcurrency);

  function targetProjectId(): string | null {
    return filter ?? projects[0]?.project.id ?? null;
  }

  async function openStudio(): Promise<void> {
    if (viewOnly) return;
    const projectId = targetProjectId();
    if (!projectId) { addToast('error', 'Add a project first'); return; }
    try {
      const thread = await WorkflowOpenStudioThread(projectId, '');
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow studio.'));
    }
  }

  async function openTriage(): Promise<void> {
    if (viewOnly) return;
    const projectId = targetProjectId();
    if (!projectId) { addToast('error', 'Add a project first'); return; }
    try {
      const thread = await WorkflowOpenTriageAgent(projectId);
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow triage.'));
    }
  }

  async function toggleQueue(): Promise<void> {
    if (viewOnly) return;
    const active = !queue.active;
    await updateSetting('workflowQueueActive', active);
    if (getSettings().workflowQueueActive !== active) return;
    applyWorkflowQueueState({ ...queue, active });
    addToast('info', active
      ? 'Queue active — draining by priority'
      : 'Paused — running items finish; nothing new starts');
  }
</script>

<div class="overview-controls flex min-w-0 items-center gap-1.5" data-testid="wf-overview-controls">
  <button
    class="shrink-0 rounded-md border border-border-subtle px-2 py-1 text-[11px] font-medium hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50"
    onclick={toggleQueue}
    disabled={viewOnly}
    title={viewOnly ? 'Local only' : 'Pause stops new starts; running items finish'}
    data-testid="wf-queue-toggle"
  >{queue.active ? '❚❚ Active' : '▶ Paused'}</button>

  <div class="wide-controls min-w-0 items-center gap-1.5">
    <span
      class="inline-flex shrink-0 items-center gap-1 text-[11px] tabular-nums text-fg-muted"
      title={`${runningCount} of ${slotCapacity} concurrency slots in use`}
      data-testid="wf-slots"
    >
      <span class="flex gap-0.5" aria-hidden="true">
        {#each Array.from({ length: slotCapacity }) as _, index}<span class={index < runningCount ? 'text-fg' : 'text-fg-muted/40'}>●</span>{/each}
      </span>
      {runningCount}/{slotCapacity}
    </span>
    <select
      class="min-w-0 max-w-32 truncate rounded-md border border-border-subtle bg-surface-1 px-1.5 py-1 text-[11px]"
      value={filter ?? ''}
      onchange={(event) => setWorkflowProjectFilter((event.currentTarget as HTMLSelectElement).value || null)}
      data-testid="wf-project-filter"
    >
      <option value="">All projects</option>
      {#each projects as project}<option value={project.project.id}>{project.project.name}</option>{/each}
    </select>
    <button class="shrink-0 rounded-md bg-accent px-2 py-1 text-[11px] font-medium text-white hover:bg-accent/90 disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openWorkflowIntake()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-run">+ New run</button>
    <button class="shrink-0 rounded-md border border-border-subtle px-2 py-1 text-[11px] hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={openStudio} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-workflow">+ New workflow</button>
    <button class="shrink-0 rounded-md border border-border-subtle px-2 py-1 text-[11px] hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-50" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-triage">Triage</button>
  </div>

  <details class="overflow-control relative" data-testid="wf-overflow">
    <summary class="cursor-pointer list-none rounded-md border border-border-subtle px-2 py-1 text-[11px]" data-testid="wf-overflow-toggle">⋯</summary>
    <div class="absolute right-0 z-20 mt-1 grid min-w-48 gap-1.5 rounded-md border border-border-subtle bg-surface-1 p-2 shadow-lg">
      <span class="text-xs text-fg-muted" title={`${runningCount} of ${slotCapacity} concurrency slots in use`} data-testid="wf-slots-mobile">● {runningCount}/{slotCapacity}</span>
      <select class="rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-xs" value={filter ?? ''} onchange={(event) => setWorkflowProjectFilter((event.currentTarget as HTMLSelectElement).value || null)} data-testid="wf-project-filter-mobile">
        <option value="">All projects</option>
        {#each projects as project}<option value={project.project.id}>{project.project.name}</option>{/each}
      </select>
      <button class="rounded-md bg-accent px-2.5 py-1.5 text-left text-xs font-medium text-white disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openWorkflowIntake()} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-run-mobile">+ New run</button>
      <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-left text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={openStudio} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-new-workflow-mobile">+ New workflow</button>
      <button class="rounded-md border border-border-subtle px-2.5 py-1.5 text-left text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-triage-mobile">Triage</button>
    </div>
  </details>
</div>

<style>
  .wide-controls { display: none; }
  .overflow-control { display: block; }
  @container (min-width: 760px) {
    .wide-controls { display: flex; }
    .overflow-control { display: none; }
  }
</style>
