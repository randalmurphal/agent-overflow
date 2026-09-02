<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { GitActionResult } from '../../types/git';
  import { GenerateCommitMessage, GitCommit } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  let { pane, open, onClose }: {
    pane: ThreadPane;
    open: boolean;
    onClose: () => void;
  } = $props();

  let subject = $state('');
  let body = $state('');
  let committing = $state(false);
  let generating = $state(false);
  let error = $state<string | null>(null);

  // The commit is a fact about the CHECKOUT, so both calls take the pane's
  // workspace ref. Null means this pane names no repository; the control
  // that opens this dialog does not render in that case.
  let workspace = $derived(pane.workspace);

  async function handleGenerate() {
    const ws = workspace;
    if (generating || committing || !ws) return;
    generating = true;
    error = null;
    try {
      const message = await GenerateCommitMessage(ws);
      subject = message.subject ?? '';
      body = message.body ?? '';
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      addToast('error', `Couldn't generate commit message: ${reason}`);
    } finally {
      generating = false;
    }
  }

  async function handleCommit() {
    const ws = workspace;
    if (!subject.trim() || !ws || committing) return;
    committing = true;
    error = null;
    try {
      const result = await GitCommit(ws, subject.trim(), body.trim());
      const r = result as GitActionResult;
      if (r.error) {
        error = r.error;
      } else {
        addToast('success', `Committed ${r.commitSha?.slice(0, 7) ?? ''}`);
        subject = '';
        body = '';
        onClose();
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      committing = false;
    }
  }

  const FIELD_CLASS =
    'w-full text-[0.8125rem] rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-1.5 ' +
    'text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 ' +
    'transition-colors';
</script>

<Modal {open} title="Commit Changes" onClose={onClose} width="lg" padding="comfortable">
  {#snippet children()}
    <div class="space-y-3">
      <div>
        <div class="flex items-center justify-between gap-2 mb-1">
          <label for="commit-subject" class="text-[0.75rem] text-fg-muted font-medium">Subject</label>
          <button
            type="button"
            data-testid="commit-dialog-generate"
            onclick={handleGenerate}
            disabled={generating || committing}
            title="Ask the agent to draft a commit message from the current diff"
            class="text-[0.625rem] px-2 py-0.5 rounded border border-border-subtle text-fg-muted hover:text-accent hover:border-accent/40 cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            {generating ? 'Generating…' : 'Generate'}
          </button>
        </div>
        <input
          id="commit-subject"
          type="text"
          data-autofocus
          bind:value={subject}
          maxlength={72}
          placeholder="Brief description of changes"
          class={FIELD_CLASS}
        />
        <span class="text-[0.625rem] text-fg-hint mt-0.5 block text-right tabular-nums">{subject.length}/72</span>
      </div>

      <div>
        <label for="commit-body" class="text-[0.75rem] text-fg-muted block mb-1 font-medium">Body (optional)</label>
        <textarea
          id="commit-body"
          bind:value={body}
          rows={4}
          placeholder="Extended description…"
          class="{FIELD_CLASS} resize-none"
        ></textarea>
      </div>

      {#if error}
        <p class="text-[0.75rem] text-error break-words" role="alert">{error}</p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onClose}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={handleCommit}
      disabled={!subject.trim()}
      loading={committing}
    >
      {#snippet children()}{committing ? 'Committing…' : 'Commit'}{/snippet}
    </Button>
  {/snippet}
</Modal>
