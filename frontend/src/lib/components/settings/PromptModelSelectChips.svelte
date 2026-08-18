<script lang="ts">
  // Model chips for one prompt override, as a SELECTION set: a lit chip means
  // the override applies to that model. Same chip vocabulary as
  // ProviderModelChips, opposite polarity — that one is a hide-list where lit
  // means "not hidden".
  //
  // A slug the override selects but the catalog no longer offers still
  // renders (dashed, `missing`), because it is otherwise unremovable from the
  // settings file through this UI. An EMPTY catalog is not evidence of that:
  // it means the load is still in flight or failed, so nothing is marked
  // until there is a catalog to be missing from.

  import { displayModelLabel } from '../../utils/modelLabels';
  import type { ProviderID } from '../../providers/catalog';
  import type { ModelInfo } from '../../types/settings';
  import {
    CHIP_BASE_CLASS,
    CHIP_EMPTY_PROSE_CLASS,
    CHIP_RESTING_CLASS,
    CHIP_SELECTED_CLASS,
  } from './styles';

  let {
    provider,
    index,
    models,
    selected,
    onToggle,
  }: {
    provider: ProviderID;
    index: number;
    models: ModelInfo[];
    selected: string[];
    onToggle: (slug: string) => void;
  } = $props();

  let options = $derived.by(() => {
    const catalog = models.map((model) => ({
      slug: model.slug,
      label: displayModelLabel(provider, model.slug, model.name),
      missing: false,
    }));
    const listed = new Set(catalog.map((option) => option.slug));
    const orphans = selected
      .filter((slug) => !listed.has(slug))
      .map((slug) => ({
        slug,
        label: displayModelLabel(provider, slug),
        // `missing` is an assertion about the CATALOG, not about the slug: it
        // can only be made once there is a catalog to be absent from. An
        // empty one means still loading or failed, and marking every
        // selection stale then would be a claim we cannot support.
        missing: catalog.length > 0,
      }));
    return [...catalog, ...orphans];
  });
</script>

{#if options.length > 0}
  <div
    class="flex flex-wrap gap-1.5"
    role="group"
    aria-label="Models this override applies to"
    data-testid="settings-prompt-models-{provider}-{index}"
  >
    {#each options as option (option.slug)}
      {@const isSelected = selected.includes(option.slug)}
      <button
        type="button"
        class="{CHIP_BASE_CLASS} {isSelected
          ? CHIP_SELECTED_CLASS
          : CHIP_RESTING_CLASS} {option.missing ? 'border-dashed' : ''}"
        aria-pressed={isSelected}
        title={option.missing
          ? 'Not in the current catalog — click to remove'
          : isSelected
            ? 'Override applies to this model — click to remove'
            : 'Click to apply the override to this model'}
        data-testid="settings-prompt-model-{provider}-{index}-{option.slug}"
        data-selected={isSelected}
        data-missing={option.missing}
        onclick={() => onToggle(option.slug)}
      >
        {option.label}
      </button>
    {/each}
  </div>
{:else}
  <span class={CHIP_EMPTY_PROSE_CLASS}>No models available.</span>
{/if}
