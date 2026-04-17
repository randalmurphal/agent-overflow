<script lang="ts">
  // Step 2: push the current branch. Pure UI; side effects belong to the
  // drawer.

  import type { ShipChangesState } from '../../../stores/shipChanges.svelte';
  import type { GitStatus } from '../../../types/git';

  let { state, onPush, onSkip }: {
    state: ShipChangesState;
    onPush: () => void;
    onSkip: () => void;
  } = $props();

  let status = $derived<GitStatus | null>(state.status);
  let busy = $derived(state.phase === 'push.busy');
  let hasError = $derived(state.phase === 'push.error');
  // If aheadCount is 0 *and* the branch has an upstream, there's nothing to
  // push. If there's no upstream, pushing sets it — so we still offer Push.
  let nothingToPush = $derived(
    !!status && status.hasUpstream && status.aheadCount === 0,
  );
</script>

<div class="space-y-3" data-testid="ship-changes-step-push">
  <header class="space-y-1">
    <h3 class="text-sm font-semibold text-text-primary">Push to remote</h3>
    {#if status}
      <p class="text-[11px] text-text-secondary" data-testid="ship-changes-push-summary">
        {#if !status.hasUpstream}
          <code class="text-[10px] bg-surface-2/60 px-1 rounded">{status.branch}</code>
          has no upstream — push will set it.
        {:else if status.aheadCount > 0}
          {status.aheadCount} commit{status.aheadCount === 1 ? '' : 's'}
          ahead of <code class="text-[10px] bg-surface-2/60 px-1 rounded">origin/{status.branch}</code>.
        {:else}
          Branch <code class="text-[10px] bg-surface-2/60 px-1 rounded">{status.branch}</code>
          is up to date with origin.
        {/if}
      </p>
    {/if}
  </header>

  {#if hasError && state.error}
    <p class="text-xs text-error break-words" role="alert" data-testid="ship-changes-push-error">
      {state.error}
    </p>
  {/if}

  <div class="flex justify-end gap-2 pt-1">
    <button
      type="button"
      onclick={onSkip}
      disabled={busy}
      data-testid="ship-changes-push-skip"
      class="px-3 py-2 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {nothingToPush ? 'Next' : 'Skip push'}
    </button>
    <button
      type="button"
      onclick={onPush}
      disabled={!state.canPush || busy}
      data-testid="ship-changes-push-submit"
      class="px-4 py-2 text-xs rounded-md font-medium bg-accent text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {busy ? 'Pushing…' : 'Push'}
    </button>
  </div>
</div>
