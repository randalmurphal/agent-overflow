<script lang="ts">
  // A submenu body that lists every model for a given provider, with a
  // checkmark on the pane's currently-active model. Split out from the
  // parent ModelProviderMenu so it can own its own `$effect` that
  // warms the shared model cache on mount (i.e. when the user opens
  // the submenu for the first time).

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { ModelInfo } from '../../../types/settings';
  import MenuItem from '../../primitives/MenuItem.svelte';

  interface Props {
    pane: ThreadPane;
    provider: 'claude' | 'codex';
    /**
     * Resolver returns the cached model list for the provider; the
     * parent reads from the same module-level cache and tracks a
     * version for invalidation.
     */
    getModels: (provider: 'claude' | 'codex') => ModelInfo[];
    /** Fetches + caches if the cache is cold. */
    ensureModels: (provider: 'claude' | 'codex') => Promise<void>;
    /** Called with the slug the user selected. */
    onSelect: (slug: string) => void;
  }

  let { pane, provider, getModels, ensureModels, onSelect }: Props = $props();

  let loading = $state(false);

  // Fire a single `ensureModels` on mount if the cache is cold. We keep
  // the guard narrow — once the cache is populated, the submenu just
  // renders directly; re-opening the submenu won't fetch again.
  $effect(() => {
    if (getModels(provider).length > 0) return;
    loading = true;
    void ensureModels(provider).finally(() => {
      loading = false;
    });
  });

  let models = $derived(getModels(provider));
  let currentModel = $derived(pane.thread?.model ?? '');
  let isActiveProvider = $derived(pane.thread?.provider === provider);
</script>

{#if loading && models.length === 0}
  <div
    class="px-3 py-2 text-xs text-text-secondary/60"
    role="presentation"
    data-testid="provider-models-loading-{provider}"
  >
    Loading…
  </div>
{:else if models.length === 0}
  <div
    class="px-3 py-2 text-xs text-text-secondary/60"
    role="presentation"
    data-testid="provider-models-empty-{provider}"
  >
    No models available
  </div>
{:else}
  {#each models as model (model.slug)}
    <MenuItem
      label={model.name || model.slug}
      checked={isActiveProvider && currentModel === model.slug}
      onSelect={() => onSelect(model.slug)}
    />
  {/each}
{/if}
