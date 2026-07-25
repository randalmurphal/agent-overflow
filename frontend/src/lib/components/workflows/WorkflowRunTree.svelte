<script lang="ts">
  // The run tree (UI-SPEC §4.2). Ordered phase attempts; two node kinds
  // expand: a fan-out phase to its units (the join last), and a call phase to
  // its child run inline — recursively, via this component's self-import.
  //
  // Every row with a thread is openable, which closes the overlay and mounts
  // the thread as a normal pane (R3). Child runs carry no bind or notify
  // affordances (D18): a parked child is resolved from the parent's action
  // row, which is why nothing here is actionable.

  import Self from './WorkflowRunTree.svelte';
  import type { WorkflowItemDetail } from '../../types/workflow';
  import { buildWorkflowRunTree, workflowNodeTone } from '../../utils/workflowRunTree';
  import { workflowMetaLine } from '../../stores/workflowData';
  import { workflowRunSignal } from '../../utils/workflowRunSignal';
  import { getWorkflowDetail, loadWorkflowDetail } from '../../stores/workflowRuns.svelte';

  interface Props {
    detail: WorkflowItemDetail;
    depth?: number;
    /** The unit a `needs-human(unit-failed)` park is about (§4.3). */
    highlightUnitId?: string;
    onOpenThread: (threadId: string) => void;
  }
  let { detail, depth = 0, highlightUnitId = '', onOpenThread }: Props = $props();

  let nodes = $derived(buildWorkflowRunTree(detail));
  let expanded = $state(new Set<string>());

  function toggleChild(itemId: string): void {
    const next = new Set(expanded);
    if (next.has(itemId)) next.delete(itemId);
    else {
      next.add(itemId);
      void loadWorkflowDetail(itemId);
    }
    expanded = next;
  }
</script>

<ul class="space-y-0.5" data-testid="workflow-run-tree" data-depth={depth}>
  {#each nodes as node (node.phaseId + ':' + node.attempt)}
    <li>
      <button
        class="flex w-full items-baseline gap-2 rounded px-1.5 py-1 text-left text-xs hover:bg-surface-2/50 disabled:cursor-default disabled:hover:bg-transparent"
        onclick={() => onOpenThread(node.threadId)}
        disabled={!node.threadId}
        data-testid="workflow-phase-row"
        data-phase-id={node.phaseId}
      >
        <span class={['shrink-0', workflowNodeTone(node.signal)].join(' ')}>{node.glyph}</span>
        <span class="min-w-0 flex-1 truncate text-fg">{node.label}</span>
        <span class="shrink-0 text-[0.6875rem] text-fg-muted">{node.meta}</span>
      </button>

      {#if node.units.length > 0}
        <ul class="ml-4 space-y-0.5 border-l border-border-subtle pl-2" data-testid="workflow-unit-list">
          {#each node.units as row (row.unit.unitId)}
            <li>
              <button
                class={[
                  'flex w-full items-baseline gap-2 rounded px-1.5 py-1 text-left text-xs hover:bg-surface-2/50',
                  'disabled:cursor-default disabled:hover:bg-transparent',
                  highlightUnitId && highlightUnitId === row.unit.unitId ? 'bg-error/10' : '',
                ].join(' ')}
                onclick={() => onOpenThread(row.threadId)}
                disabled={!row.threadId}
                data-testid="workflow-unit-row"
                data-unit-id={row.unit.unitId}
                data-unit-status={row.unit.status}
              >
                <span class={['shrink-0', workflowNodeTone(row.signal)].join(' ')}>{row.glyph}</span>
                <span class="min-w-0 flex-1 truncate text-fg">{row.label}</span>
                <span class="shrink-0 text-[0.6875rem] text-fg-muted">{row.meta}</span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}

      {#each node.children as child (child.itemId)}
        {@const childDetail = getWorkflowDetail(child.itemId)}
        {@const signal = workflowRunSignal(child.state, child.reason)}
        <div class="ml-4 border-l border-border-subtle pl-2" data-testid="workflow-child-run" data-child-item-id={child.itemId}>
          <button
            class="flex w-full items-baseline gap-2 rounded px-1.5 py-1 text-left text-xs hover:bg-surface-2/50"
            onclick={() => toggleChild(child.itemId)}
            aria-expanded={expanded.has(child.itemId)}
            data-testid="workflow-child-toggle"
          >
            <span class="shrink-0 text-fg-muted">{expanded.has(child.itemId) ? '▼' : '▶'}</span>
            <span class="min-w-0 flex-1 truncate text-fg">{child.workflowId}</span>
            <span class={['shrink-0 text-[0.6875rem]', signal.tone].join(' ')}>
              {workflowMetaLine([
                signal.label || child.state,
                child.phaseCount > 0 ? `phase ${child.currentPhaseOrdinal}/${child.phaseCount}` : '',
                depth >= 1 ? `↳ depth ${depth + 1}` : '',
              ])}
            </span>
          </button>
          {#if expanded.has(child.itemId)}
            {#if childDetail}
              <Self detail={childDetail} depth={depth + 1} {onOpenThread} />
            {:else}
              <p class="px-1.5 py-1 text-[0.6875rem] text-fg-muted">Loading…</p>
            {/if}
          {/if}
        </div>
      {/each}
    </li>
  {/each}
</ul>
