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
  import { workflowAge, workflowQueuedRanks } from '../../stores/workflowData';

  interface Props {
    queued: WorkItem[];
    queueActive: boolean;
    viewOnly: boolean;
    projectColor: (projectId: string) => string;
    workflowName: (item: WorkItem) => string;
    onOpenRun: (item: WorkItem) => void;
  }

  let { queued, queueActive, viewOnly, projectColor, workflowName, onOpenRun }: Props = $props();
  let draggedId: string | null = $state(null);
  let dropTarget: string | 'after-last' | null = $state(null);
  let reorderInFlight = $state(false);
  let cancelling = $state(new Set<string>());
  let armed = $derived(getWorkflowArmedAction());
  let queuedRanks = $derived(workflowQueuedRanks(queued));

  function rowMeta(item: WorkItem): string {
    const queuedAt = item.createdAt > 0
      ? item.source === 'automation' ? `spawned ${workflowAge(item.createdAt)} ago` : `queued ${workflowAge(item.createdAt)}`
      : '';
    return [workflowName(item), queuedAt, queueActive ? '' : 'held'].filter(Boolean).join(' · ');
  }

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

<section class="space-y-2" data-testid="wf-up-next">
  <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Up next · {queued.length}</h2>
  {#each queued as item (item.id)}
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
      <span
        class={viewOnly
          ? 'cursor-not-allowed text-fg-muted opacity-40'
          : 'cursor-grab text-fg-muted opacity-0 group-hover:opacity-100'}
        title={viewOnly ? 'Local only' : undefined}
        aria-disabled={viewOnly}
        data-testid="wf-queue-grip"
      >⠿</span>
      <span class="w-6 shrink-0 text-xs text-fg-muted">#{queuedRanks.get(item.id) ?? '–'}</span>
      <span class="h-2 w-2 shrink-0 rounded-full" style:background-color={projectColor(item.projectId)} aria-hidden="true"></span>
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
