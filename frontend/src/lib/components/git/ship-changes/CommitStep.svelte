<script lang="ts">
  // Step 1: review, edit, and commit the current diff. Pure UI over the
  // ShipChangesState store — the parent drawer owns the GitCommit call.

  import type { ShipChangesState } from '../../../stores/shipChanges.svelte';
  import type { GitStatus } from '../../../types/git';

  let { state, onCommit, onSkip }: {
    state: ShipChangesState;
    onCommit: () => void;
    onSkip: () => void;
  } = $props();

  let status = $derived<GitStatus | null>(state.status);
  let busy = $derived(state.phase === 'commit.busy');
  let hasError = $derived(state.phase === 'commit.error');
  let nothingToCommit = $derived(!!status && !status.hasChanges);
</script>

<div class="space-y-3" data-testid="ship-changes-step-commit">
  <header class="space-y-1">
    <h3 class="text-sm font-semibold text-text-primary">Commit changes</h3>
    {#if status}
      <p class="text-[11px] text-text-secondary" data-testid="ship-changes-diff-summary">
        {#if status.hasChanges}
          {status.fileCount} file{status.fileCount === 1 ? '' : 's'} changed
          &middot; +{status.insertions}/-{status.deletions}
          on <code class="text-[10px] bg-surface-2/60 px-1 rounded">{status.branch}</code>
        {:else}
          No uncommitted changes. Skip to Push.
        {/if}
      </p>
    {/if}
  </header>

  <div class="space-y-2">
    <label class="text-xs text-text-secondary block" for="ship-commit-subject">Subject</label>
    <input
      id="ship-commit-subject"
      data-testid="ship-changes-commit-subject"
      type="text"
      maxlength={72}
      value={state.commitSubject}
      oninput={(e) => state.setCommitSubject((e.currentTarget as HTMLInputElement).value)}
      disabled={busy || nothingToCommit}
      placeholder="Describe the change"
      class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 transition-colors"
    />
    <span class="text-[10px] text-text-secondary/40 block text-right">
      {state.commitSubject.length}/72
    </span>
  </div>

  <div class="space-y-2">
    <label class="text-xs text-text-secondary block" for="ship-commit-body">Body (optional)</label>
    <textarea
      id="ship-commit-body"
      data-testid="ship-changes-commit-body"
      rows={4}
      value={state.commitBody}
      oninput={(e) => state.setCommitBody((e.currentTarget as HTMLTextAreaElement).value)}
      disabled={busy || nothingToCommit}
      placeholder="Extended description…"
      class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 transition-colors resize-none"
    ></textarea>
  </div>

  {#if hasError && state.error}
    <p class="text-xs text-error break-words" role="alert" data-testid="ship-changes-commit-error">
      {state.error}
    </p>
  {/if}

  <div class="flex justify-end gap-2 pt-1">
    <button
      type="button"
      onclick={onSkip}
      disabled={busy}
      data-testid="ship-changes-commit-skip"
      class="px-3 py-2 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {nothingToCommit ? 'Next' : 'Skip commit'}
    </button>
    <button
      type="button"
      onclick={onCommit}
      disabled={!state.canCommit || busy || nothingToCommit}
      data-testid="ship-changes-commit-submit"
      class="px-4 py-2 text-xs rounded-md font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {busy ? 'Committing…' : 'Commit'}
    </button>
  </div>
</div>
