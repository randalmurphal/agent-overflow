<script lang="ts">
  import { CreateThread, StartSession, GitCreateWorktree, GetThread } from '../../stores/bindings';
  import { getSettings, loadSettings } from '../../stores/settings.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { prependThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Thread } from '../../types/models';
  import ProviderPicker from '../composer/ProviderPicker.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import ThreadList from './ThreadList.svelte';
  import WorkspacePicker from './WorkspacePicker.svelte';

  let {
    pane,
    onOpenSettings,
    onStartDiscussion,
  }: {
    pane: ThreadPane;
    onOpenSettings?: () => void;
    onStartDiscussion?: (thread: Thread) => void;
  } = $props();

  let showForm = $state(false);
  let provider = $state<'claude' | 'codex'>(getSettings().defaultProvider as 'claude' | 'codex');
  let workspacePath = $state('');
  let defaultModel = $derived(
    provider === 'claude' ? getSettings().defaultModelClaude : getSettings().defaultModelCodex
  );
  let model = $state('');
  let worktreeMode = $state(false);
  let worktreeBranch = $state('');
  let creating = $state(false);

  function resetForm() {
    showForm = false;
    provider = getSettings().defaultProvider as 'claude' | 'codex';
    workspacePath = '';
    model = '';
    worktreeMode = false;
    worktreeBranch = '';
  }

  function openForm() {
    provider = getSettings().defaultProvider as 'claude' | 'codex';
    workspacePath = '';
    model = '';
    worktreeMode = false;
    worktreeBranch = '';
    showForm = true;
  }

  async function handleCreate() {
    if (!workspacePath.trim()) return;

    creating = true;
    try {
      const effectiveModel = model.trim() || defaultModel;
      let thread = await CreateThread(provider, workspacePath.trim(), effectiveModel) as Thread;

      if (worktreeMode) {
        try {
          await GitCreateWorktree(thread.id, worktreeBranch.trim());
          thread = await GetThread(thread.id) as Thread;
          addToast('info', `Worktree created on branch ${thread.branch || 'forge/...'}`);
        } catch (err) {
          console.error('Failed to create worktree:', err);
          pane.setError(`Failed to create worktree: ${err}`);
        }
      }

      prependThread(thread);
      await pane.switchThread(thread);

      // Start the provider session for this thread.
      try {
        await StartSession(thread.id);
      } catch (err) {
        console.error('Failed to start session:', err);
        pane.setError(`Failed to start session: ${err}`);
      }

      await loadSettings();
      resetForm();
    } catch (err) {
      console.error('Failed to create thread:', err);
      pane.setError(`Failed to create thread: ${err}`);
    } finally {
      creating = false;
    }
  }

  function handleCancel() {
    resetForm();
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

<aside class="w-[292px] shrink-0 border-r border-border/70 bg-surface-1/70 backdrop-blur-md flex flex-col h-full">
  <div class="border-b border-border/70 p-3">
    {#if showForm}
      <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
      <form onsubmit={(e) => { e.preventDefault(); handleCreate(); }} onkeydown={handleFormKeydown} class="space-y-3 rounded-2xl border border-border/70 bg-surface-0/45 p-3 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)]">
        <div>
          <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">New Session</p>
          <p class="mt-1 text-sm text-text-primary">Create a thread and choose where it should run.</p>
        </div>
        <ProviderPicker currentProvider={provider} onSelect={(p) => provider = p as 'claude' | 'codex'} />
        <WorkspacePicker
          value={workspacePath}
          onSelect={(path) => workspacePath = path}
          recentWorkspaces={getSettings().recentWorkspaces}
        />
        <input
          type="text"
          bind:value={model}
          placeholder={defaultModel ? `Model (default: ${defaultModel})` : 'Model (optional)'}
          aria-label="Model"
          class="w-full text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
        />
        <div class="flex items-center justify-between gap-3 rounded-2xl border border-border/55 bg-surface-0/55 px-3 py-2.5">
          <div>
            <p class="text-xs font-medium text-text-primary">Worktree mode</p>
            <p class="text-[11px] text-text-secondary/70">Create an isolated git worktree for this thread.</p>
          </div>
          <ToggleSwitch
            checked={worktreeMode}
            ariaLabel="Toggle worktree mode"
            onToggle={(value) => worktreeMode = value}
          />
        </div>
        {#if worktreeMode}
          <div class="space-y-1">
            <input
              type="text"
              bind:value={worktreeBranch}
              placeholder="Branch name for worktree (optional)"
              aria-label="Branch name for worktree"
              class="w-full text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/50 shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
            />
            <p class="px-1 text-[11px] text-text-secondary/70">Leave blank to start on a temporary branch and rename it from your first message.</p>
          </div>
        {/if}
        <div class="flex gap-2">
          <button
            type="submit"
            disabled={!workspacePath.trim() || creating}
            class="flex-1 rounded-xl bg-accent px-3 py-2 text-xs font-semibold text-surface-0 shadow-[0_12px_24px_-18px_var(--accent)] hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            {creating ? 'Creating...' : 'Create'}
          </button>
          <button
            type="button"
            onclick={handleCancel}
            class="rounded-xl border border-border px-3 py-2 text-xs text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            Cancel
          </button>
        </div>
      </form>
    {:else}
      <button
        onclick={openForm}
        class="w-full rounded-xl bg-accent px-4 py-3 text-sm font-semibold text-surface-0 shadow-[0_12px_24px_-18px_var(--accent)] hover:opacity-90 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 focus-visible:ring-offset-2 focus-visible:ring-offset-surface-1"
      >
        + New Thread
      </button>
    {/if}
  </div>

  <ThreadList {pane} {onStartDiscussion} />

  {#if onOpenSettings}
    <div class="border-t border-border/70 p-2 shrink-0">
      <button
        onclick={onOpenSettings}
        class="w-full flex items-center gap-2 rounded-xl px-3 py-2 text-xs text-text-secondary hover:text-text-primary hover:bg-surface-2/50 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        <svg class="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <circle cx="12" cy="12" r="3" />
          <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
        </svg>
        Settings
      </button>
    </div>
  {/if}
</aside>
