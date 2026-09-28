<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { GitActionResult } from '../../types/git';
  import { GitCreatePR } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { FIELD_CLASS } from './dialogFieldClass';

  let { pane, open, onClose }: {
    pane: ThreadPane;
    open: boolean;
    onClose: () => void;
  } = $props();

  // Nothing is pre-filled: the user types the title and description.
  let title = $state('');
  let body = $state('');
  let draft = $state(false);
  let creating = $state(false);
  let error = $state<string | null>(null);

  // The request is a fact about the CHECKOUT, so the call takes the pane's
  // workspace ref. Null means this pane names no repository; the control
  // that opens this dialog does not render in that case.
  let workspace = $derived(pane.workspace);
  // Labels follow the forge the status stream detected (PR vs MR).
  let labels = $derived(forgeLabels(pane.gitStatus.status?.forge));

  async function handleCreate() {
    const ws = workspace;
    if (!title.trim() || !ws || creating) return;
    creating = true;
    error = null;
    try {
      const result = await GitCreatePR(ws, title.trim(), body.trim(), draft);
      const r = result as GitActionResult;
      if (r.error) {
        error = r.error;
      } else {
        addToast(
          'success',
          r.prUrl ? `${labels.noun} created: ${r.prUrl}` : `${labels.noun} created`,
        );
        title = '';
        body = '';
        draft = false;
        onClose();
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err);
    } finally {
      creating = false;
    }
  }
</script>

<Modal {open} title={`Create ${labels.longSingularTitleCase}`} onClose={onClose} width="lg" padding="comfortable">
  {#snippet children()}
    <div class="space-y-3">
      <div>
        <label for="create-pr-title" class="text-[0.75rem] text-fg-muted block mb-1 font-medium">Title</label>
        <input
          id="create-pr-title"
          type="text"
          data-autofocus
          bind:value={title}
          disabled={creating}
          placeholder="{labels.noun} title"
          class={FIELD_CLASS}
        />
      </div>

      <div>
        <label for="create-pr-body" class="text-[0.75rem] text-fg-muted block mb-1 font-medium">Description (optional)</label>
        <textarea
          id="create-pr-body"
          bind:value={body}
          rows={5}
          disabled={creating}
          placeholder="Describe the change for reviewers…"
          class="{FIELD_CLASS} resize-none"
        ></textarea>
      </div>

      <label class="flex items-center gap-2 text-[0.75rem] text-fg-muted cursor-pointer select-none">
        <input
          id="create-pr-draft"
          type="checkbox"
          class="accent-accent"
          bind:checked={draft}
          disabled={creating}
        />
        <span>Open as draft</span>
      </label>

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
      onclick={handleCreate}
      disabled={!title.trim()}
      loading={creating}
      testId="create-pr-submit"
    >
      {#snippet children()}{creating ? `Creating ${labels.noun}…` : labels.createAction}{/snippet}
    </Button>
  {/snippet}
</Modal>
