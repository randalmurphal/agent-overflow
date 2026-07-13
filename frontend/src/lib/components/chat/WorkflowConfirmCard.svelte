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
    state?: 'pending' | 'queued' | 'dismissed';
    disabled?: boolean;
    busy?: boolean;
  }
  let {
    projectName, title, workflowName, baseBranch, prefill, onQueue, onEdit, onDismiss,
    state = 'pending', disabled = false, busy = false,
  }: Props = $props();
</script>

<article class="rounded-lg border border-border-subtle bg-surface-1 p-3" data-testid="wf-confirm-card">
  <h3 class="text-sm font-semibold">{state === 'pending' ? 'Queue this run?' : state === 'queued' ? 'Queued' : 'Dismissed'}</h3>
  <p class="mt-1 truncate text-xs text-fg-muted">● {projectName} · {title} · {workflowName} · {baseBranch}</p>
  {#if state === 'pending'}
    <div class="mt-3 flex gap-2">
      <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white disabled:cursor-not-allowed disabled:opacity-50" onclick={() => onQueue(prefill)} disabled={disabled || busy} title={disabled ? 'Local only' : undefined} data-testid="wf-confirm-queue">Queue it</button>
      <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={() => onEdit(prefill)} disabled={disabled || busy} title={disabled ? 'Local only' : undefined} data-testid="wf-confirm-edit">Edit</button>
      <button class="rounded-md px-3 py-1.5 text-xs text-fg-muted disabled:cursor-not-allowed disabled:opacity-50" onclick={onDismiss} disabled={disabled || busy} title={disabled ? 'Local only' : undefined} data-testid="wf-confirm-dismiss">Dismiss</button>
    </div>
  {:else}
    <p class="mt-2 text-xs text-fg-muted" data-testid="wf-confirm-receipt">
      {state === 'queued' ? 'Added to Up next.' : 'No run was queued.'}
    </p>
  {/if}
</article>
