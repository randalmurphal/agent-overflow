<script lang="ts">
  // Step 2: push the current branch. Pure UI; side effects belong to the
  // drawer.

  import type { ShipChangesState } from '../../../stores/shipChanges.svelte';
  import type { GitStatus } from '../../../types/git';
  import Button from '../../primitives/Button.svelte';

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
    <h3 class="text-sm font-semibold text-text-primary">Push to Remote</h3>
    {#if status}
      <p class="text-[0.6875rem] text-text-secondary" data-testid="ship-changes-push-summary">
        {#if !status.hasUpstream}
          <code class="text-[0.625rem] bg-surface-2/60 px-1 rounded">{status.branch}</code>
          has no upstream — push will set it.
        {:else if status.aheadCount > 0}
          {status.aheadCount} commit{status.aheadCount === 1 ? '' : 's'}
          ahead of <code class="text-[0.625rem] bg-surface-2/60 px-1 rounded">origin/{status.branch}</code>.
        {:else}
          Branch <code class="text-[0.625rem] bg-surface-2/60 px-1 rounded">{status.branch}</code>
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
    <Button
      variant="secondary"
      size="md"
      onclick={onSkip}
      disabled={busy}
      testId="ship-changes-push-skip"
    >
      {#snippet children()}{nothingToPush ? 'Next' : 'Skip push'}{/snippet}
    </Button>
    <Button
      variant="primary"
      size="md"
      onclick={onPush}
      disabled={!state.canPush}
      loading={busy}
      testId="ship-changes-push-submit"
    >
      {#snippet children()}{busy ? 'Pushing…' : 'Push'}{/snippet}
    </Button>
  </div>
</div>
