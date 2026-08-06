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
  import Icon from '../../primitives/Icon.svelte';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';

  interface Props {
    favorites: ChatBarFavorite[];
    activeProvider: ProviderID | null;
    currentModel?: string;
    onSelectModel: (provider: ProviderID, model: string) => void;
    onSelectDiscussion: (favorite: ChatBarFavorite) => void;
  }

  let {
    favorites,
    activeProvider,
    currentModel,
    onSelectModel,
    onSelectDiscussion,
  }: Props = $props();
</script>

{#if favorites.length > 0}
  <MenuSectionHeader label="Favorites" />
  {#each favorites as fav (`${fav.kind}:${fav.provider ?? ''}:${fav.value}`)}
    {@const providerID = asProviderID(fav.provider)}
    {@const label = fav.kind === 'model' ? displayModelLabel(fav.provider ?? '', fav.value, fav.label) : fav.label}
    <MenuItem
      {label}
      checked={fav.kind === 'model' && providerID === activeProvider && fav.value === currentModel}
      onSelect={() => {
        if (fav.kind === 'model' && providerID) {
          onSelectModel(providerID, fav.value);
        } else if (fav.kind === 'discussion') {
          onSelectDiscussion(fav);
        }
      }}
    >
      {#snippet icon()}
        {#if fav.kind === 'model'}
          <ProviderIcon provider={fav.provider} size={13} />
        {:else}
          <Icon icon={MessagesSquare} size={13} strokeWidth={1.75} />
        {/if}
      {/snippet}
    </MenuItem>
  {/each}
  <MenuDivider />
{/if}
