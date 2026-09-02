<script lang="ts">
  // Prune preview: the consent surface for deleting local branches whose
  // remote counterpart is gone. Safe rows (merged into the default
  // branch, or tip exactly matching a merged PR head — the squash case)
  // arrive pre-checked; anything that might hold unpushed work arrives
  // unchecked with the reason spelled out. Deletion is consented per
  // (branch, tip) pair — the backend refuses a branch whose tip moved
  // after this preview rendered.

  import { untrack } from 'svelte';
  import Modal from '../../primitives/Modal.svelte';
  import Button from '../../primitives/Button.svelte';
  import SteppedSpinner from '../../primitives/SteppedSpinner.svelte';
  import { GitListBranchPruneCandidates, GitPruneBranches } from '../../../stores/bindings';
  import { isViewOnlySession } from '../../../transport/runMode';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';
  import type {
    BranchPruneCandidate,
    BranchPruneCandidates,
    BranchPruneResult,
    WorkspaceRef,
  } from '../../../types/git';

  interface Props {
    workspace: WorkspaceRef;
    open: boolean;
    onClose: () => void;
  }

  let { workspace, open, onClose }: Props = $props();

  let viewOnly = $derived(isViewOnlySession());
  let loading = $state(false);
  let deleting = $state(false);
  let loadError = $state('');
  let forgeWarning = $state('');
  let candidates: BranchPruneCandidate[] = $state([]);
  let checked: Record<string, boolean> = $state({});
  // Per-branch refusal/failure reasons from the last delete attempt.
  // Rendered inline (the dialog stays open on partial failure) because
  // the refreshed preview shows classification reasons, not these.
  let failures: Record<string, string> = $state({});

  let checkedCount = $derived(candidates.filter((c) => checked[c.branch]).length);
  let failureEntries = $derived(Object.entries(failures));

  // Re-fetch every time the dialog opens; candidates are point-in-time.
  // untrack: loadCandidates reads AND writes `loading`/`candidates`, so a
  // tracked call would re-run this effect after every fetch settles.
  $effect(() => {
    if (!open) return;
    untrack(() => {
      void loadCandidates();
    });
  });

  async function loadCandidates(): Promise<void> {
    if (loading) return;
    loading = true;
    loadError = '';
    forgeWarning = '';
    candidates = [];
    checked = {};
    try {
      const res = (await GitListBranchPruneCandidates(workspace)) as BranchPruneCandidates;
      candidates = res?.candidates ?? [];
      forgeWarning = res?.forgeWarning ?? '';
      const next: Record<string, boolean> = {};
      for (const candidate of candidates) {
        next[candidate.branch] = candidate.safe;
      }
      checked = next;
    } catch (err) {
      console.error('GitListBranchPruneCandidates failed:', err);
      loadError = userFacingError(err);
    } finally {
      loading = false;
    }
  }

  async function confirmDelete(): Promise<void> {
    if (viewOnly || deleting || checkedCount === 0) return;
    const selections = candidates
      .filter((c) => checked[c.branch])
      .map((c) => ({ branch: c.branch, tip: c.tip }));
    deleting = true;
    failures = {};
    try {
      const res = (await GitPruneBranches(workspace, selections)) as BranchPruneResult;
      const deleted = res?.deleted ?? [];
      const failed = res?.failed ?? {};
      const failedNames = Object.keys(failed);
      if (deleted.length > 0) {
        addToast('info', `Pruned ${deleted.length} branch${deleted.length === 1 ? '' : 'es'}`);
      }
      if (failedNames.length === 0) {
        onClose();
      } else {
        // Keep the dialog up: per-branch reasons render inline over a
        // fresh preview instead of vanishing behind a toast.
        const next: Record<string, string> = {};
        for (const branch of failedNames) {
          next[branch] = userFacingError(failed[branch]);
        }
        failures = next;
        await loadCandidates();
      }
    } catch (err) {
      console.error('GitPruneBranches failed:', err);
      addToast('error', userFacingError(err));
    } finally {
      deleting = false;
    }
  }

  function toggle(branch: string): void {
    checked = { ...checked, [branch]: !checked[branch] };
  }
</script>

<Modal {open} title="Prune branches" onClose={onClose} width="md" padding="comfortable">
  {#snippet children()}
    {#if loading}
      <div
        class="flex items-center gap-2 py-4 text-[0.8125rem] text-fg-muted"
        data-testid="prune-dialog-loading"
      >
        <SteppedSpinner size={12} />
        Checking remotes for deleted branches…
      </div>
    {:else if loadError}
      <p class="py-2 text-[0.8125rem] text-error" data-testid="prune-dialog-error">{loadError}</p>
    {:else if candidates.length === 0 && failureEntries.length === 0}
      <p class="py-2 text-[0.8125rem] text-fg-muted" data-testid="prune-dialog-empty">
        Nothing to prune — every local branch either still exists on the remote, was never
        pushed, or is checked out in a worktree.
      </p>
    {:else}
      <div class="flex flex-col gap-1" data-testid="prune-dialog-list">
        {#if failureEntries.length > 0}
          <div
            class="flex flex-col gap-0.5 pb-1 text-[0.75rem] text-error"
            data-testid="prune-dialog-failures"
          >
            {#each failureEntries as [branch, reason] (branch)}
              <p>Couldn't prune <span class="font-medium">{branch}</span>: {reason}</p>
            {/each}
          </div>
        {/if}
        {#if forgeWarning}
          <p class="pb-1 text-[0.75rem] text-warning" data-testid="prune-dialog-forge-warning">
            {forgeWarning} — squash-merged branches can't be verified and are left unchecked.
          </p>
        {/if}
        {#each candidates as candidate (candidate.branch)}
          <label
            class={[
              'flex cursor-pointer items-start gap-2 rounded border border-border-subtle px-2 py-1.5',
              'hover:border-border-strong',
            ].join(' ')}
            data-testid={`prune-row-${candidate.branch}`}
          >
            <input
              type="checkbox"
              class="mt-0.5 accent-accent"
              checked={!!checked[candidate.branch]}
              onchange={() => toggle(candidate.branch)}
              disabled={deleting}
            />
            <span class="min-w-0 flex-1">
              <span class="flex items-baseline gap-2">
                <span class="truncate text-[0.8125rem] text-fg" title={candidate.branch}>
                  {candidate.branch}
                </span>
                <code class="shrink-0 text-[0.6875rem] text-fg-hint">{candidate.tip.slice(0, 7)}</code>
              </span>
              <span
                class={[
                  'block truncate text-[0.75rem]',
                  candidate.safe ? 'text-fg-muted' : 'text-warning',
                ].join(' ')}
                title={candidate.reason}
              >
                {candidate.reason}
              </span>
              {#if candidate.subject}
                <span class="block truncate text-[0.6875rem] text-fg-hint" title={candidate.subject}>
                  {candidate.subject}
                </span>
              {/if}
            </span>
          </label>
        {/each}
      </div>
    {/if}
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" autofocus={true} onclick={onClose}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="danger"
      size="sm"
      testId="prune-dialog-confirm"
      title={viewOnly ? 'Local only' : undefined}
      disabled={viewOnly || loading || checkedCount === 0}
      loading={deleting}
      onclick={confirmDelete}
    >
      {#snippet children()}
        {`Delete ${checkedCount} branch${checkedCount === 1 ? '' : 'es'}`}
      {/snippet}
    </Button>
  {/snippet}
</Modal>
