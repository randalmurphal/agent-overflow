<script lang="ts">
  import Modal from '../../primitives/Modal.svelte';
  import Button from '../../primitives/Button.svelte';
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
</script>

<Modal
  {open}
  title={`Revert to turn ${turnIndex}`}
  onClose={onCancel}
  width="md"
  padding="comfortable"
>
  {#snippet children()}
    <div data-testid="revert-dialog">
      <p id="revert-desc-{dialogId}" class="text-[13px] text-fg-muted mb-3 leading-relaxed">
        The checkpoint captures state from just before turn {turnIndex} ran, so
        anything from turn {turnIndex} onwards is dropped.
      </p>

      <fieldset class="space-y-2">
        <legend class="sr-only">Revert scope</legend>

        <label
          class="flex items-start gap-2 rounded-[var(--radius-control)] border bg-surface-0 px-3 py-2 cursor-pointer transition-colors
            {selected === 'revert-both' ? 'border-accent/60' : 'border-border-subtle hover:border-accent/40'}"
        >
          <input
            type="radio"
            name="revert-mode-{dialogId}"
            value="revert-both"
            checked={selected === 'revert-both'}
            onchange={() => (selected = 'revert-both')}
            data-testid="revert-mode-both"
            class="mt-0.5 accent-accent"
          />
          <div class="min-w-0 flex-1">
            <div class="text-[13px] font-medium text-fg">Revert conversation and files</div>
            <div class="text-[12px] text-fg-muted leading-relaxed">
              Drop turns after this point and restore the workspace to the captured state.
            </div>
          </div>
        </label>

        <label
          class="flex items-start gap-2 rounded-[var(--radius-control)] border bg-surface-0 px-3 py-2 cursor-pointer transition-colors
            {selected === 'revert-conversation' ? 'border-accent/60' : 'border-border-subtle hover:border-accent/40'}"
        >
          <input
            type="radio"
            name="revert-mode-{dialogId}"
            value="revert-conversation"
            checked={selected === 'revert-conversation'}
            onchange={() => (selected = 'revert-conversation')}
            data-testid="revert-mode-conversation"
            class="mt-0.5 accent-accent"
          />
          <div class="min-w-0 flex-1">
            <div class="text-[13px] font-medium text-fg">Revert conversation only</div>
            <div class="text-[12px] text-fg-muted leading-relaxed">
              Drop turns after this point. Keep any file changes you want to hand-edit or
              commit. {isClaude ? 'Starts a fresh Claude session; the agent will not remember prior turns.' : ''}
            </div>
          </div>
        </label>

        <label
          class="flex items-start gap-2 rounded-[var(--radius-control)] border bg-surface-0 px-3 py-2 cursor-pointer transition-colors
            {selected === 'revert-code' ? 'border-accent/60' : 'border-border-subtle hover:border-accent/40'}"
        >
          <input
            type="radio"
            name="revert-mode-{dialogId}"
            value="revert-code"
            checked={selected === 'revert-code'}
            onchange={() => (selected = 'revert-code')}
            data-testid="revert-mode-code"
            class="mt-0.5 accent-accent"
          />
          <div class="min-w-0 flex-1">
            <div class="text-[13px] font-medium text-fg">Revert files only</div>
            <div class="text-[12px] text-fg-muted leading-relaxed">
              Restore the workspace to the captured state. Conversation stays intact so the
              agent can keep working from where it left off.
            </div>
          </div>
        </label>
      </fieldset>
    </div>
  {/snippet}
  {#snippet footer()}
    <!--
      "Fork instead" is a link-style action, not a standard CTA.
      Keeping it hand-rolled so the label reads as a secondary
      navigation choice rather than competing with Cancel/Revert.
    -->
    <button
      type="button"
      onclick={handleFork}
      data-testid="revert-fork"
      class="mr-auto text-[11px] text-fg-muted hover:text-accent cursor-pointer underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded px-1 transition-colors"
    >
      Fork instead (new thread)
    </button>
    <Button variant="secondary" size="sm" onclick={onCancel} testId="revert-cancel">
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="danger"
      size="sm"
      autofocus
      onclick={handleApply}
      testId="revert-apply"
    >
      {#snippet children()}Revert{/snippet}
    </Button>
  {/snippet}
</Modal>
