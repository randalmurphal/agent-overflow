<script lang="ts">
  // A submenu body that lists every model for a given provider, with a
  // checkmark on the pane's currently-active model. Split out from the
  // parent ModelProviderMenu so it can own its own `$effect` that
  // warms the shared model cache on mount (i.e. when the user opens
  // the submenu for the first time).

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { ModelInfo } from '../../../types/settings';
  import type { ProviderID } from '../../../types/providers';
  import { getSettings } from '../../../stores/settings.svelte';
  import { hiddenModelSlugs } from '../../../utils/hiddenModels';
  import { displayModelLabel } from '../../../utils/modelLabels';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import Star from 'lucide-svelte/icons/star';

  interface Props {
    pane: ThreadPane;
    provider: ProviderID;
    /**
     * Resolver returns the shared cached model list for the provider.
     */
    getModels: (provider: ProviderID) => ModelInfo[];
    /** Fetches + caches if the cache is cold. */
    ensureModels: (provider: ProviderID) => Promise<void>;
    /** Called with the slug the user selected. */
    onSelect: (slug: string) => void;
    isFavorite?: (provider: ProviderID, slug: string) => boolean;
    onToggleFavorite?: (model: ModelInfo) => void;
  }

  let {
    pane,
    provider,
    getModels,
    ensureModels,
    onSelect,
    isFavorite = () => false,
    onToggleFavorite,
  }: Props = $props();

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

  let currentModel = $derived(pane.activeModel);
  let isActiveProvider = $derived(pane.thread?.provider === provider);
  // Hidden models are dropped from the list, except the pane's active
  // model — a thread already riding a hidden model keeps its checkmark
  // row so the picker never shows "nothing selected".
  let models = $derived.by(() => {
    const all = getModels(provider);
    const hidden = hiddenModelSlugs(getSettings(), provider);
    if (hidden.size === 0) return all;
    const visible = all.filter(
      (model) =>
        !hidden.has(model.slug) ||
        (isActiveProvider && model.slug === currentModel),
    );
    // The settings UI refuses to hide the last visible model, but a
    // hand-edited settings.json (or a second connected client) can
    // still hide everything — fall back to the full catalog rather
    // than presenting an empty picker. Same backstop as the Go seed
    // path's firstVisibleModel.
    if (visible.length === 0) return all;
    return visible;
  });
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
    {@const favorite = isFavorite(provider, model.slug)}
    {@const label = displayModelLabel(provider, model.slug, model.name)}
    <MenuItem
      {label}
      checked={isActiveProvider && currentModel === model.slug}
      onSelect={() => onSelect(model.slug)}
      actionLabel={favorite ? `Remove ${label} from favorites` : `Add ${label} to favorites`}
      actionPressed={favorite}
      actionPosition="start"
      onAction={onToggleFavorite ? () => onToggleFavorite(model) : undefined}
    >
      {#snippet action()}
        <Icon
          icon={Star}
          size={13}
          strokeWidth={1.8}
          class={favorite ? 'fill-current' : ''}
        />
      {/snippet}
    </MenuItem>
  {/each}
{/if}
