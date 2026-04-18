<script lang="ts">
  // AddProjectModal = Modal shell + DirectoryBrowser + Cancel/Add buttons.
  //
  // The modal commits whatever path DirectoryBrowser most-recently reported
  // (via onSelect). If the backend rejects with an "already in use" signal
  // from ErrProjectPathInUse, we surface an inline warning and call
  // onDuplicate so the parent can highlight the existing project row
  // instead of treating it as a hard error.

  import { untrack } from 'svelte';
  import Modal from '../primitives/Modal.svelte';
  import DirectoryBrowser from './DirectoryBrowser.svelte';
  import { CreateProject } from '../../stores/bindings';
  import {
    addProjectLocal,
    getProjects,
  } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Project } from '../../types/models';

  interface Props {
    open: boolean;
    onClose: () => void;
    /** Called with the newly-created project after a successful Add. */
    onCreated?: (project: Project) => void;
    /** Called with an existing project's id when Add finds the path is
     * already registered; the parent typically scrolls to + highlights
     * that row instead of showing an error. */
    onDuplicate?: (projectId: string) => void;
    /** Starting path for the browser. Defaults to the user's home dir. */
    initialPath?: string;
  }

  let { open, onClose, onCreated, onDuplicate, initialPath = '~' }: Props = $props();

  // Snapshot the initial path so $state init doesn't read a reactive prop
  // directly. After mount, `pendingPath` is driven by DirectoryBrowser's
  // onSelect callback.
  const startingPath = untrack(() => initialPath);
  let pendingPath = $state(startingPath);
  let submitting = $state(false);
  let submitError: string | null = $state(null);
  let duplicateOf: string | null = $state(null);

  // Reset per-open so reopening after a cancel doesn't show a stale error.
  $effect(() => {
    if (open) {
      submitError = null;
      duplicateOf = null;
    }
  });

  function handleBrowserSelect(path: string): void {
    pendingPath = path;
    // Clear stale "already exists" state — the user might have navigated
    // away from the conflicting path.
    if (submitError || duplicateOf) {
      submitError = null;
      duplicateOf = null;
    }
  }

  async function handleAdd(): Promise<void> {
    if (submitting || !pendingPath.trim()) return;
    submitting = true;
    submitError = null;
    duplicateOf = null;
    try {
      const created = (await CreateProject(pendingPath.trim())) as Project;
      addProjectLocal(created);
      addToast('info', `Added project "${created.name}"`);
      onCreated?.(created);
      onClose();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      if (/already/i.test(message)) {
        // Map the ErrProjectPathInUse signal to a soft warning. Surface
        // which existing project matches so the parent can focus it.
        const existing = getProjects().find(
          (p) => p.project.path === pendingPath.trim(),
        );
        if (existing) {
          duplicateOf = existing.project.id;
          onDuplicate?.(existing.project.id);
          onClose();
          return;
        }
        submitError = 'Already a project.';
      } else {
        submitError = message;
      }
    } finally {
      submitting = false;
    }
  }

  function handleCancel(): void {
    onClose();
  }
</script>

<Modal {open} title="Add project" onClose={handleCancel} width="md">
  {#snippet children()}
    <div class="flex flex-col gap-3 min-h-[320px]">
      <p class="text-xs text-text-secondary">
        Pick a directory to track as a project. Threads created inside it
        will group here.
      </p>
      <DirectoryBrowser {initialPath} onSelect={handleBrowserSelect} />
      {#if submitError}
        <p
          role="alert"
          data-testid="add-project-error"
          class="rounded-md border border-error/40 bg-error/10 px-3 py-1.5 text-xs text-error"
        >
          {submitError}
        </p>
      {/if}
      {#if duplicateOf}
        <p
          role="status"
          data-testid="add-project-duplicate"
          class="rounded-md border border-warning/40 bg-warning/10 px-3 py-1.5 text-xs text-warning"
        >
          That path is already a project.
        </p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <button
      type="button"
      onclick={handleCancel}
      data-testid="add-project-cancel"
      class="px-4 py-1.5 text-xs rounded-md border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      Cancel
    </button>
    <button
      type="button"
      onclick={handleAdd}
      disabled={submitting || !pendingPath.trim()}
      data-testid="add-project-submit"
      data-autofocus
      class="px-4 py-1.5 text-xs rounded-md bg-accent text-surface-0 font-semibold hover:opacity-90 cursor-pointer disabled:opacity-50 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {submitting ? 'Adding…' : 'Add'}
    </button>
  {/snippet}
</Modal>
