<script lang="ts">
  // What deleting a project involves when it owns workflow work (D25).
  //
  // Informational, not a consent gate: deletion is cleanup. It removes the runs
  // and automations from the app and the checkouts the app made for them, and
  // it deletes no branch — the commits those runs produced stay in the
  // repository, which the user still owns after the project is gone. The one
  // thing worth saying up front is which checkouts will be left behind, because
  // git refuses to remove a checkout with uncommitted work in it and the app
  // does not override that.
  //
  // The plain case (a project with no runs and no automations) keeps the
  // ordinary ConfirmDialog; nothing here renders for it.

  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { ProjectDeletionPreview } from '../../types/workflow';
  import { cleanupSummary, retainedInPreview } from '../../utils/projectCleanup';
  import { hasScope } from '../../transport/scopes';

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

  // Deleting a project is thread/project bookkeeping.
  let ungranted = $derived(!hasScope('threads:operate'));
  let threadSummary = $derived(
    `Permanently delete "${projectName}" and all ${threadCount} thread${
      threadCount === 1 ? '' : 's'
    } it contains.`,
  );
  let workSummary = $derived(
    cleanupSummary(preview.runCount, preview.liveRunIds.length, preview.automationCount),
  );
  let retained = $derived(retainedInPreview(preview.worktrees));
</script>

<Modal {open} title="Delete Project" onClose={onCancel} width="lg" padding="comfortable">
  {#snippet children()}
    <div class="space-y-3 text-sm" data-testid="project-delete-dialog">
      <p class="text-[0.8125rem] text-fg-muted leading-relaxed">{threadSummary}</p>
      {#if workSummary}
        <p class="text-xs text-fg-muted" data-testid="project-delete-work">{workSummary}</p>
      {/if}
      {#if preview.worktrees.length > 0}
        <p class="text-xs text-fg-muted" data-testid="project-delete-branches">
          The worktrees these runs used are removed. Their branches are kept, so nothing they
          committed is lost.
        </p>
      {/if}
      {#if retained.length > 0}
        <p class="text-xs text-warning" data-testid="project-delete-retained-note">
          {retained.length === 1 ? 'This worktree has' : 'These worktrees have'} uncommitted changes
          and {retained.length === 1 ? 'is' : 'are'} left in place for you:
        </p>
        <ul
          class="divide-y divide-border-subtle rounded-md border border-border-subtle"
          data-testid="project-delete-retained"
        >
          {#each retained as worktree (worktree.path)}
            <li class="px-3 py-2" data-testid="project-delete-worktree">
              <p class="truncate font-mono text-xs text-fg">{worktree.path}</p>
              <p class="truncate text-[0.6875rem] text-fg-muted">
                {worktree.branch || 'no branch'} · {worktree.reason}
              </p>
            </li>
          {/each}
        </ul>
      {/if}
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
      title={ungranted ? 'Local only' : undefined}
      onclick={onConfirm}
      disabled={ungranted || submitting}
      loading={submitting}
    >
      {#snippet children()}{submitting ? 'Deleting…' : 'Delete Project'}{/snippet}
    </Button>
  {/snippet}
</Modal>
