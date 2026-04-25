<script lang="ts">
  // Step 3: open a pull request. Success path renders a clickable URL; the
  // drawer stays mounted so the user can copy the link before closing.

  import type { ShipChangesState } from '../../../stores/shipChanges.svelte';
  import type { GitStatus } from '../../../types/git';
  import Button from '../../primitives/Button.svelte';

  let { state, onCreate }: {
    state: ShipChangesState;
    onCreate: () => void;
  } = $props();

  let status = $derived<GitStatus | null>(state.status);
  let busy = $derived(state.phase === 'pr.busy');
  let hasError = $derived(state.phase === 'pr.error');
  let done = $derived(state.phase === 'pr.done');
  let alreadyHasPR = $derived(!!status?.openPrUrl);
</script>

<div class="space-y-3" data-testid="ship-changes-step-pr">
  <header class="space-y-1">
    <h3 class="text-sm font-semibold text-text-primary">Open Pull Request</h3>
    {#if alreadyHasPR}
      <p class="text-[11px] text-text-secondary">
        A PR is already open for this branch:
        <a
          href={status?.openPrUrl}
          target="_blank"
          rel="noopener noreferrer"
          class="underline text-accent hover:opacity-90"
        >
          {status?.openPrUrl}
        </a>
      </p>
    {:else if status}
      <p class="text-[11px] text-text-secondary">
        Create a PR for <code class="text-[10px] bg-surface-2/60 px-1 rounded">{status.branch}</code>
        against the default branch.
      </p>
    {/if}
  </header>

  {#if done}
    <div class="space-y-1" data-testid="ship-changes-pr-done">
      <p class="text-xs text-text-secondary">Pull request opened:</p>
      <a
        href={state.prUrl ?? ''}
        target="_blank"
        rel="noopener noreferrer"
        data-testid="ship-changes-pr-url"
        class="inline-block text-xs text-accent underline break-all hover:opacity-90"
      >
        {state.prUrl}
      </a>
    </div>
  {:else if !alreadyHasPR}
    <div class="space-y-2">
      <label class="text-xs text-text-secondary block" for="ship-pr-title">Title</label>
      <input
        id="ship-pr-title"
        data-testid="ship-changes-pr-title"
        type="text"
        value={state.prTitle}
        oninput={(e) => state.setPRTitle((e.currentTarget as HTMLInputElement).value)}
        disabled={busy}
        placeholder="PR title"
        class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 transition-colors"
      />
    </div>

    <div class="space-y-2">
      <label class="text-xs text-text-secondary block" for="ship-pr-body">Description (optional)</label>
      <textarea
        id="ship-pr-body"
        data-testid="ship-changes-pr-body"
        rows={5}
        value={state.prBody}
        oninput={(e) => state.setPRBody((e.currentTarget as HTMLTextAreaElement).value)}
        disabled={busy}
        placeholder="Describe the change for reviewers…"
        class="w-full text-sm rounded border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 transition-colors resize-none"
      ></textarea>
    </div>

    <label class="flex items-center gap-2 text-xs text-text-secondary cursor-pointer select-none">
      <input
        type="checkbox"
        data-testid="ship-changes-pr-draft"
        checked={state.prDraft}
        onchange={(e) => state.setPRDraft((e.currentTarget as HTMLInputElement).checked)}
        disabled={busy}
        class="h-3 w-3 rounded border-border cursor-pointer"
      />
      <span>Open as draft</span>
    </label>
  {/if}

  {#if hasError && state.error}
    <p class="text-xs text-error break-words" role="alert" data-testid="ship-changes-pr-error">
      {state.error}
    </p>
  {/if}

  {#if !done && !alreadyHasPR}
    <div class="flex justify-end gap-2 pt-1">
      <Button
        variant="primary"
        size="md"
        onclick={onCreate}
        disabled={!state.canCreatePR}
        loading={busy}
        testId="ship-changes-pr-submit"
      >
        {#snippet children()}{busy ? 'Opening PR…' : 'Create PR'}{/snippet}
      </Button>
    </div>
  {/if}
</div>
