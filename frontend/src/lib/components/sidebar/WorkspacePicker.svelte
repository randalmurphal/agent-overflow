<script lang="ts">
  import { fly } from 'svelte/transition';
  import { Dialogs } from '@wailsio/runtime';
  import { addToast } from '../../stores/toast.svelte';

  let { value, onSelect, recentWorkspaces = [] }: {
    value: string;
    onSelect: (path: string) => void;
    recentWorkspaces?: string[];
  } = $props();

  let showRecent = $state(false);
  let inputEl: HTMLInputElement | undefined = $state(undefined);

  function handleInput(e: Event) {
    const target = e.target as HTMLInputElement;
    onSelect(target.value);
  }

  async function handleBrowse() {
    try {
      const result = await Dialogs.OpenFile({
        Title: 'Select workspace',
        CanChooseDirectories: true,
        CanChooseFiles: false,
        CanCreateDirectories: true,
      });
      if (result) {
        onSelect(result as string);
      }
    } catch (err) {
      console.error('Failed to open directory picker:', err);
      addToast('error', 'Failed to open directory picker');
    }
  }

  function handleFocus() {
    if (recentWorkspaces.length > 0) {
      showRecent = true;
    }
  }

  function handleBlur() {
    // Delay to allow click on dropdown items
    setTimeout(() => { showRecent = false; }, 150);
  }

  function selectRecent(path: string) {
    onSelect(path);
    showRecent = false;
  }
</script>

<div class="relative">
  <div class="flex gap-1">
    <input
      bind:this={inputEl}
      type="text"
      {value}
      oninput={handleInput}
      onfocus={handleFocus}
      onblur={handleBlur}
      placeholder="Workspace path"
      aria-label="Workspace path"
      role="combobox"
      aria-expanded={showRecent && recentWorkspaces.length > 0}
      aria-controls="workspace-recent-list"
      aria-haspopup="listbox"
      aria-autocomplete="list"
      class="flex-1 text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors min-w-0"
    />
    <button
      onclick={handleBrowse}
      type="button"
      class="text-xs px-2 py-1.5 rounded border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      title="Browse for folder"
    >
      Browse
    </button>
  </div>

  {#if showRecent && recentWorkspaces.length > 0}
    <div transition:fly={{ y: -4, duration: 120 }} id="workspace-recent-list" class="absolute top-full left-0 right-0 mt-1 z-50 bg-surface-1 border border-border rounded shadow-lg max-h-[160px] overflow-y-auto" role="listbox" aria-label="Recent workspaces">
      {#each recentWorkspaces as ws (ws)}
        <button
          onclick={() => selectRecent(ws)}
          role="option"
          aria-selected={ws === value}
          class="w-full text-left px-2 py-1.5 text-xs font-mono text-text-secondary hover:text-text-primary hover:bg-surface-2/50 cursor-pointer truncate"
        >
          {ws}
        </button>
      {/each}
    </div>
  {/if}
</div>
