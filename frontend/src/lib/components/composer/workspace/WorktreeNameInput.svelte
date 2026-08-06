<script lang="ts">
  // Inline new-branch slot. Renders one of three states based on the
  // thread's worktree intent:
  //
  //   creatingBranch=true                    → text input + confirm ✓ + cancel ✕
  //   mode='new-worktree' && !creatingBranch → "+ new branch" + confirm ✓
  //   otherwise                              → nothing
  //
  // The "+ new branch" button's only job is to call enterCreateBranchMode —
  // the resulting state then re-renders this component into the text-input
  // branch. In local mode the entry point is the BranchPicker dropdown's
  // "+ New branch…" entry instead, so no button shows here.
  //
  // The confirm button materializes the staged intent immediately via the
  // same path a message send runs (applyWorktreeIntentNow); sending a
  // message remains an equivalent trigger, so the intent applies exactly
  // once whichever comes first.

  import Check from '@lucide/svelte/icons/check';
  import Plus from '@lucide/svelte/icons/plus';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../../primitives/Icon.svelte';
  import SteppedSpinner from '../../primitives/SteppedSpinner.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';
  import { isImeComposingEvent } from '../../../utils/imeComposition';
  import {
    enterCreateBranchMode,
    exitCreateBranchMode,
    setNewBranchName,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import { applyWorktreeIntentNow } from '../../../stores/worktreeIntentMaterialize';

  interface Props {
    pane: ThreadPane;
    workspaceDirty: boolean;
    workspaceLock: WorkspaceChangeLockState;
  }

  let { pane, workspaceDirty, workspaceLock }: Props = $props();

  let applying = $state(false);

  let intent = $derived(worktreeIntentForThread(pane.thread));
  let inputId = $derived(`new-branch-name-${pane.thread?.id ?? 'none'}`);
  let currentBranch = $derived(pane.thread?.branch ?? '');
  let showInput = $derived(intent.creatingBranch);
  let showButton = $derived(!intent.creatingBranch && intent.mode === 'new-worktree');
  let nameMissing = $derived(intent.creatingBranch && !intent.newBranchName.trim());
  let confirmDisabled = $derived(applying || workspaceLock.locked || nameMissing);

  let confirmLabel = $derived(
    intent.mode === 'new-worktree' ? 'Create worktree now' : 'Create branch now',
  );
  let confirmTitle = $derived.by(() => {
    if (workspaceLock.locked) return workspaceLock.reason;
    if (nameMissing) return 'Enter a branch name first';
    return `${confirmLabel} — sending a message also applies it`;
  });

  function updateBranchName(value: string): void {
    if (!pane.thread) return;
    setNewBranchName(pane.thread, value);
  }

  function cancelCreate(): void {
    if (!pane.thread) return;
    exitCreateBranchMode(pane.thread);
  }

  async function confirmCreate(): Promise<void> {
    if (confirmDisabled || !pane.thread) return;
    applying = true;
    const wasWorktree = intent.mode === 'new-worktree';
    try {
      const updated = await applyWorktreeIntentNow(pane);
      if (updated) {
        addToast(
          'info',
          wasWorktree
            ? `Created worktree on ${updated.branch || 'new branch'}`
            : `Created branch ${updated.branch || intent.newBranchName}`,
        );
      }
    } catch (err) {
      console.error('apply worktree intent failed:', err);
      addToast('error', userFacingError(err));
    } finally {
      applying = false;
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    // Enter confirms the IME candidate while composing; creating the worktree
    // here would use the pre-composition name.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter') {
      event.preventDefault();
      void confirmCreate();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      cancelCreate();
    }
  }

  function startCreate(): void {
    if (!pane.thread) return;
    enterCreateBranchMode(pane.thread, { workspaceDirty, currentBranch });
  }

  const smallButtonClasses = [
    'inline-flex h-6 w-6 shrink-0 items-center justify-center rounded border border-border bg-transparent',
    'text-fg-muted hover:border-border-strong hover:text-fg',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:border-border disabled:hover:text-fg-muted',
  ].join(' ');
</script>

{#snippet confirmButton()}
  <button
    type="button"
    aria-label={confirmLabel}
    title={confirmTitle}
    data-testid="apply-worktree-intent-button"
    disabled={confirmDisabled}
    onclick={confirmCreate}
    class={smallButtonClasses}
  >
    {#if applying}
      <SteppedSpinner size={11} />
    {:else}
      <Icon icon={Check} size={11} strokeWidth={2} />
    {/if}
  </button>
{/snippet}

{#if showInput}
  <div class="inline-flex min-w-0 items-center gap-1">
    <label class="sr-only" for={inputId}>New branch name</label>
    <input
      id={inputId}
      data-testid="worktree-branch-name-input"
      type="text"
      value={intent.newBranchName}
      placeholder="Branch Name"
      disabled={applying}
      oninput={(e) => updateBranchName((e.target as HTMLInputElement).value)}
      onkeydown={handleKeydown}
      class={[
        'h-6 w-[11rem] min-w-0 rounded border border-border bg-transparent',
        'px-2 text-[0.6875rem] text-text-primary placeholder:text-fg-hint',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
      ].join(' ')}
    />
    {@render confirmButton()}
    <button
      type="button"
      aria-label="Cancel new branch"
      title="Cancel new branch"
      data-testid="cancel-new-branch-button"
      disabled={applying}
      onclick={cancelCreate}
      class={smallButtonClasses}
    >
      <Icon icon={X} size={11} strokeWidth={2} />
    </button>
  </div>
{:else if showButton}
  <div class="inline-flex min-w-0 items-center gap-1">
    <button
      type="button"
      onclick={startCreate}
      data-testid="new-branch-toggle"
      class={[
        'inline-flex h-6 items-center gap-1 rounded border border-border bg-transparent',
        'px-2 text-[0.6875rem] text-fg-muted hover:text-fg hover:border-border-strong',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
      ].join(' ')}
    >
      <Icon icon={Plus} size={10} strokeWidth={2} />
      new branch
    </button>
    {@render confirmButton()}
  </div>
{/if}
