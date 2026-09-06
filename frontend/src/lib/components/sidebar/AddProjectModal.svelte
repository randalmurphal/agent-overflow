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
  import Button from '../primitives/Button.svelte';
  import DirectoryBrowser from './DirectoryBrowser.svelte';
  import { addComputerProject, projectAtComputerPath } from '../../stores/computerProjects';
  import { selectedBackend } from '../../stores/selectedBackend.svelte';
  import { hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import ComputerSelect from '../primitives/ComputerSelect.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { hasScope } from '../../transport/scopes';
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
    /** Fixed starting computer for an action launched from another surface. */
    initialBackend?: BackendKey;
    /** A containing workflow has already chosen its destination. */
    lockBackend?: boolean;
  }

  let { open, onClose, onCreated, onDuplicate, initialPath = '~', initialBackend, lockBackend = false }: Props = $props();

  // Snapshot the initial path so $state init doesn't read a reactive prop
  // directly. After mount, `pendingPath` is driven by DirectoryBrowser's
  // onSelect callback.
  let browserStart = $state(untrack(() => initialPath));
  let pendingPath = $state('');
  let dialogGeneration = 0;
  let computer = $state<BackendKey>(untrack(() => initialBackend ?? selectedBackend()));
  let submitting = $state(false);
  let submitError: string | null = $state(null);
  let duplicateOf: string | null = $state(null);

  // Reset per-open so reopening after a cancel doesn't show a stale error.
  $effect(() => {
    if (open) {
      ++dialogGeneration;
      untrack(() => {
        computer = initialBackend ?? selectedBackend();
        browserStart = initialPath;
      });
      pendingPath = '';
      submitting = false;
      submitError = null;
      duplicateOf = null;
    }
    return () => { ++dialogGeneration; };
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

  function selectComputer(backend: BackendKey): void {
    if (submitting) return;
    computer = backend;
    browserStart = '~';
    pendingPath = '';
    submitError = null;
    duplicateOf = null;
  }

  async function handleAdd(): Promise<void> {
    if (submitting || !pendingPath.trim() || !hasScope('git:operate', computer)) return;
    submitting = true;
    submitError = null;
    duplicateOf = null;
    const generation = dialogGeneration;
    const backend = computer;
    const path = pendingPath.trim();
    try {
      const created = await addComputerProject(backend, path);
      addToast('info', `Added project "${created.name}".`);
      if (generation !== dialogGeneration || !open) return;
      onCreated?.(created);
      onClose();
    } catch (err) {
      if (generation !== dialogGeneration || !open) return;
      const message = err instanceof Error ? err.message : String(err);
      if (/already/i.test(message)) {
        // Map the ErrProjectPathInUse signal to a soft warning. Surface
        // which existing project matches so the parent can focus it.
        const existing = projectAtComputerPath(backend, path);
        if (existing) {
          duplicateOf = existing.id;
          onDuplicate?.(existing.id);
          onClose();
          return;
        }
        submitError = 'That folder is already a project.';
      } else {
        submitError = message;
      }
    } finally {
      if (generation === dialogGeneration) submitting = false;
    }
  }

  function handleCancel(): void {
    ++dialogGeneration;
    onClose();
  }
</script>

<Modal {open} title="Add Project" onClose={handleCancel} width="md">
  {#snippet children()}
    <div class="flex flex-col gap-3 min-h-[320px]">
      <p class="text-xs text-text-secondary">
        Pick a directory to track as a project. Threads created inside it
        will group here.
      </p>
      {#if hasMultipleBackends()}
        <ComputerSelect value={computer} onchange={selectComputer} disabled={submitting || lockBackend} scope="git:operate" />
      {/if}
      {#key computer}
        <DirectoryBrowser initialPath={browserStart} backend={computer} onSelect={handleBrowserSelect} />
      {/key}
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
    <Button
      variant="secondary"
      size="sm"
      onclick={handleCancel}
      testId="add-project-cancel"
    >
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={handleAdd}
      disabled={!pendingPath.trim() || !hasScope('git:operate', computer)}
      loading={submitting}
      testId="add-project-submit"
    >
      {#snippet children()}{submitting ? 'Adding…' : 'Add'}{/snippet}
    </Button>
  {/snippet}
</Modal>
