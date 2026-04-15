<script lang="ts">
  import type { Thread } from '../types/models';
  import type { ThreadPane } from '../stores/thread.svelte';
  import { CreateThread, StartSession } from '../stores/bindings';
  import { prependThread } from '../stores/threads.svelte';
  import ThreadList from './ThreadList.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let showForm = $state(false);
  let provider = $state<'claude' | 'codex'>('claude');
  let workspacePath = $state('');
  let model = $state('');
  let creating = $state(false);

  async function handleCreate() {
    if (!workspacePath.trim()) return;

    creating = true;
    try {
      const thread = await CreateThread(provider, workspacePath.trim(), model.trim()) as Thread;
      prependThread(thread);
      pane.switchThread(thread);

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
        <input
          type="text"
          bind:value={workspacePath}
          placeholder="Workspace path"
          class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent"
        />
        <input
          type="text"
          bind:value={model}
          placeholder="Model (optional)"
          class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent"
        />
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
</aside>
