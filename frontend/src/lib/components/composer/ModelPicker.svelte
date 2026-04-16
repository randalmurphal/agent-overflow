<script lang="ts">
  import { fade, fly } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import type { ModelInfo } from '../../types/settings';
  import { GetModelsForProvider, UpdateThreadModel } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let open = $state(false);
  let models = $state<ModelInfo[]>([]);
  let loading = $state(false);
  let applying = $state(false);
  let customModel = $state('');
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let listboxEl: HTMLDivElement | undefined = $state(undefined);

  let threadId = $derived(pane.thread?.id ?? '');
  let threadModel = $derived(pane.thread?.model ?? '');
  let provider = $derived(pane.thread?.provider ?? 'claude');

  async function openPicker() {
    open = true;
    loading = true;
    try {
      const result = await GetModelsForProvider(provider);
      models = (result ?? []) as ModelInfo[];
    } catch (err) {
      console.error('Failed to load models:', err);
      addToast('error', 'Failed to load models');
      models = [];
    } finally {
      loading = false;
    }
  }

  // Focus the first option or custom input once models load
  $effect(() => {
    if (open && !loading && listboxEl) {
      const first = listboxEl.querySelector<HTMLElement>('button[role="option"], input');
      first?.focus();
    }
  });

  async function selectModel(slug: string) {
    if (!threadId) return;
    const nextModel = slug.trim();
    if (!nextModel) return;
    if (threadModel === nextModel) {
      open = false;
      triggerEl?.focus();
      return;
    }

    applying = true;
    open = false;
    triggerEl?.focus();
    try {
      const updated = await UpdateThreadModel(threadId, nextModel) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
      addToast('info', `Switched ${provider} thread to ${updated.model}.`);
    } catch (err) {
      console.error('Failed to switch model:', err);
      addToast('error', 'Failed to switch thread model');
    } finally {
      applying = false;
    }
  }

  function handleCustomSubmit() {
    if (customModel.trim()) {
      selectModel(customModel.trim());
      customModel = '';
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      open = false;
      triggerEl?.focus();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      open = false;
    }
  }
</script>

<div class="relative flex items-center gap-1.5 min-w-0">
  <span
    class="max-w-[220px] truncate rounded-full border border-border/70 bg-surface-2/70 px-2.5 py-1 text-[11px] font-medium text-text-primary"
    title={threadModel || 'No active thread model'}
  >
    {threadModel || 'No model'}
  </span>
  <button
    bind:this={triggerEl}
    onclick={openPicker}
    disabled={applying}
    class="max-w-[220px] truncate rounded-full border border-border px-2.5 py-1 text-[11px] text-text-secondary transition-colors hover:border-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-not-allowed disabled:opacity-60"
    aria-label="Change thread model"
    aria-expanded={open}
    aria-haspopup="listbox"
  >
    {applying ? 'Switching...' : 'Change model'}
  </button>

  {#if open}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div transition:fade={{ duration: 100 }} class="fixed inset-0 z-40" onclick={handleBackdropClick} onkeydown={handleKeydown}></div>
    <div bind:this={listboxEl} transition:fly={{ y: -4, duration: 120 }} class="absolute top-full left-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-xl min-w-[200px] max-h-[280px] overflow-y-auto" role="listbox" aria-label="Available models">
      <div class="border-b border-border px-3 py-2.5">
        <p class="text-[10px] font-semibold uppercase tracking-[0.18em] text-text-secondary/70">Current thread</p>
        <p class="mt-1 truncate text-xs font-medium text-text-primary" title={threadModel || 'No active thread model'}>
          {threadModel || 'No model'}
        </p>
        <p class="mt-2 text-[11px] leading-4 text-text-secondary/75">
          Changing the model restarts the active session and resumes the conversation when the provider supports it.
        </p>
      </div>
      {#if loading}
        <div class="px-3 py-2 text-xs text-text-secondary animate-pulse">Loading models...</div>
      {:else}
        {#each models as model (model.slug)}
          <button
            onclick={() => selectModel(model.slug)}
            role="option"
            aria-selected={model.slug === threadModel}
            class="w-full text-left px-3 py-1.5 text-xs hover:bg-surface-2/50 cursor-pointer flex items-center gap-2
              {model.slug === threadModel ? 'text-accent font-medium' : 'text-text-secondary hover:text-text-primary'}"
          >
            {model.name || model.slug}
            {#if model.slug === threadModel}
              <span class="ml-auto text-accent">&#10003;</span>
            {/if}
          </button>
        {/each}
        {#if models.length === 0}
          <div class="px-3 py-2 text-xs text-text-secondary/60">No models found</div>
        {:else}
          <div class="border-t border-border my-1"></div>
        {/if}
        <div class="px-3 py-2">
          <label for="custom-model-input" class="text-[10px] text-text-secondary/60 mb-1 block">Custom model</label>
          <input
            id="custom-model-input"
            type="text"
            bind:value={customModel}
            onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); handleCustomSubmit(); } }}
            placeholder="model-slug"
            class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
          />
        </div>
      {/if}
    </div>
  {/if}
</div>
