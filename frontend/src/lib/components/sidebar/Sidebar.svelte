<script lang="ts">
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { CreateThread, StartSession, GitCreateWorktree } from '../../stores/bindings';
  import { prependThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import ThreadList from './ThreadList.svelte';
  import WorkspacePicker from './WorkspacePicker.svelte';
  import { getSettings } from '../../stores/settings.svelte';

  let { pane, onOpenSettings }: { pane: ThreadPane; onOpenSettings?: () => void } = $props();

  let showForm = $state(false);
  let provider = $state<'claude' | 'codex'>('claude');
  let workspacePath = $state('');
  let model = $state('');
  let worktreeMode = $state(false);
  let worktreeBranch = $state('');
  let creating = $state(false);

  async function handleCreate() {
    if (!workspacePath.trim()) return;

    creating = true;
    try {
      const thread = await CreateThread(provider, workspacePath.trim(), model.trim()) as Thread;
      prependThread(thread);
      pane.switchThread(thread);

      // Create worktree if in worktree mode
      if (worktreeMode && worktreeBranch.trim()) {
        try {
          await GitCreateWorktree(thread.id, worktreeBranch.trim());
          addToast('info', `Worktree created on branch ${worktreeBranch.trim()}`);
        } catch (err) {
          console.error('Failed to create worktree:', err);
          pane.setError(`Failed to create worktree: ${err}`);
        }
      }

      // Start the provider session for this thread.
      try {
        await StartSession(thread.id);
      } catch (err) {
        console.error('Failed to start session:', err);
        pane.setError(`Failed to start session: ${err}`);
      }

      // Reset form.
      showForm = false;
      workspacePath = '';
      model = '';
      worktreeMode = false;
      worktreeBranch = '';
    } catch (err) {
      console.error('Failed to create thread:', err);
      pane.setError(`Failed to create thread: ${err}`);
    } finally {
      creating = false;
    }
  }

  function handleCancel() {
    showForm = false;
    workspacePath = '';
    model = '';
    worktreeMode = false;
    worktreeBranch = '';
  }

  function handleFormKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleCreate();
    }
    if (e.key === 'Escape') {
      handleCancel();
    }
  }
</script>

<aside class="w-[280px] shrink-0 bg-surface-1 border-r border-border flex flex-col h-full">
  <div class="p-3 border-b border-border">
    {#if showForm}
      <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
      <form onsubmit={(e) => { e.preventDefault(); handleCreate(); }} onkeydown={handleFormKeydown} class="space-y-2">
        <div class="flex gap-1">
          <button
            type="button"
            onclick={() => provider = 'claude'}
            class="flex-1 text-xs py-1.5 rounded cursor-pointer
              {provider === 'claude' ? 'bg-accent text-surface-0 font-medium' : 'bg-surface-2 text-text-secondary hover:text-text-primary'}"
          >
            Claude
          </button>
          <button
            type="button"
            onclick={() => provider = 'codex'}
            class="flex-1 text-xs py-1.5 rounded cursor-pointer
              {provider === 'codex' ? 'bg-accent text-surface-0 font-medium' : 'bg-surface-2 text-text-secondary hover:text-text-primary'}"
          >
            Codex
          </button>
        </div>
        <WorkspacePicker
          value={workspacePath}
          onSelect={(path) => workspacePath = path}
          recentWorkspaces={getSettings().recentWorkspaces}
        />
        <input
          type="text"
          bind:value={model}
          placeholder="Model (optional)"
          class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent"
        />
        <label class="flex items-center gap-2 text-xs text-text-secondary cursor-pointer">
          <input type="checkbox" bind:checked={worktreeMode} class="w-3 h-3 rounded cursor-pointer" />
          Worktree mode
        </label>
        {#if worktreeMode}
          <input
            type="text"
            bind:value={worktreeBranch}
            placeholder="Branch name for worktree"
            class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent"
          />
        {/if}
        <div class="flex gap-2">
          <button
            type="submit"
            disabled={!workspacePath.trim() || creating}
            class="flex-1 text-xs py-1.5 rounded bg-accent text-surface-0 font-medium hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer"
          >
            {creating ? 'Creating...' : 'Create'}
          </button>
          <button
            type="button"
            onclick={handleCancel}
            class="text-xs py-1.5 px-3 rounded border border-border text-text-secondary hover:text-text-primary cursor-pointer"
          >
            Cancel
          </button>
        </div>
      </form>
    {:else}
      <button
        onclick={() => showForm = true}
        class="w-full text-sm py-2 rounded-md bg-accent text-surface-0 font-medium hover:opacity-90 cursor-pointer"
      >
        + New Thread
      </button>
    {/if}
  </div>

  <ThreadList {pane} />

  {#if onOpenSettings}
    <div class="border-t border-border p-2 shrink-0">
      <button
        onclick={onOpenSettings}
        class="w-full flex items-center gap-2 px-2 py-1.5 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 rounded cursor-pointer"
      >
        <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="3" />
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
        </svg>
        Settings
      </button>
    </div>
  {/if}
</aside>
