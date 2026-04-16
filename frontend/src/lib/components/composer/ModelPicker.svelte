<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ModelInfo } from '../../types/settings';
  import { GetModelsForProvider } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let open = $state(false);
  let models = $state<ModelInfo[]>([]);
  let loading = $state(false);
  let customModel = $state('');

  let currentModel = $derived(pane.thread?.model ?? '');
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

  function selectModel(slug: string) {
    // Thread model selection is read-only for now (no SetModel binding available).
    // The model will be sent with the next thread creation or via settings.
    open = false;
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
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === e.currentTarget) {
      open = false;
    }
  }
</script>

<div class="relative">
  <button
    onclick={openPicker}
    class="text-xs px-2 py-1 rounded border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer truncate max-w-[150px]"
    aria-label="Change model"
    aria-expanded={open}
    aria-haspopup="listbox"
  >
    {currentModel || 'Select model'}
  </button>

  {#if open}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="fixed inset-0 z-40" onclick={handleBackdropClick} onkeydown={handleKeydown}></div>
    <div class="absolute top-full left-0 mt-1 z-50 bg-surface-1 border border-border rounded-lg shadow-xl min-w-[200px] max-h-[280px] overflow-y-auto" role="listbox" aria-label="Available models">
      {#if loading}
        <div class="px-3 py-2 text-xs text-text-secondary animate-pulse">Loading models...</div>
      {:else}
        {#each models as model}
          <button
            onclick={() => selectModel(model.slug)}
            role="option"
            aria-selected={model.slug === currentModel}
            class="w-full text-left px-3 py-1.5 text-xs hover:bg-surface-2/50 cursor-pointer flex items-center gap-2
              {model.slug === currentModel ? 'text-accent font-medium' : 'text-text-secondary hover:text-text-primary'}"
          >
            {model.name || model.slug}
            {#if model.slug === currentModel}
              <span class="ml-auto text-accent">&#10003;</span>
            {/if}
          </button>
        {/each}
        {#if models.length > 0}
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
            class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent"
          />
        </div>
      {/if}
    </div>
  {/if}
</div>
