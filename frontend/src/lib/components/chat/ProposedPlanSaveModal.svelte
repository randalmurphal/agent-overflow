<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import EditorLink from '../common/EditorLink.svelte';

  interface Props {
    open: boolean;
    workspacePath?: string;
    savePath: string;
    saving: boolean;
    onPathChange: (value: string) => void;
    onClose: () => void;
    onSave: () => void | Promise<void>;
  }

  let { open, workspacePath, savePath, saving, onPathChange, onClose, onSave }: Props = $props();
</script>

<Modal
  {open}
  title="Save Plan to Workspace"
  onClose={onClose}
  width="lg"
  padding="comfortable"
>
  {#snippet children()}
    <p class="text-[0.8125rem] text-fg-muted mb-4 leading-relaxed">
      Enter a path relative to
      {#if workspacePath}
        <span class="inline-flex items-center gap-1 align-baseline">
          <code class="font-mono text-[0.75rem] bg-surface-2/50 px-1 rounded">{workspacePath}</code>
          <EditorLink path={workspacePath} asIcon class="opacity-70 hover:opacity-100" />
        </span>
      {:else}
        <code class="font-mono text-[0.75rem] bg-surface-2/50 px-1 rounded">the workspace</code>
      {/if}.
    </p>

    <label class="block">
      <span class="mb-1 block text-[0.75rem] text-fg-muted font-medium">Workspace Path</span>
      <input
        data-autofocus
        value={savePath}
        disabled={saving}
        spellcheck={false}
        placeholder="plans/my-plan.md"
        oninput={(event) => onPathChange(event.currentTarget.value)}
        class="w-full text-[0.8125rem] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      />
    </label>
  {/snippet}
  {#snippet footer()}
    <Button
      variant="secondary"
      size="sm"
      onclick={onClose}
      disabled={saving}
    >
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={() => void onSave()}
      loading={saving}
    >
      {#snippet children()}{saving ? 'Saving...' : 'Save'}{/snippet}
    </Button>
  {/snippet}
</Modal>
