<script lang="ts">
  import type { ChatBarFavorite } from '../../../stores/bindings';
  import {
    asProviderID,
    type ProviderID,
  } from '../../../types/providers';
  import ProviderIcon from '../../shared/ProviderIcon.svelte';
  import { displayModelLabel } from '../../../utils/modelLabels';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';

  interface Props {
    favorites: ChatBarFavorite[];
    activeProvider: ProviderID | null;
    currentModel?: string;
    /** Load failure for the shared list; rendered instead of a toast so it
     * persists while the store's retry curve runs. */
    error?: string | null;
    onSelectModel: (provider: ProviderID, model: string) => void;
  }

  let {
    favorites,
    activeProvider,
    currentModel,
    error = null,
    onSelectModel,
  }: Props = $props();
</script>

{#if error}
  <MenuSectionHeader label="Favorites" />
  <div class="px-3 py-1.5 text-[0.75rem] text-error" data-testid="chat-bar-favorites-error">
    Failed to load favorites: {error}
  </div>
  <MenuDivider />
{:else if favorites.length > 0}
  <MenuSectionHeader label="Favorites" />
  {#each favorites as fav (`${fav.provider ?? ''}:${fav.value}`)}
    {@const providerID = asProviderID(fav.provider)}
    <MenuItem
      label={displayModelLabel(fav.provider ?? '', fav.value, fav.label)}
      checked={providerID === activeProvider && fav.value === currentModel}
      onSelect={() => {
        if (providerID) onSelectModel(providerID, fav.value);
      }}
    >
      {#snippet icon()}
        <ProviderIcon provider={fav.provider} size={13} />
      {/snippet}
    </MenuItem>
  {/each}
  <MenuDivider />
{/if}
