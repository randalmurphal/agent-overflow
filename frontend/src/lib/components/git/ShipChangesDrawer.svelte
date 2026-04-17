<script lang="ts">
  // Ship Changes wizard — a 3-step drawer that walks the user through
  //   Commit -> Push -> Create PR
  // without leaving the chat. The drawer is the *only* place in the UI that
  // mutates the state machine; the step components render state and emit
  // intent handlers which trigger the side effects declared here.
  //
  // The drawer and the state machine stay loosely coupled on purpose:
  //   * The drawer owns GetGitStatus + the three Git* RPC calls.
  //   * The state machine owns phase transitions and error storage.
  //   * The step components only read state + call intent handlers.

  import { untrack } from 'svelte';
  import { fade, fly } from 'svelte/transition';
  import {
    GetGitStatus,
    GitCommit,
    GitPush,
    GitCreatePR,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { GitActionResult, GitStatus } from '../../types/git';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createShipChangesState,
    type ShipChangesState,
  } from '../../stores/shipChanges.svelte';
  import StepIndicator from './ship-changes/StepIndicator.svelte';
  import CommitStep from './ship-changes/CommitStep.svelte';
  import PushStep from './ship-changes/PushStep.svelte';
  import PRStep from './ship-changes/PRStep.svelte';

  let { open, pane, onClose, state: externalState }: {
    open: boolean;
    pane: ThreadPane;
    onClose: () => void;
    /** Optional externally-created state — used in tests. */
    state?: ShipChangesState;
  } = $props();

  // If the caller supplies a store (tests), use it; otherwise own one.
  // `untrack` resolves the initial value without subscribing to further
  // prop updates — swapping the state store mid-wizard would be a bug.
  // Named `wizard` to avoid colliding with Svelte's `$state` rune.
  const wizard: ShipChangesState = untrack(() => externalState ?? createShipChangesState());
  let dialogEl: HTMLDivElement | undefined = $state(undefined);
  let previousFocus: Element | null = null;
  let statusLoadGeneration = 0;
  // Track the thread the wizard was opened for so we can detect a switch
  // while the drawer is still open and bail out rather than silently
  // resetting the user's typed content onto a different thread's state.
  let openedForThreadId: string | null = null;

  async function refreshStatus(threadId: string): Promise<void> {
    const generation = ++statusLoadGeneration;
    try {
      const fetched = (await GetGitStatus(threadId)) as GitStatus;
      if (generation !== statusLoadGeneration) return;
      wizard.setStatus(fetched);
    } catch (err) {
      console.error('ship-changes: GetGitStatus failed', err);
      pane.setError(`Failed to load git status: ${err}`);
    }
  }

  $effect(() => {
    if (!open) {
      // Wrap mutations in untrack so the reset-on-close pass doesn't
      // re-enter the effect via its own writes.
      untrack(() => {
        wizard.close();
      });
      openedForThreadId = null;
      return;
    }
    const threadId = pane.threadId;
    if (!threadId) return;

    // The user switched the active thread while the drawer was open.
    // Silently resetting wizard state onto a different thread would
    // throw away the commit subject/body they typed. Close the drawer
    // with an informative toast so the user knows what happened.
    if (openedForThreadId !== null && openedForThreadId !== threadId) {
      untrack(() => {
        addToast('info', 'Ship Changes closed (thread switched)');
        onClose();
      });
      return;
    }

    previousFocus = document.activeElement;
    untrack(() => {
      wizard.open(threadId);
      void refreshStatus(threadId);
    });
    openedForThreadId = threadId;
  });

  function close(): void {
    if (previousFocus instanceof HTMLElement) previousFocus.focus();
    previousFocus = null;
    onClose();
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
    }
  }

  function handleBackdropClick(e: MouseEvent): void {
    if (e.target === e.currentTarget) close();
  }

  async function handleCommit(): Promise<void> {
    if (!pane.threadId) return;
    if (!wizard.canCommit) return;
    const subject = wizard.commitSubject.trim();
    const body = wizard.commitBody.trim();
    const startGeneration = wizard.generation;
    wizard.beginCommit();
    try {
      const result = (await GitCommit(pane.threadId, subject, body)) as GitActionResult;
      // Drawer may have been closed (or reopened on a new thread) while the
      // commit was in flight; resuming would blow up the state machine.
      if (wizard.generation !== startGeneration) return;
      if (result.error) {
        wizard.failCommit(result.error);
        return;
      }
      wizard.completeCommit(result.commitSha ?? '');
      // Seed the PR with the commit copy so the user can skip typing it
      // again in the common one-commit-per-PR flow.
      if (wizard.prTitle === '') wizard.seedPR(subject, body);
      addToast('success', `Committed ${(result.commitSha ?? '').slice(0, 7)}`);
      await refreshStatus(pane.threadId);
    } catch (err) {
      if (wizard.generation !== startGeneration) return;
      wizard.failCommit(err instanceof Error ? err.message : String(err));
    }
  }

  function handleSkipCommit(): void {
    wizard.skipCommit();
  }

  async function handlePush(): Promise<void> {
    if (!pane.threadId) return;
    if (!wizard.canPush) return;
    const startGeneration = wizard.generation;
    wizard.beginPush();
    try {
      const result = (await GitPush(pane.threadId)) as GitActionResult;
      if (wizard.generation !== startGeneration) return;
      if (result.error) {
        wizard.failPush(result.error);
        return;
      }
      wizard.completePush();
      addToast('success', 'Pushed');
      await refreshStatus(pane.threadId);
    } catch (err) {
      if (wizard.generation !== startGeneration) return;
      wizard.failPush(err instanceof Error ? err.message : String(err));
    }
  }

  function handleSkipPush(): void {
    wizard.skipPush();
  }

  async function handleCreatePR(): Promise<void> {
    if (!pane.threadId) return;
    if (!wizard.canCreatePR) return;
    const title = wizard.prTitle.trim();
    const body = wizard.prBody.trim();
    const draft = wizard.prDraft;
    const startGeneration = wizard.generation;
    wizard.beginCreatePR();
    try {
      const result = (await GitCreatePR(pane.threadId, title, body, draft)) as GitActionResult;
      if (wizard.generation !== startGeneration) return;
      if (result.error) {
        wizard.failCreatePR(result.error);
        return;
      }
      wizard.completeCreatePR(result.prUrl ?? '');
      addToast('success', 'Pull request opened');
      await refreshStatus(pane.threadId);
    } catch (err) {
      if (wizard.generation !== startGeneration) return;
      wizard.failCreatePR(err instanceof Error ? err.message : String(err));
    }
  }

  function handleRetry(): void {
    wizard.retry();
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    data-testid="ship-changes-backdrop"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      bind:this={dialogEl}
      transition:fly={{ y: 12, duration: 160 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="ship-changes-title"
      data-testid="ship-changes-drawer"
      class="bg-surface-1 border border-border rounded-lg shadow-xl w-full max-w-lg mx-4 p-5 space-y-4"
    >
      <header class="flex items-center justify-between gap-3">
        <h2 id="ship-changes-title" class="text-base font-semibold text-text-primary">
          Ship Changes
        </h2>
        <StepIndicator phase={wizard.phase} />
      </header>

      {#if wizard.phase === 'idle'}
        <p class="text-xs text-text-secondary" data-testid="ship-changes-loading">Loading git status…</p>
      {:else if wizard.phase.startsWith('commit.')}
        <CommitStep state={wizard} onCommit={handleCommit} onSkip={handleSkipCommit} />
        {#if wizard.phase === 'commit.error'}
          <div class="flex justify-end">
            <button
              type="button"
              onclick={handleRetry}
              data-testid="ship-changes-commit-retry"
              class="px-3 py-2 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Edit and retry
            </button>
          </div>
        {/if}
      {:else if wizard.phase.startsWith('push.')}
        <PushStep state={wizard} onPush={handlePush} onSkip={handleSkipPush} />
        {#if wizard.phase === 'push.error'}
          <div class="flex justify-end">
            <button
              type="button"
              onclick={handleRetry}
              data-testid="ship-changes-push-retry"
              class="px-3 py-2 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Retry push
            </button>
          </div>
        {/if}
      {:else}
        <PRStep state={wizard} onCreate={handleCreatePR} />
        {#if wizard.phase === 'pr.error'}
          <div class="flex justify-end">
            <button
              type="button"
              onclick={handleRetry}
              data-testid="ship-changes-pr-retry"
              class="px-3 py-2 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              Edit and retry
            </button>
          </div>
        {/if}
      {/if}

      <footer class="flex justify-end gap-2 pt-1">
        <button
          type="button"
          onclick={close}
          data-testid="ship-changes-close"
          class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {wizard.finished ? 'Done' : 'Close'}
        </button>
      </footer>
    </div>
  </div>
{/if}
