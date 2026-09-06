<script lang="ts">
  import { workflowItemHasScope } from '../../transport/entityScopes';
  // Discard — preview is consent (UI-SPEC §4.5, D23). Nothing is destroyed
  // until this dialog has shown exactly what would be: one row per worktree in
  // the run's tree (the run's own, its units' sub-worktrees, its children's),
  // each with its branch, dirty files and unmerged commits.
  //
  // A worktree that could not be inspected says so rather than being omitted —
  // a silent gap in a loss preview is the one thing this surface cannot do.

  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { WorkflowDiscardPreview } from '../../types/workflow';
  import { worktreeLossSummary } from '../../utils/workflowLoss';
  import { WorkflowDiscardPreview as fetchDiscardPreview } from '../../stores/bindings';
  import { getWorkflowCosts, getWorkflowDetail, getWorkflowRun } from '../../stores/workflowRuns.svelte';
  import { resolveWorkflowRun } from '../../stores/workflowResolve';
  import { userFacingError } from '../../utils/userFacingError';

  interface Props {
    open: boolean;
    itemId: string;
    onClose: () => void;
  }
  let { open, itemId, onClose }: Props = $props();

  // Every control here drives the workflow engine, which is `threads:autonomy`.
  let ungranted = $derived(!workflowItemHasScope('threads:autonomy', itemId));
  let item = $derived(getWorkflowRun(itemId));
  // Composed spend (see WorkflowRunDetail): `usage.costUsd` is wire-reported
  // cost alone and reads as zero for a run that ran on Codex.
  let costUsd = $derived(getWorkflowDetail(itemId)?.spend?.costUsd || getWorkflowCosts()[itemId] || 0);

  let preview = $state<WorkflowDiscardPreview | null>(null);
  let loading = $state(false);
  let error = $state('');
  let submitting = $state(false);
  let loadedFor = '';

  $effect(() => {
    if (!open || !itemId) {
      loadedFor = '';
      return;
    }
    if (loadedFor === itemId) return;
    loadedFor = itemId;
    preview = null;
    error = '';
    loading = true;
    void (async () => {
      try {
        preview = await fetchDiscardPreview(itemId);
      } catch (err) {
        error = userFacingError(err, 'Could not work out what discarding would destroy.');
      } finally {
        loading = false;
      }
    })();
  });

  async function confirm(): Promise<void> {
    if (!item || ungranted || submitting) return;
    submitting = true;
    try {
      if (await resolveWorkflowRun(item, { kind: 'discard' }, costUsd)) onClose();
    } finally {
      submitting = false;
    }
  }
</script>

<Modal {open} title="Discard this run?" {onClose} width="lg" padding="comfortable">
  {#snippet children()}
    <div class="space-y-3 text-sm" data-testid="workflow-discard-dialog">
      {#if loading}
        <p class="text-xs text-fg-muted">Working out what this would destroy…</p>
      {:else if error}
        <p class="text-xs text-error" data-testid="workflow-discard-error">{error}</p>
      {:else if preview}
        {#if preview.liveMembers.length > 0}
          <p class="text-xs text-warning" data-testid="workflow-discard-live">
            {preview.liveMembers.length} {preview.liveMembers.length === 1 ? 'run is' : 'runs are'} still working and will be stopped first.
          </p>
        {/if}

        {#if preview.worktrees.length > 0}
          <ul
            class="divide-y divide-border-subtle rounded-md border border-border-subtle"
            data-testid="workflow-discard-worktrees"
          >
            {#each preview.worktrees as worktree (worktree.unitId ? `${worktree.path}::${worktree.unitId}` : worktree.path)}
              <li class="px-3 py-2" data-testid="workflow-discard-worktree">
                <p class="truncate font-mono text-xs text-fg">{worktree.path}</p>
                <p class="truncate text-[0.6875rem] text-fg-muted">{worktreeLossSummary(worktree)}</p>
              </li>
            {/each}
          </ul>
        {:else}
          <p class="text-xs text-fg-muted" data-testid="workflow-discard-no-worktrees">
            No checkouts to remove — this run left nothing on disk.
          </p>
        {/if}

        <p class="text-xs text-fg-muted" data-testid="workflow-discard-artifacts">
          Artifacts already captured survive. The run record is kept.
        </p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onClose}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="danger"
      size="sm"
      testId="workflow-discard-confirm"
      onclick={() => { void confirm(); }}
      disabled={ungranted || submitting || loading || !item || Boolean(error)}
      loading={submitting}
    >
      {#snippet children()}{submitting ? 'Discarding…' : 'Discard'}{/snippet}
    </Button>
  {/snippet}
</Modal>
