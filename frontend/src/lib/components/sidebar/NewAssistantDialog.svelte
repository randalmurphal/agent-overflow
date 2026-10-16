<script lang="ts">
  import { untrack } from 'svelte';
  // The New Assistant dialog (docs/specs/standing-agents.md, "Sidebar"): a
  // name, the project and the workspace, and Create. The backend creates the
  // record, the home and the thread, and queues the opening message that
  // starts the discussion of the assistant's role; a name in use is refused
  // inline naming the assistant that holds it.
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import { CreateAssistant, GitListWorktrees, type WorktreeListItem } from '../../stores/bindings';
  import { closeNewAssistantDialog, getNewAssistantRequest } from '../../stores/newAssistant.svelte';
  import { getProjects } from '../../stores/projects.svelte';
  import { prependThread } from '../../stores/threads.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import { openThreadInNewPane, openThreadInPane } from '../../stores/panes.svelte';
  import { requireEntityBackend, withBackendTarget } from '../../transport/backends';
  import { projectBackend } from '../../transport/entityIndex';
  import { projectHasScope } from '../../transport/entityScopes';
  import { isPassiveConnectionFailure } from '../../transport/passiveReadFailure';
  import type { Thread } from '../../types/models';
  import { userFacingError } from '../../utils/userFacingError';
  import { workspaceRefForProject } from '../../utils/workspaceKey';
  import { generateWorktreeBranchName } from '../../utils/worktreeBranchName';
  import { pathBasename } from '../../utils/pathDisplay';

  const ROOT = 'root';
  const NEW_WORKTREE = 'new';
  const SELECT_CLASS = 'min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-2 text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
  const INPUT_CLASS = 'min-w-0 flex-1 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1.5 text-sm text-fg placeholder:text-fg-hint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';

  let request = $derived(getNewAssistantRequest());
  let name = $state('');
  let projectId = $state('');
  let workspace = $state(ROOT);
  let branch = $state('');
  let worktrees: WorktreeListItem[] = $state([]);
  let submitting = $state(false);
  let error: string | null = $state(null);
  let listGeneration = 0;

  let projects = $derived(getProjects());
  let ungranted = $derived(projectId !== '' && !projectHasScope('threads:autonomy', projectId));
  let canCreate = $derived(
    name.trim() !== '' && projectId !== '' && !ungranted && !submitting
      && (workspace !== NEW_WORKTREE || branch.trim() !== ''),
  );

  // Each opening starts clean, on the project the action came from. Only
  // the opening is tracked: the reset reads state it must not re-run on.
  $effect(() => {
    const current = request;
    if (!current) return;
    untrack(() => {
      name = '';
      error = null;
      submitting = false;
      branch = generateWorktreeBranchName();
      selectProject(current.projectId);
    });
  });

  function selectProject(id: string): void {
    projectId = id;
    workspace = ROOT;
    worktrees = [];
    const generation = ++listGeneration;
    const ws = workspaceRefForProject(id, '');
    if (!ws) return;
    let backend;
    try {
      backend = requireEntityBackend(projectBackend(id));
    } catch {
      return;
    }
    void withBackendTarget(backend, () => GitListWorktrees(ws)).then((rows) => {
      if (generation !== listGeneration) return;
      worktrees = ((rows ?? []) as WorktreeListItem[]).filter((row) => !row.missing);
    }).catch((err: unknown) => {
      if (generation !== listGeneration || isPassiveConnectionFailure(err)) return;
      error = `The project's worktrees could not be listed: ${userFacingError(err)}`;
    });
  }

  function close(): void {
    listGeneration++;
    closeNewAssistantDialog();
  }

  async function create(): Promise<void> {
    const current = request;
    if (!current || !canCreate) return;
    submitting = true;
    error = null;
    const worktree = worktrees.find((row) => row.path === workspace);
    const options = {
      projectId,
      name: name.trim(),
      ...(workspace === NEW_WORKTREE ? { worktreeBranch: branch.trim() } : {}),
      ...(worktree ? { worktreePath: worktree.path, branch: worktree.branch } : {}),
    };
    try {
      const backend = requireEntityBackend(projectBackend(projectId));
      const thread = (await withBackendTarget(backend, () => CreateAssistant(options))) as Thread;
      prependThread(thread);
      expandProject(projectId);
      closeNewAssistantDialog();
      if (current.openInNewPane) await openThreadInNewPane(thread);
      else await openThreadInPane(thread, current.targetPane ?? undefined);
    } catch (err) {
      console.error('Failed to create an assistant:', err);
      error = userFacingError(err);
      submitting = false;
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.isComposing) {
      e.preventDefault();
      void create();
    }
  }
</script>

<Modal open={request !== null} title="New Assistant" onClose={close} width="md">
  {#snippet children()}
    <div class="flex flex-col gap-3 text-xs text-fg-muted" data-testid="new-assistant-dialog">
      <p>
        An assistant is a standing thread with its own home folder. It starts by discussing its role with you.
      </p>
      <label class="flex min-w-0 items-center gap-3">
        <span class="w-20 shrink-0">Name</span>
        <!-- svelte-ignore a11y_autofocus -->
        <input
          type="text"
          class={INPUT_CLASS}
          bind:value={name}
          onkeydown={handleKeydown}
          oninput={() => { error = null; }}
          autofocus
          maxlength="200"
          aria-label="Name"
          aria-invalid={error !== null}
          data-testid="new-assistant-name"
        />
      </label>
      <label class="flex min-w-0 items-center gap-3">
        <span class="w-20 shrink-0">Project</span>
        <select
          class={SELECT_CLASS}
          value={projectId}
          aria-label="Project"
          data-testid="new-assistant-project"
          onchange={(e) => selectProject(e.currentTarget.value)}
        >
          {#each projects as entry (entry.project.id)}
            <option value={entry.project.id}>{entry.project.name}</option>
          {/each}
        </select>
      </label>
      <label class="flex min-w-0 items-center gap-3">
        <span class="w-20 shrink-0">Workspace</span>
        <select
          class={SELECT_CLASS}
          value={workspace}
          onchange={(e) => { workspace = e.currentTarget.value; }}
          aria-label="Workspace"
          data-testid="new-assistant-workspace"
        >
          <option value={ROOT}>Project root</option>
          {#each worktrees as worktree (worktree.path)}
            <option value={worktree.path}>{worktree.branch || pathBasename(worktree.path)}</option>
          {/each}
          <option value={NEW_WORKTREE}>New worktree</option>
        </select>
      </label>
      {#if workspace === NEW_WORKTREE}
        <label class="flex min-w-0 items-center gap-3">
          <span class="w-20 shrink-0">Branch</span>
          <input
            type="text"
            class={INPUT_CLASS}
            bind:value={branch}
            onkeydown={handleKeydown}
            aria-label="Branch"
            data-testid="new-assistant-branch"
          />
        </label>
      {/if}
      {#if error}
        <p
          role="alert"
          data-testid="new-assistant-error"
          class="rounded-md border border-error/40 bg-error/10 px-3 py-1.5 text-xs text-error"
        >
          {error}
        </p>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={close} testId="new-assistant-cancel">
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      onclick={() => void create()}
      disabled={!canCreate}
      loading={submitting}
      title={ungranted ? 'Not granted to this device' : undefined}
      testId="new-assistant-create"
    >
      {#snippet children()}{submitting ? 'Creating…' : 'Create'}{/snippet}
    </Button>
  {/snippet}
</Modal>
