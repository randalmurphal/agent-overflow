<script lang="ts">
  import type { WorkflowIntakePrefill } from '../../stores/workflowsPane.svelte';
  interface Props {
    projectName: string;
    title: string;
    workflowName: string;
    baseBranch: string;
    prefill: WorkflowIntakePrefill;
    onQueue: (prefill: WorkflowIntakePrefill) => void;
    onEdit: (prefill: WorkflowIntakePrefill) => void;
    onDismiss: () => void;
  }
  let { projectName, title, workflowName, baseBranch, prefill, onQueue, onEdit, onDismiss }: Props = $props();
</script>

<article class="rounded-lg border border-border-subtle bg-surface-1 p-3" data-testid="wf-confirm-card">
  <h3 class="text-sm font-semibold">Queue this run?</h3>
  <p class="mt-1 truncate text-xs text-fg-muted">● {projectName} · {title} · {workflowName} · {baseBranch}</p>
  <div class="mt-3 flex gap-2">
    <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" onclick={() => onQueue(prefill)} data-testid="wf-confirm-queue">Queue it</button>
    <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => onEdit(prefill)} data-testid="wf-confirm-edit">Edit</button>
    <button class="rounded-md px-3 py-1.5 text-xs text-fg-muted" onclick={onDismiss} data-testid="wf-confirm-dismiss">Dismiss</button>
  </div>
</article>
