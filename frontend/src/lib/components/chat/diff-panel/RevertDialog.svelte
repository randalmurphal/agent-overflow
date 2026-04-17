<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import { focusTrap } from '../../../utils/focusTrap';
  import type { RevertMode } from '../../../types/checkpoint';

  interface Props {
    open: boolean;
    turnIndex: number;
    /** Provider name of the thread, lowercased. Drives the Claude-specific note. */
    provider: string;
    /** Called when the user picks a mode and confirms. The parent performs the actual call. */
    onRevert: (mode: RevertMode) => void;
    onCancel: () => void;
  }

  let { open, turnIndex, provider, onRevert, onCancel }: Props = $props();

  // Default to the least-surprising mode: in-place revert of both conversation
  // and code. If the user wants a non-destructive branch they can click "Fork
  // instead" from the same dialog.
  let selected: Exclude<RevertMode, 'fork'> = $state('revert-both');

  // Reset the selection every time the dialog reopens so a previous pick
  // doesn't leak between sessions.
  $effect(() => {
    if (open) selected = 'revert-both';
  });

  const dialogId = crypto.randomUUID().slice(0, 8);

  const isClaude = $derived(provider === 'claude');

  function handleApply() {
    onRevert(selected);
  }

  function handleFork() {
    onRevert('fork');
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      onCancel();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) onCancel();
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
    data-testid="revert-dialog-backdrop"
  >
    <div
      use:focusTrap={{ active: open }}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="revert-title-{dialogId}"
      aria-describedby="revert-desc-{dialogId}"
      data-testid="revert-dialog"
      class="bg-surface-1 border border-border rounded-lg shadow-xl max-w-md w-full mx-4 p-5"
    >
      <h2 id="revert-title-{dialogId}" class="text-base font-semibold text-text-primary mb-1.5">
        Revert to turn {turnIndex}
      </h2>
      <p id="revert-desc-{dialogId}" class="text-sm text-text-secondary mb-3">
        The checkpoint captures state from just before turn {turnIndex} ran, so
        anything from turn {turnIndex} onwards is dropped.
      </p>

      <fieldset class="space-y-2 mb-4">
        <legend class="sr-only">Revert scope</legend>

        <label
          class="flex items-start gap-2 rounded border border-border bg-surface-0 px-3 py-2 cursor-pointer hover:border-accent/40"
          class:border-accent={selected === 'revert-both'}
        >
          <input
            type="radio"
            name="revert-mode-{dialogId}"
            value="revert-both"
            checked={selected === 'revert-both'}
            onchange={() => (selected = 'revert-both')}
            data-testid="revert-mode-both"
            class="mt-0.5"
          />
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-text-primary">Revert conversation and files</div>
            <div class="text-xs text-text-secondary">
              Drop turns after this point and restore the workspace to the captured state.
            </div>
          </div>
        </label>

        <label
          class="flex items-start gap-2 rounded border border-border bg-surface-0 px-3 py-2 cursor-pointer hover:border-accent/40"
          class:border-accent={selected === 'revert-conversation'}
        >
          <input
            type="radio"
            name="revert-mode-{dialogId}"
            value="revert-conversation"
            checked={selected === 'revert-conversation'}
            onchange={() => (selected = 'revert-conversation')}
            data-testid="revert-mode-conversation"
            class="mt-0.5"
          />
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-text-primary">Revert conversation only</div>
            <div class="text-xs text-text-secondary">
              Drop turns after this point. Keep any file changes you want to hand-edit or
              commit. {isClaude ? 'Starts a fresh Claude session; the agent will not remember prior turns.' : ''}
            </div>
          </div>
        </label>

        <label
          class="flex items-start gap-2 rounded border border-border bg-surface-0 px-3 py-2 cursor-pointer hover:border-accent/40"
          class:border-accent={selected === 'revert-code'}
        >
          <input
            type="radio"
            name="revert-mode-{dialogId}"
            value="revert-code"
            checked={selected === 'revert-code'}
            onchange={() => (selected = 'revert-code')}
            data-testid="revert-mode-code"
            class="mt-0.5"
          />
          <div class="min-w-0 flex-1">
            <div class="text-sm font-medium text-text-primary">Revert files only</div>
            <div class="text-xs text-text-secondary">
              Restore the workspace to the captured state. Conversation stays intact so the
              agent can keep working from where it left off.
            </div>
          </div>
        </label>
      </fieldset>

      <div class="flex items-center justify-between gap-2">
        <button
          type="button"
          onclick={handleFork}
          data-testid="revert-fork"
          class="text-xs text-text-secondary hover:text-accent cursor-pointer underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded px-1"
        >
          Fork instead (new thread)
        </button>
        <div class="flex gap-2">
          <button
            type="button"
            onclick={onCancel}
            data-testid="revert-cancel"
            class="px-4 py-2 text-sm rounded-md border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            Cancel
          </button>
          <button
            type="button"
            data-autofocus
            onclick={handleApply}
            data-testid="revert-apply"
            class="px-4 py-2 text-sm rounded-md font-medium bg-error text-surface-0 hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            Revert
          </button>
        </div>
      </div>
    </div>
  </div>
{/if}
