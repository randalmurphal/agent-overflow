<script lang="ts">
  // The rows a loss preview shows before anything is destroyed: one per
  // checkout, with its branch, dirty files and unmerged commits. Shared by the
  // single-run discard dialog (§4.5, D23) and the project-deletion dialog
  // (D25) so the two cannot describe the same loss differently.
  //
  // Lives in shared/ rather than workflows/ on purpose: the sidebar's delete
  // flow is eagerly loaded, and a static import reaching into the workflows
  // overlay would drag that whole chunk into startup.

  import type { WorkflowDiscardWorktree } from '../../types/workflow';
  import { worktreeLossSummary } from '../../utils/workflowLoss';

  interface Props {
    worktrees: WorkflowDiscardWorktree[];
    /** Shown in place of the list when the loss touches no checkout. */
    emptyMessage: string;
    /** Prefixes the row testids so each host surface stays addressable. */
    testIdPrefix: string;
  }
  let { worktrees, emptyMessage, testIdPrefix }: Props = $props();

  let rows = $derived(
    worktrees.map((worktree) => ({
      key: worktree.unitId ? `${worktree.path}::${worktree.unitId}` : worktree.path,
      path: worktree.path,
      summary: worktreeLossSummary(worktree),
    })),
  );
</script>

{#if rows.length > 0}
  <ul
    class="divide-y divide-border-subtle rounded-md border border-border-subtle"
    data-testid="{testIdPrefix}-worktrees"
  >
    {#each rows as row (row.key)}
      <li class="px-3 py-2" data-testid="{testIdPrefix}-worktree">
        <p class="truncate font-mono text-xs text-fg">{row.path}</p>
        <p class="truncate text-[0.6875rem] text-fg-muted">{row.summary}</p>
      </li>
    {/each}
  </ul>
{:else}
  <p class="text-xs text-fg-muted" data-testid="{testIdPrefix}-no-worktrees">
    {emptyMessage}
  </p>
{/if}
