<script lang="ts">
  // Ship Changes wizard — a 3-step dialog that walks the user through
  //   Commit -> Push -> Create PR
  // without leaving the chat. The dialog is the *only* place in the UI
  // that mutates the state machine; the step components render state
  // and emit intent handlers which trigger the side effects declared
  // here.
  //
  // The dialog and the state machine stay loosely coupled on purpose:
  //   * The dialog owns the three Git* RPC calls.
  //   * The state machine owns phase transitions and error storage.
  //   * The step components only read state + call intent handlers.
  //
  // Git status is NOT fetched here. It is workspace state, read from the
  // shared store through `pane.gitStatus` and fed into the wizard by the
  // effect below, so the gates the wizard opens on (hasChanges, aheadCount,
  // openPrUrl, pendingOperation) are the same values the header badges and
  // every other pane on this worktree are showing. After each action the
  // wizard asks for one refresh through the same chokepoint, which also
  // reconciles the observed branch and heals the other clients.

  import { untrack } from 'svelte';
  import {
    GitCommit,
    GitPush,
    GitCreatePR,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { forgeLabels } from '../../utils/forgeLabels';
  import type { GitActionResult } from '../../types/git';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createShipChangesState,
    type ShipChangesState,
  } from '../../stores/shipChanges.svelte';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
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
  // Track the thread the wizard was opened for so we can detect a switch
  // while the drawer is still open and bail out rather than silently
  // resetting the user's typed content onto a different thread's state.
  let openedForThreadId: string | null = null;

  // The workspace's live status feeds the wizard. setStatus auto-advances
  // only out of `idle`, so once the user is inside a step this is purely
  // informational and cannot yank them between stages.
  $effect(() => {
    const status = pane.gitStatus.status;
    if (!open || status === null) return;
    untrack(() => wizard.setStatus(status));
  });

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

    untrack(() => {
      wizard.open(threadId);
      // Seed synchronously: open() resets to idle, and the feed effect
      // above has already run for this flush (its deps did not change
      // again), so without this the wizard would sit on "Loading…" until
      // the next status push. Null status keeps the loading state, which
      // that push then resolves.
      const status = pane.gitStatus.status;
      if (status !== null) wizard.setStatus(status);
    });
    openedForThreadId = threadId;
  });

  async function handleCommit(): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    if (!wizard.canCommit) return;
    const subject = wizard.commitSubject.trim();
    const body = wizard.commitBody.trim();
    const startGeneration = wizard.generation;
    wizard.beginCommit();
    try {
      const result = (await GitCommit(threadId, subject, body)) as GitActionResult;
      // Dialog may have been closed (or reopened on a new thread) while the
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
      await pane.gitStatus.refreshNow();
    } catch (err) {
      if (wizard.generation !== startGeneration) return;
      wizard.failCommit(err instanceof Error ? err.message : String(err));
    }
  }

  function handleSkipCommit(): void {
    wizard.skipCommit();
  }

  async function handlePush(): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    if (!wizard.canPush) return;
    const startGeneration = wizard.generation;
    wizard.beginPush();
    try {
      const result = (await GitPush(threadId)) as GitActionResult;
      if (wizard.generation !== startGeneration) return;
      if (result.error) {
        wizard.failPush(result.error);
        return;
      }
      wizard.completePush();
      addToast('success', 'Pushed');
      await pane.gitStatus.refreshNow();
    } catch (err) {
      if (wizard.generation !== startGeneration) return;
      wizard.failPush(err instanceof Error ? err.message : String(err));
    }
  }

  function handleSkipPush(): void {
    wizard.skipPush();
  }

  async function handleCreatePR(): Promise<void> {
    const threadId = pane.threadId;
    if (!threadId) return;
    if (!wizard.canCreatePR) return;
    const title = wizard.prTitle.trim();
    const body = wizard.prBody.trim();
    const draft = wizard.prDraft;
    const startGeneration = wizard.generation;
    wizard.beginCreatePR();
    try {
      const result = (await GitCreatePR(threadId, title, body, draft)) as GitActionResult;
      if (wizard.generation !== startGeneration) return;
      if (result.error) {
        wizard.failCreatePR(result.error);
        return;
      }
      wizard.completeCreatePR(result.prUrl ?? '');
      addToast('success', `${forgeLabels(wizard.status?.forge).longSingular} opened`);
      await pane.gitStatus.refreshNow();
    } catch (err) {
      if (wizard.generation !== startGeneration) return;
      wizard.failCreatePR(err instanceof Error ? err.message : String(err));
    }
  }

  function handleRetry(): void {
    wizard.retry();
  }
</script>

<Modal
  {open}
  title="Ship Changes"
  onClose={onClose}
  width="lg"
  padding="comfortable"
>
  {#snippet headerActions()}
    <StepIndicator phase={wizard.phase} forge={wizard.status?.forge} />
  {/snippet}
  {#snippet children()}
    <div data-testid="ship-changes-drawer" class="space-y-4">
      {#if wizard.phase === 'idle'}
        <p class="text-[0.75rem] text-fg-muted" data-testid="ship-changes-loading">Loading git status…</p>
      {:else if wizard.phase.startsWith('commit.')}
        <CommitStep state={wizard} onCommit={handleCommit} onSkip={handleSkipCommit} />
        {#if wizard.phase === 'commit.error'}
          <div class="flex justify-end">
            <Button
              variant="secondary"
              size="sm"
              onclick={handleRetry}
              testId="ship-changes-commit-retry"
            >
              {#snippet children()}Edit and retry{/snippet}
            </Button>
          </div>
        {/if}
      {:else if wizard.phase.startsWith('push.')}
        <PushStep state={wizard} onPush={handlePush} onSkip={handleSkipPush} />
        {#if wizard.phase === 'push.error'}
          <div class="flex justify-end">
            <Button
              variant="secondary"
              size="sm"
              onclick={handleRetry}
              testId="ship-changes-push-retry"
            >
              {#snippet children()}Retry push{/snippet}
            </Button>
          </div>
        {/if}
      {:else}
        <PRStep state={wizard} onCreate={handleCreatePR} />
        {#if wizard.phase === 'pr.error'}
          <div class="flex justify-end">
            <Button
              variant="secondary"
              size="sm"
              onclick={handleRetry}
              testId="ship-changes-pr-retry"
            >
              {#snippet children()}Edit and retry{/snippet}
            </Button>
          </div>
        {/if}
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onClose} testId="ship-changes-close">
      {#snippet children()}{wizard.finished ? 'Done' : 'Close'}{/snippet}
    </Button>
  {/snippet}
</Modal>
