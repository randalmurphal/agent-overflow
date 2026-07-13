<script lang="ts">
  import type { WorkItem } from '../../types/workflow';
  import { WorkflowRemoveQueuedItem, WorkflowReorderQueue } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { workflowActionConfirmationKey } from '../../stores/workflowActions';
  import {
    getWorkflowArmedAction,
    loadWorkflowOverview,
    reconcileWorkflowQueueOrder,
    setWorkflowArmedAction,
  } from '../../stores/workflowsPane.svelte';
  import { refreshWorkflowsSidebar } from '../../stores/workflowsSidebar.svelte';

  interface Props {
    queued: WorkItem[];
    costs: Readonly<Record<string, number>>;
    queueActive: boolean;
    viewOnly: boolean;
    projectName: (projectId: string) => string;
    onOpenRun: (item: WorkItem) => void;
  }

  let { queued, costs, queueActive, viewOnly, projectName, onOpenRun }: Props = $props();
  let draggedId: string | null = $state(null);
  let dropTarget: string | 'after-last' | null = $state(null);
  let reorderInFlight = $state(false);
  let cancelling = $state(new Set<string>());
  let armed = $derived(getWorkflowArmedAction());

  function finishDrag(): void {
    draggedId = null;
    dropTarget = null;
  }

  async function reorder(projectId: string, beforeId: string | null): Promise<void> {
    if (viewOnly || reorderInFlight || !draggedId) return;
    const projectItems = queued.filter((item) => item.projectId === projectId);
    const from = projectItems.findIndex((item) => item.id === draggedId);
    if (from < 0) {
      finishDrag();
      return;
    }
    const ordered = projectItems.slice();
    const [moved] = ordered.splice(from, 1);
    const target = beforeId === null ? ordered.length : ordered.findIndex((item) => item.id === beforeId);
    if (target < 0) {
      finishDrag();
      return;
    }
    ordered.splice(target, 0, moved);
    const ids = ordered.map((item) => item.id);
    if (ids.every((id, index) => id === projectItems[index]?.id)) {
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
    const dragged = queued.find((item) => item.id === draggedId);
    if (!dragged || dragged.id === target.id) {
      finishDrag();
      return;
    }
    if (dragged.projectId !== target.projectId) {
      addToast('info', 'Queue order is per project — move runs within the same project');
      finishDrag();
      return;
    }
    void reorder(target.projectId, target.id);
  }

  function dropAfterLast(): void {
    const dragged = queued.find((item) => item.id === draggedId);
    if (!dragged) {
      finishDrag();
      return;
    }
    void reorder(dragged.projectId, null);
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
      addToast('info', 'Removed from queue — record kept, nothing was provisioned');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not remove the queued run.'));
    } finally {
      const next = new Set(cancelling);
      next.delete(item.id);
      cancelling = next;
    }
  }
</script>

<section class="space-y-2" data-testid="wf-up-next">
  <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Up next · {queued.length}</h2>
  {#each queued as item, index (item.id)}
    <div
      role="listitem"
      draggable={!viewOnly && !reorderInFlight}
      ondragstart={() => { if (!viewOnly && !reorderInFlight) draggedId = item.id; }}
      ondragend={finishDrag}
      ondragover={(event) => { if (!viewOnly && !reorderInFlight) { event.preventDefault(); dropTarget = item.id; } }}
      ondrop={() => dropBefore(item)}
      class={["group flex items-center gap-2 rounded-md border px-2.5 py-2 hover:bg-surface-2/50", dropTarget === item.id ? 'border-accent bg-accent/5' : 'border-border-subtle'].join(' ')}
      data-testid="wf-queue-row"
    >
      {#if !viewOnly}<span class="cursor-grab text-fg-muted opacity-0 group-hover:opacity-100" data-testid="wf-queue-grip">⠿</span>{/if}
      <span class="w-6 text-xs text-fg-muted">#{index + 1}</span>
      <button class="min-w-0 flex-1 truncate text-left text-sm" onclick={() => onOpenRun(item)} data-testid="wf-queue-open">{item.goal}</button>
      <span class="text-xs text-fg-muted">{projectName(item.projectId)} · {item.workflowId}{queueActive ? '' : ' · held'} · ${(costs[item.id] ?? 0).toFixed(2)}</span>
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
