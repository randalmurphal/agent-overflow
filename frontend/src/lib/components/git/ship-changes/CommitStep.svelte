<script lang="ts">
  // Step 1: review, edit, and commit the current diff. Pure UI over the
  // ShipChangesState store — the parent drawer owns the GitCommit call.

  import type { ShipChangesState } from '../../../stores/shipChanges.svelte';
  import type { GitStatus } from '../../../types/git';
  import { GenerateCommitMessage } from '../../../stores/bindings';
  import { addToast } from '../../../stores/toast.svelte';
  import Button from '../../primitives/Button.svelte';

  // Renamed the destructured prop to `ship` so `$state(...)` doesn't trip the
  // Svelte 5 compiler's store-subscription heuristic (any local binding named
  // `state` causes `$state` to be parsed as `$store` auto-sub).
  let { state: ship, onCommit, onSkip }: {
    state: ShipChangesState;
    onCommit: () => void;
    onSkip: () => void;
  } = $props();

  let status = $derived<GitStatus | null>(ship.status);
  let busy = $derived(ship.phase === 'commit.busy');
  let hasError = $derived(ship.phase === 'commit.error');
  let nothingToCommit = $derived(!!status && !status.hasChanges);
  let pendingOperation = $derived(status?.pendingOperation ?? '');
  let generating = $state(false);

  async function handleGenerate(): Promise<void> {
    if (generating || busy || nothingToCommit) return;
    if (!ship.threadId) return;
    generating = true;
    try {
      const message = await GenerateCommitMessage(ship.threadId);
      ship.setCommitSubject(message.subject ?? '');
      ship.setCommitBody(message.body ?? '');
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      addToast('error', `Couldn't generate commit message: ${reason}`);
    } finally {
      generating = false;
    }
  }
  let pendingMessage = $derived.by(() => {
    switch (pendingOperation) {
      case 'merge':
        return 'A merge is in progress. Resolve or abort it with `git merge --abort` before committing.';
      case 'rebase':
        return 'A rebase is in progress. Run `git rebase --continue` or `git rebase --abort` before committing.';
      case 'bisect':
        return 'A bisect is in progress. Run `git bisect reset` before committing.';
      default:
        return '';
    }
  });
</script>

<div class="space-y-3" data-testid="ship-changes-step-commit">
  <header class="space-y-1">
    <h3 class="text-sm font-semibold text-text-primary">Commit Changes</h3>
    {#if status}
      <p class="text-[0.6875rem] text-text-secondary" data-testid="ship-changes-diff-summary">
        {#if status.hasChanges}
          {status.fileCount} file{status.fileCount === 1 ? '' : 's'} changed
          &middot; +{status.insertions}/-{status.deletions}
          on <code class="text-[0.625rem] bg-surface-2/60 px-1 rounded">{status.branch}</code>
        {:else}
          No uncommitted changes. Skip to Push.
        {/if}
      </p>
    {/if}
  </header>

  {#if pendingMessage}
    <p
      class="text-xs text-warning bg-warning/10 border border-warning/40 rounded px-2 py-1.5"
      role="alert"
      data-testid="ship-changes-pending-operation"
    >
      {pendingMessage}
    </p>
  {/if}

  <div class="space-y-2">
    <div class="flex items-center justify-between gap-2">
      <label class="text-xs text-text-secondary" for="ship-commit-subject">Subject</label>
      <button
        type="button"
        data-testid="ship-changes-generate-message"
        onclick={handleGenerate}
        disabled={generating || busy || nothingToCommit}
        title="Ask the agent to draft a commit message from the current diff"
        class="text-[0.625rem] px-2 py-0.5 rounded border border-border/70 text-text-secondary hover:text-accent hover:border-accent/40 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        {generating ? 'Generating…' : 'Generate'}
      </button>
    </div>
    <input
      id="ship-commit-subject"
      data-testid="ship-changes-commit-subject"
      type="text"
      maxlength={72}
      value={ship.commitSubject}
      oninput={(e) => ship.setCommitSubject((e.currentTarget as HTMLInputElement).value)}
      disabled={busy || nothingToCommit}
      placeholder="Describe the change"
      class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 transition-colors"
    />
    <span class="text-[0.625rem] text-text-secondary/40 block text-right">
      {ship.commitSubject.length}/72
    </span>
  </div>

  <div class="space-y-2">
    <label class="text-xs text-text-secondary block" for="ship-commit-body">Body (optional)</label>
    <textarea
      id="ship-commit-body"
      data-testid="ship-changes-commit-body"
      rows={4}
      value={ship.commitBody}
      oninput={(e) => ship.setCommitBody((e.currentTarget as HTMLTextAreaElement).value)}
      disabled={busy || nothingToCommit}
      placeholder="Extended description…"
      class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 transition-colors resize-none"
    ></textarea>
  </div>

  {#if hasError && ship.error}
    <p class="text-xs text-error break-words" role="alert" data-testid="ship-changes-commit-error">
      {ship.error}
    </p>
  {/if}

  <div class="flex justify-end gap-2 pt-1">
    <Button
      variant="secondary"
      size="md"
      onclick={onSkip}
      disabled={busy}
      testId="ship-changes-commit-skip"
    >
      {#snippet children()}{nothingToCommit ? 'Next' : 'Skip commit'}{/snippet}
    </Button>
    <Button
      variant="primary"
      size="md"
      onclick={onCommit}
      disabled={!ship.canCommit || nothingToCommit}
      loading={busy}
      testId="ship-changes-commit-submit"
    >
      {#snippet children()}{busy ? 'Committing…' : 'Commit'}{/snippet}
    </Button>
  </div>
</div>
