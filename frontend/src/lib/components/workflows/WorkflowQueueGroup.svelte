<script lang="ts">
  import type { WorkItem } from '../../types/workflow';
  import { WorkflowRemoveQueuedItem, WorkflowReorderQueue, WorkflowUpdateProjectQueue } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { workflowActionConfirmationKey } from '../../stores/workflowActions';
  import {
    getWorkflowArmedAction,
    loadWorkflowOverview,
    reconcileWorkflowProjectQueue,
    reconcileWorkflowQueueOrder,
    setWorkflowArmedAction,
  } from '../../stores/workflowsPane.svelte';
  import { refreshWorkflowsSidebar } from '../../stores/workflowsSidebar.svelte';
  import { workflowAge, workflowQueuedRanks } from '../../stores/workflowData';

  interface Props {
    projectId: string;
    projectName: string;
    projectColor: string;
    queued: WorkItem[];
    globalActive: boolean;
    globalConcurrency: number;
    paused: boolean;
    concurrency: number;
    runningCount: number;
    viewOnly: boolean;
    workflowName: (item: WorkItem) => string;
    onOpenRun: (item: WorkItem) => void;
  }

  let {
    projectId, projectName, projectColor, queued, globalActive, globalConcurrency,
    paused, concurrency, runningCount, viewOnly, workflowName, onOpenRun,
  }: Props = $props();
  let draggedId: string | null = $state(null);
  let dropTarget: string | 'after-last' | null = $state(null);
  let reorderInFlight = $state(false);
  let settingsInFlight = $state(false);
  let cancelling = $state(new Set<string>());
  let armed = $derived(getWorkflowArmedAction());
  let queuedRanks = $derived(workflowQueuedRanks(queued));
  let effectiveCapacity = $derived(concurrency === 0
    ? globalConcurrency
    : Math.min(concurrency, globalConcurrency));

  function rowMeta(item: WorkItem): string {
    const queuedAt = item.createdAt > 0
      ? item.source === 'automation' ? `spawned ${workflowAge(item.createdAt)} ago` : `queued ${workflowAge(item.createdAt)}`
      : '';
    return [workflowName(item), queuedAt, globalActive && !paused ? '' : 'held'].filter(Boolean).join(' · ');
  }

  function finishDrag(): void {
    draggedId = null;
    dropTarget = null;
  }

  async function updateSettings(nextPaused: boolean, nextConcurrency: number): Promise<void> {
    if (viewOnly || settingsInFlight) return;
    settingsInFlight = true;
    try {
      await WorkflowUpdateProjectQueue(
        projectId,
        nextPaused === paused ? null : nextPaused,
        nextConcurrency === concurrency ? null : nextConcurrency,
      );
      reconcileWorkflowProjectQueue(projectId, nextPaused, nextConcurrency);
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not update the project queue.'));
    } finally {
      settingsInFlight = false;
    }
  }

  async function reorder(beforeId: string | null): Promise<void> {
    if (viewOnly || reorderInFlight || !draggedId) return;
    const from = queued.findIndex((item) => item.id === draggedId);
    if (from < 0) {
      finishDrag();
      return;
    }
    const ordered = queued.slice();
    const [moved] = ordered.splice(from, 1);
    const target = beforeId === null ? ordered.length : ordered.findIndex((item) => item.id === beforeId);
    if (target < 0) {
      finishDrag();
      return;
    }
    ordered.splice(target, 0, moved);
    const ids = ordered.map((item) => item.id);
    if (ids.every((id, index) => id === queued[index]?.id)) {
      finishDrag();
      return;
    }

    reorderInFlight = true;
    reconcileWorkflowQueueOrder(projectId, ids);
    finishDrag();
    try {
      await WorkflowReorderQueue(projectId, ids);
      refreshWorkflowsSidebar();
      addToast('success', 'Priority reordered — the drain picks it up immediately');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not reorder the queue.'));
      await loadWorkflowOverview();
    } finally {
      reorderInFlight = false;
    }
  }

  function dropBefore(target: WorkItem): void {
    if (!draggedId || draggedId === target.id) {
      finishDrag();
      return;
    }
    void reorder(target.id);
  }

  function dropAfterLast(): void {
    if (!draggedId) {
      finishDrag();
      return;
    }
    void reorder(null);
  }

  async function cancelQueued(item: WorkItem): Promise<void> {
    if (viewOnly || cancelling.has(item.id)) return;
    const key = workflowActionConfirmationKey('queue-cancel', item);
    if (armed !== key) {
      setWorkflowArmedAction(key);
      return;
    }
    cancelling = new Set(cancelling).add(item.id);
    try {
      await WorkflowRemoveQueuedItem(item.id);
      setWorkflowArmedAction(null);
      addToast('info', item.source === 'automation'
        ? 'Removed from queue — automation will re-propose it next cycle'
        : 'Removed from queue — record kept, nothing was provisioned');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not remove the queued run.'));
    } finally {
      const next = new Set(cancelling);
      next.delete(item.id);
      cancelling = next;
    }
  }
</script>

<section class="space-y-2 rounded-lg border border-border-subtle bg-surface-1 p-2.5" data-testid="wf-project-queue">
  <header class="flex flex-wrap items-center gap-2">
    <span class="h-2.5 w-2.5 shrink-0 rounded-full" style:background-color={projectColor} aria-hidden="true"></span>
    <h3 class="min-w-0 flex-1 truncate text-sm font-medium text-fg" data-testid="wf-project-queue-name">{projectName}</h3>
    <span class="text-xs text-fg-muted" title={`${runningCount} of ${effectiveCapacity} effective project slots in use`} data-testid="wf-project-slots">{runningCount}/{effectiveCapacity}</span>
    <button
      class="rounded border border-border-subtle px-2 py-1 text-xs hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-45"
      onclick={() => void updateSettings(!paused, concurrency)}
      disabled={viewOnly || settingsInFlight}
      title={viewOnly ? 'Local only' : undefined}
      data-testid="wf-project-queue-toggle"
    >{paused ? 'Resume' : 'Pause'}</button>
    <label class="text-xs text-fg-muted">Concurrency
      <select
        class="ml-1 rounded border border-border-subtle bg-surface-0 px-1.5 py-1 text-xs text-fg disabled:cursor-not-allowed disabled:opacity-45"
        value={concurrency}
        onchange={(event) => void updateSettings(paused, Number((event.currentTarget as HTMLSelectElement).value))}
        disabled={viewOnly || settingsInFlight}
        title={viewOnly ? 'Local only' : undefined}
        data-testid="wf-project-concurrency"
      >
        <option value={0}>Global</option>
        {#each Array.from({ length: 32 }, (_, index) => index + 1) as option}
          <option value={option}>{option}</option>
        {/each}
      </select>
    </label>
  </header>

  {#each queued as item (item.id)}
    <div
      role="listitem"
      draggable={!viewOnly && !reorderInFlight}
      ondragstart={() => { if (!viewOnly && !reorderInFlight) draggedId = item.id; }}
      ondragend={finishDrag}
      ondragover={(event) => { if (!viewOnly && !reorderInFlight && draggedId) { event.preventDefault(); dropTarget = item.id; } }}
      ondrop={() => dropBefore(item)}
      class={["group flex items-center gap-2 rounded-md border px-2.5 py-2 hover:bg-surface-2/50", dropTarget === item.id ? 'border-accent bg-accent/5' : 'border-border-subtle'].join(' ')}
      data-testid="wf-queue-row"
    >
      <span class={viewOnly ? 'cursor-not-allowed text-fg-muted opacity-40' : 'cursor-grab text-fg-muted opacity-0 group-hover:opacity-100'} title={viewOnly ? 'Local only' : undefined} aria-disabled={viewOnly} data-testid="wf-queue-grip">⠿</span>
      <span class="w-6 shrink-0 text-xs text-fg-muted">#{queuedRanks.get(item.id) ?? '–'}</span>
      <button class="min-w-0 flex-1 truncate text-left text-sm" onclick={() => onOpenRun(item)} data-testid="wf-queue-open">{item.goal}</button>
      <span class="min-w-0 max-w-[45%] truncate text-right text-xs text-fg-muted" title={rowMeta(item)}>{rowMeta(item)}</span>
      <button class="rounded px-1.5 py-1 text-xs text-error opacity-0 group-hover:opacity-100 disabled:cursor-not-allowed disabled:opacity-40" onclick={() => cancelQueued(item)} disabled={viewOnly || cancelling.has(item.id)} title={viewOnly ? 'Local only' : undefined} data-testid="wf-queue-cancel">{armed === workflowActionConfirmationKey('queue-cancel', item) ? 'cancel?' : '✕'}</button>
    </div>
  {/each}
  {#if draggedId}
    <div
      role="listitem"
      class={["rounded-md border border-dashed px-3 py-2 text-center text-xs", dropTarget === 'after-last' ? 'border-accent bg-accent/5 text-fg' : 'border-border-subtle text-fg-muted'].join(' ')}
      ondragover={(event) => { if (!reorderInFlight) { event.preventDefault(); dropTarget = 'after-last'; } }}
      ondrop={dropAfterLast}
      data-testid="wf-queue-drop-after"
    >Drop to move to the end of this project's queue</div>
  {/if}
</section>
