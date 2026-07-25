<script lang="ts">
  // The consent dialog for deleting a project that owns workflow work (D25).
  // The preview has already been fetched by the caller — the delete is not
  // offered until we can say what it would destroy — so this surface only has
  // to show the loss and take the confirmation.
  //
  // The plain case (a project with no runs and no automations) keeps the
  // ordinary ConfirmDialog; nothing here renders for it.

  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import WorkflowLossList from '../shared/WorkflowLossList.svelte';
  import type { ProjectDeletionPreview } from '../../types/workflow';
  import { runLossSummary } from '../../utils/workflowLoss';
  import { isViewOnlySession } from '../../transport/runMode';

  interface Props {
    open: boolean;
    projectName: string;
    threadCount: number;
    preview: ProjectDeletionPreview;
    submitting: boolean;
    onConfirm: () => void;
    onCancel: () => void;
  }
  let {
    open,
    projectName,
    threadCount,
    preview,
    submitting,
    onConfirm,
    onCancel,
  }: Props = $props();

  let viewOnly = $derived(isViewOnlySession());
  let threadSummary = $derived(
    `Permanently delete "${projectName}" and all ${threadCount} thread${
      threadCount === 1 ? '' : 's'
    } it contains.`,
  );
  let workSummary = $derived(
    runLossSummary(preview.runCount, preview.liveRunIds.length, preview.automationCount),
  );
</script>

<Modal {open} title="Delete Project" onClose={onCancel} width="lg" padding="comfortable">
  {#snippet children()}
    <div class="space-y-3 text-sm" data-testid="project-delete-dialog">
      <p class="text-[0.8125rem] text-fg-muted leading-relaxed">{threadSummary}</p>
      {#if workSummary}
        <p class="text-xs text-warning" data-testid="project-delete-work">{workSummary}</p>
      {/if}
      <WorkflowLossList
        worktrees={preview.worktrees}
        emptyMessage="No checkouts to remove — these runs left nothing on disk."
        testIdPrefix="project-delete"
      />
      <p class="text-xs text-fg-muted" data-testid="project-delete-irreversible">
        The branches listed here are deleted with their checkouts. This cannot be undone.
      </p>
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" autofocus onclick={onCancel}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="danger"
      size="sm"
      testId="project-delete-confirm"
      title={viewOnly ? 'Local only' : undefined}
      onclick={onConfirm}
      disabled={viewOnly || submitting}
      loading={submitting}
    >
      {#snippet children()}{submitting ? 'Deleting…' : 'Delete Everything'}{/snippet}
    </Button>
  {/snippet}
</Modal>
