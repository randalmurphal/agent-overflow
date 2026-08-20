<script lang="ts">
  // The in-pane surface for a chat thread's worktree setup run.
  //
  // A floating, non-modal card anchored above the composer inside the pane —
  // NOT a Modal. Setup runs while the user is reading, typing, or already
  // sending a first message; trapping focus for a background provisioning step
  // would be hostile, and the thread is usable whether the recipe succeeds or
  // not.
  //
  // Outcome behaviour is asymmetric on purpose:
  //   - Success dismisses itself after a beat. It is an acknowledgement, not
  //     information the user has to act on, and the backend has already
  //     dropped the run.
  //   - Failure stays, with the failed step highlighted and Retry in reach.
  //     Dismissing it collapses to a one-line bar rather than hiding it: the
  //     worktree is genuinely under-provisioned until something fixes it.
  import AnsiText from './AnsiText.svelte';
  import WorktreeSetupSteps from './WorktreeSetupSteps.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import {
    clearSettledWorktreeSetup,
    dismissWorktreeSetup,
    getWorktreeSetup,
    retryWorktreeSetup,
    showWorktreeSetup,
  } from '../../stores/worktreeSetup.svelte';
  import { userFacingError } from '../../utils/userFacingError';

  let { threadId }: { threadId: string } = $props();

  /** How long a succeeded run's card stays up before clearing itself. */
  const SUCCESS_LINGER_MS = 2500;

  const view = $derived(getWorktreeSetup(threadId));
  const running = $derived(view?.state === 'running');
  const failed = $derived(view?.state === 'failed');

  let retrying = $state(false);
  // Self-ticking clock for the elapsed readout. Only advances while a run is
  // in flight, so a settled card costs nothing.
  let now = $state(Date.now());

  $effect(() => {
    if (!running) return;
    const timer = setInterval(() => { now = Date.now(); }, 1000);
    return () => clearInterval(timer);
  });

  // Keyed on the settled FACT, not the view box: the registry replaces the
  // box wholesale per streaming frame, so an effect reading `view` directly
  // restarted the linger timer on every trailing frame that arrived after
  // `succeeded`.
  const succeededRunId = $derived(view?.state === 'succeeded' ? view.runId : null);

  $effect(() => {
    const runId = succeededRunId;
    if (runId === null) return;
    const timer = setTimeout(() => clearSettledWorktreeSetup(threadId, runId), SUCCESS_LINGER_MS);
    return () => clearTimeout(timer);
  });

  const elapsedLabel = $derived.by(() => {
    if (!view?.startedAt) return '';
    const end = view.finishedAt || now;
    const seconds = Math.max(0, Math.round((end - view.startedAt) / 1000));
    if (seconds < 60) return `${seconds}s`;
    return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, '0')}s`;
  });

  const headline = $derived.by(() => {
    if (running) return 'Setting up worktree';
    if (failed) return 'Worktree setup failed';
    return 'Worktree ready';
  });

  const failedStep = $derived.by(() => {
    if (!view || !failed) return null;
    const index = view.stepStatuses.findIndex((status) => status === 'failed');
    return index < 0 ? null : view.steps[index] ?? null;
  });

  async function onRetry(): Promise<void> {
    if (retrying) return;
    retrying = true;
    try {
      await retryWorktreeSetup(threadId);
    } catch (err) {
      addToast('error', userFacingError(err, 'Failed to retry worktree setup'));
    } finally {
      retrying = false;
    }
  }
</script>

{#if view}
  {#if view.dismissed}
    <div
      class="pointer-events-auto mx-auto w-full max-w-[62rem] px-6 pb-2"
      data-testid="worktree-setup-bar"
    >
      <div class="flex items-center gap-2 border border-warning/40 bg-surface-1/95 px-3 py-1.5 text-xs shadow-sheet">
        <span class="h-1.5 w-1.5 shrink-0 rounded-full bg-warning" aria-hidden="true"></span>
        <span class="min-w-0 flex-1 truncate text-text-secondary">Worktree setup failed</span>
        <button
          type="button"
          class="text-fg-muted transition-colors hover:text-text-primary"
          data-testid="worktree-setup-show"
          onclick={() => showWorktreeSetup(threadId)}
        >Show</button>
        <span class="text-fg-muted" aria-hidden="true">·</span>
        <button
          type="button"
          class="text-warning transition-colors hover:text-text-primary disabled:opacity-50"
          data-testid="worktree-setup-bar-retry"
          disabled={retrying}
          onclick={onRetry}
        >Retry</button>
      </div>
    </div>
  {:else}
    <div
      class="pointer-events-auto mx-auto w-full max-w-[62rem] px-6 pb-2"
      data-testid="worktree-setup-panel"
      data-state={view.state}
    >
      <div
        class="flex max-h-[45vh] min-h-0 flex-col border bg-surface-1/95 shadow-sheet
          {failed ? 'border-warning/50' : 'border-border-subtle'}"
      >
        <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-2">
          <span
            class="h-1.5 w-1.5 shrink-0 rounded-full {failed ? 'bg-warning' : running ? 'bg-info animate-pulse' : 'bg-success'}"
            aria-hidden="true"
          ></span>
          <span class="min-w-0 flex-1 truncate text-xs font-medium text-text-primary">{headline}</span>
          {#if elapsedLabel}
            <span class="shrink-0 font-mono text-[0.6875rem] text-fg-muted">{elapsedLabel}</span>
          {/if}
          {#if failed}
            <button
              type="button"
              class="shrink-0 border border-warning/50 px-2 py-0.5 text-[0.6875rem] text-warning transition-colors hover:bg-warning/10 disabled:opacity-50"
              data-testid="worktree-setup-retry"
              disabled={retrying}
              onclick={onRetry}
            >Retry</button>
            <button
              type="button"
              class="shrink-0 text-[0.6875rem] text-fg-muted transition-colors hover:text-text-primary"
              data-testid="worktree-setup-dismiss"
              onclick={() => dismissWorktreeSetup(threadId)}
            >Dismiss</button>
          {/if}
        </div>

        <div class="min-h-0 flex-1 overflow-y-auto px-3 py-2">
          {#if view.steps.length > 0}
            <WorktreeSetupSteps steps={view.steps} statuses={view.stepStatuses} />
          {/if}
          {#if failed && view.error}
            <p
              class="mt-2 whitespace-pre-wrap break-words text-xs text-error"
              data-testid="worktree-setup-error"
            >{failedStep ? `${failedStep.label}: ` : ''}{view.error}</p>
          {/if}
          {#if view.output}
            <AnsiText
              source={view.output}
              class="mt-2 max-h-48 overflow-y-auto whitespace-pre-wrap break-words font-mono text-[0.6875rem] leading-relaxed text-text-secondary"
            />
          {/if}
        </div>
      </div>
    </div>
  {/if}
{/if}
