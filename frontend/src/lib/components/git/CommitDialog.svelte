<script lang="ts">
  import { untrack } from 'svelte';
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import Kbd from '../primitives/Kbd.svelte';
  import CommitFileList from './CommitFileList.svelte';
  import CommitFilePreview from './CommitFilePreview.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { projectHasScope } from '../../transport/entityScopes';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { FIELD_CLASS, FIELD_ACTION_CLASS } from './dialogFieldClass';
  import { createCommitDialogState, type CommitDialogState } from './commitDialogState.svelte';

  let { pane, open, onClose }: {
    pane: ThreadPane;
    open: boolean;
    onClose: () => void;
  } = $props();

  // The draft survives closing the dialog; a commit clears it.
  let subject = $state('');
  let body = $state('');

  // The commit is a fact about the CHECKOUT, so every call takes the pane's
  // workspace ref. Null means this pane names no repository; the control
  // that opens this dialog does not render in that case.
  let workspace = $derived(pane.workspace);
  let workspaceKey = $derived(workspace ? `${workspace.projectId}\n${workspace.workspacePath}` : '');

  // One state per opening on one checkout (commitDialogState.svelte.ts).
  let dialog = $state.raw<CommitDialogState | null>(null);
  $effect(() => {
    if (!open || !workspaceKey) return;
    const ws = untrack(() => workspace);
    if (!ws) return;
    const opened = createCommitDialogState(ws, () => onClose());
    dialog = opened;
    return () => {
      opened.dispose();
      if (dialog === opened) dialog = null;
    };
  });

  // Lists on open, and again on every git status change (an agent's edit,
  // a commit from a terminal).
  $effect(() => {
    void pane.gitStatus.status;
    const current = dialog;
    untrack(() => current?.refresh());
  });

  let files = $derived(dialog?.files ?? []);
  let wholeOnly = $derived(dialog?.wholeOnly ?? false);
  let loaded = $derived(dialog?.loaded ?? false);
  let activeFile = $derived(files.find((file) => file.path === dialog?.active) ?? null);
  let selectedCount = $derived(wholeOnly ? files.length : dialog?.selected.size ?? 0);
  let allSelected = $derived(files.length > 0 && selectedCount === files.length);
  let hasSelection = $derived(!!dialog?.selection);
  let canCommit = $derived(hasSelection && subject.trim() !== '');

  // Informational only: git itself refuses a commit it cannot record
  // (unresolved conflicts) and that refusal surfaces as the commit error.
  let pendingNotice = $derived.by(() => {
    switch (pane.gitStatus.status?.pendingOperation ?? '') {
      case 'merge':
        return 'A merge is in progress. Committing completes the merge.';
      case 'cherry-pick':
        return 'A cherry-pick is in progress. Committing completes it.';
      case 'revert':
        return 'A revert is in progress. Committing completes it.';
      case 'rebase':
        return 'A rebase is in progress. Use `git rebase --continue` to proceed after committing.';
      case 'bisect':
        return 'A bisect is in progress. Run `git bisect reset` when done.';
      default:
        return '';
    }
  });

  async function handleGenerate() {
    try {
      const message = await dialog?.generate();
      if (!message) return;
      subject = message.subject;
      body = message.body;
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      addErrorToast(`Couldn't generate commit message: ${reason}`, err);
    }
  }

  async function handleCommit() {
    const sha = await dialog?.commit(subject, body);
    if (sha === null || sha === undefined) return;
    addToast('success', `Committed ${sha.slice(0, 7)}`);
    subject = '';
    body = '';
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void handleCommit();
    }
  }
</script>

<Modal {open} title="Commit Changes" onClose={onClose} width="xl" padding="comfortable">
  {#snippet children()}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="space-y-3" onkeydown={handleKeydown}>
      {#if pendingNotice}
        <p
          class="text-[0.75rem] text-fg-muted bg-info/10 border border-info/40 rounded-[var(--radius-control)] px-3 py-1.5"
          role="status"
          data-testid="commit-dialog-pending-operation"
        >
          {pendingNotice}
        </p>
      {/if}

      <div>
        <div class="flex items-center justify-between gap-2 mb-1">
          <span class="text-[0.75rem] text-fg-muted font-medium" data-testid="commit-dialog-selection">
            {#if !loaded}
              Files
            {:else if wholeOnly}
              All {files.length} {files.length === 1 ? 'file' : 'files'} · committed together
            {:else}
              {selectedCount} of {files.length} {files.length === 1 ? 'file' : 'files'} selected
            {/if}
          </span>
          {#if loaded && !wholeOnly && files.length > 0}
            <button
              type="button"
              data-testid="commit-dialog-toggle-all"
              onclick={() => dialog?.toggleAll()}
              class="{FIELD_ACTION_CLASS} compact:select-none"
            >
              {allSelected ? 'Select none' : 'Select all'}
            </button>
          {/if}
        </div>
        <div class="grid h-64 grid-cols-[minmax(0,2fr)_minmax(0,3fr)] gap-2 max-[720px]:h-auto max-[720px]:grid-cols-1">
          <div class="min-h-0 max-[720px]:h-40">
            <CommitFileList
              {files}
              selected={dialog?.selected ?? new Set()}
              active={dialog?.active ?? null}
              locked={wholeOnly}
              onActivate={(path) => dialog?.activate(path)}
              onToggle={(path) => dialog?.toggle(path)}
              onToggleAll={() => dialog?.toggleAll()}
            />
          </div>
          <div class="min-h-0 max-[720px]:h-40">
            {#if workspace}
              <CommitFilePreview
                {workspace}
                backend={threadMachine(pane.threadId ?? '', workspace.projectId)}
                file={activeFile}
                ungranted={!projectHasScope('files:read', workspace.projectId)}
              />
            {/if}
          </div>
        </div>
        {#if dialog?.listError}
          <p class="mt-1 text-[0.75rem] text-error break-words" role="alert" data-testid="commit-dialog-list-error">{dialog.listError}</p>
        {:else if loaded && files.length === 0}
          <p class="mt-1 text-[0.75rem] text-fg-muted" data-testid="commit-dialog-empty">No changes to commit.</p>
        {:else if dialog && dialog.total > files.length}
          <p class="mt-1 text-[0.75rem] text-fg-muted" data-testid="commit-dialog-truncated">
            Showing the first {files.length.toLocaleString()} of {dialog.total.toLocaleString()} changed files.
          </p>
        {:else}
          <p class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[0.625rem] text-fg-hint compact:hidden">
            <span><Kbd>{#snippet children()}space{/snippet}</Kbd> select</span>
            <span><Kbd>{#snippet children()}a{/snippet}</Kbd> all</span>
            <span><Kbd>{#snippet children()}ctrl enter{/snippet}</Kbd> commit</span>
          </p>
        {/if}
      </div>

      <div>
        <div class="flex items-center justify-between gap-2 mb-1">
          <label for="commit-subject" class="text-[0.75rem] text-fg-muted font-medium">Subject</label>
          <button
            type="button"
            data-testid="commit-dialog-generate"
            onclick={handleGenerate}
            disabled={!hasSelection || dialog?.generating || dialog?.committing}
            title={hasSelection
              ? 'Ask the agent to draft a commit message from the selected files'
              : 'Select the files to describe'}
            class={FIELD_ACTION_CLASS}
          >
            {dialog?.generating ? 'Generating…' : 'Generate'}
          </button>
        </div>
        <input
          id="commit-subject"
          type="text"
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
          rows={3}
          placeholder="Extended description…"
          class="{FIELD_CLASS} resize-none"
        ></textarea>
      </div>

      {#if dialog?.commitError}
        <p class="text-[0.75rem] text-error break-words" role="alert">{dialog.commitError}</p>
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
      disabled={!canCommit}
      loading={dialog?.committing ?? false}
    >
      {#snippet children()}{dialog?.committing ? 'Committing…' : 'Commit'}{/snippet}
    </Button>
  {/snippet}
</Modal>
