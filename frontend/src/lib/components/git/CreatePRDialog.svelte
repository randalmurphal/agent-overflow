<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { GitCreatePR } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import {
    askGitAttempt,
    lostAnswerAskAgainMessage,
    runGitAttempt,
    unknownAttemptMessage,
    type GitAttemptAnswer,
  } from './gitAttempt';
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
  // The attempt whose answer was lost and whose outcome is not known yet:
  // the next press asks for it rather than creating again (gitAttempt.ts).
  let lostAttempt = $state<string | null>(null);

  // The request is a fact about the CHECKOUT, so the call takes the pane's
  // workspace ref. Null means this pane names no repository; the control
  // that opens this dialog does not render in that case.
  let workspace = $derived(pane.workspace);
  // Labels follow the forge the status stream detected (PR vs MR).
  let labels = $derived(forgeLabels(pane.gitStatus.status?.forge));

  async function handleCreate() {
    const ws = workspace;
    const lost = lostAttempt;
    if ((lost === null && !title.trim()) || !ws || creating) return;
    creating = true;
    error = null;
    try {
      settle(lost !== null
        ? await askGitAttempt(ws, 'pr', lost)
        : await runGitAttempt(ws, 'pr', (attemptId) => GitCreatePR(ws, title.trim(), body.trim(), draft, attemptId)));
    } finally {
      creating = false;
    }
  }

  function settle(answer: GitAttemptAnswer): void {
    lostAttempt = answer.kind === 'lost' ? answer.attemptId : null;
    switch (answer.kind) {
      case 'done':
        addToast(
          'success',
          answer.result.prUrl ? `${labels.noun} created: ${answer.result.prUrl}` : `${labels.noun} created`,
        );
        title = '';
        body = '';
        draft = false;
        onClose();
        return;
      case 'failed':
        error = answer.message;
        return;
      case 'lost':
        error = lostAnswerAskAgainMessage(labels.noun, labels.createAction, answer);
        return;
      case 'unknown':
        error = unknownAttemptMessage(labels.noun);
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
      disabled={!title.trim() && lostAttempt === null}
      loading={creating}
      testId="create-pr-submit"
    >
      {#snippet children()}{creating ? `Creating ${labels.noun}…` : labels.createAction}{/snippet}
    </Button>
  {/snippet}
</Modal>
