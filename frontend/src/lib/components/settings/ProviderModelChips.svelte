<script lang="ts">
  // Click-to-toggle model-visibility chips for one provider's catalog.
  // Hiding is a display-only hide-list in settings (pickers filter it;
  // capability/cost lookups keep the full catalog). The last visible
  // model can't be hidden so pickers never go empty.

  import { getSettings, updateSettingsPatch } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { hiddenModelSlugs, hiddenModelsSettingsKey } from '../../utils/hiddenModels';
  import { displayModelLabel } from '../../utils/modelLabels';
  import type { ModelInfo } from '../../types/settings';
  import type { ProviderID } from '../../providers/catalog';
  import {
    CHIP_BASE_CLASS,
    CHIP_EMPTY_PROSE_CLASS,
    CHIP_EXCLUDED_CLASS,
    CHIP_RESTING_CLASS,
  } from './styles';

  interface Props {
    provider: ProviderID;
    models: ModelInfo[];
  }

  let { provider, models }: Props = $props();

  let settings = $derived(getSettings());
  let hidden = $derived(hiddenModelSlugs(settings, provider));

  async function toggleModelVisibility(slug: string): Promise<void> {
    const key = hiddenModelsSettingsKey(provider);
    if (!key) return;
    const next = new Set(settings[key] ?? []);
    if (next.has(slug)) {
      next.delete(slug);
    } else {
      const visibleCount = models.filter((model) => !next.has(model.slug)).length;
      if (visibleCount <= 1) {
        addToast('info', 'At least one model must stay visible.');
        return;
      }
      next.add(slug);
    }
    await updateSettingsPatch({ [key]: [...next] });
  }
</script>

{#if models.length > 0}
  <div
    class="flex flex-wrap gap-1.5"
    data-testid="settings-provider-models"
  >
    {#each models as model (model.slug)}
      {@const isHidden = hidden.has(model.slug)}
      <button
        type="button"
        class="{CHIP_BASE_CLASS} {isHidden ? CHIP_EXCLUDED_CLASS : CHIP_RESTING_CLASS}"
        aria-pressed={!isHidden}
        title={isHidden ? 'Hidden from pickers — click to show' : 'Shown in pickers — click to hide'}
        data-testid="settings-model-toggle-{provider}-{model.slug}"
        data-hidden={isHidden}
        onclick={() => void toggleModelVisibility(model.slug)}
      >
		{displayModelLabel(provider, model.slug, model.name)}
      </button>
    {/each}
  </div>
{:else}
  <span class={CHIP_EMPTY_PROSE_CLASS}>No models available.</span>
{/if}
