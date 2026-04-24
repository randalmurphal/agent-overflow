<script lang="ts">
  import Modal from '../../primitives/Modal.svelte';
  import Button from '../../primitives/Button.svelte';
  import type { RevertMode } from '../../../types/checkpoint';

  interface Props {
    open: boolean;
    checkpointTurnCount: number;
    provider: string;
    reverting?: boolean;
    onRevert: (mode: RevertMode) => void;
    onCancel: () => void;
  }

  let { open, checkpointTurnCount, provider, reverting = false, onRevert, onCancel }: Props = $props();

  let selected: RevertMode = $state('conversation-and-files');
  const isClaude = $derived(provider === 'claude');

  $effect(() => {
    if (open) selected = 'conversation-and-files';
  });
</script>

<Modal
  {open}
  title={`Revert to checkpoint ${checkpointTurnCount}`}
  onClose={onCancel}
  width="md"
  padding="comfortable"
>
  {#snippet children()}
    <div data-testid="revert-dialog">
      <p class="mb-3 text-[13px] leading-relaxed text-fg-muted">
        This drops conversation turns after checkpoint {checkpointTurnCount}. Pick whether the workspace should also return to that checkpoint.
      </p>

      <fieldset class="space-y-2">
        <legend class="sr-only">Revert scope</legend>
        <label
          class="flex cursor-pointer items-start gap-2 rounded-[var(--radius-control)] border bg-surface-0 px-3 py-2 transition-colors
            {selected === 'conversation-and-files' ? 'border-accent/60' : 'border-border-subtle hover:border-accent/40'}"
        >
          <input
            type="radio"
            name="revert-mode"
            checked={selected === 'conversation-and-files'}
            disabled={reverting}
            onchange={() => (selected = 'conversation-and-files')}
            data-testid="revert-mode-conversation-and-files"
            class="mt-0.5 accent-accent"
          />
          <span class="min-w-0 flex-1">
            <span class="block text-[13px] font-medium text-fg">Conversation and files</span>
            <span class="block text-[12px] leading-relaxed text-fg-muted">
              Restore the workspace and remove newer messages, checkpoints, and provider context.
            </span>
          </span>
        </label>

        <label
          class="flex cursor-pointer items-start gap-2 rounded-[var(--radius-control)] border bg-surface-0 px-3 py-2 transition-colors
            {selected === 'conversation-only' ? 'border-accent/60' : 'border-border-subtle hover:border-accent/40'}"
        >
          <input
            type="radio"
            name="revert-mode"
            checked={selected === 'conversation-only'}
            disabled={reverting}
            onchange={() => (selected = 'conversation-only')}
            data-testid="revert-mode-conversation-only"
            class="mt-0.5 accent-accent"
          />
          <span class="min-w-0 flex-1">
            <span class="block text-[13px] font-medium text-fg">Conversation only</span>
            <span class="block text-[12px] leading-relaxed text-fg-muted">
              Keep the current files and make them the new baseline for the reverted conversation.
              {isClaude ? ' Claude starts fresh after the rollback.' : ''}
            </span>
          </span>
        </label>
      </fieldset>
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onCancel} disabled={reverting} testId="revert-cancel">
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button variant="danger" size="sm" autofocus loading={reverting} onclick={() => onRevert(selected)} testId="revert-apply">
      {#snippet children()}Revert{/snippet}
    </Button>
  {/snippet}
</Modal>
