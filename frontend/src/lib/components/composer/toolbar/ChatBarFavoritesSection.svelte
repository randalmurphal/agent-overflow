<script lang="ts">
  import type { ChatBarFavorite } from '../../../stores/bindings';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import ClaudeIcon from '../../primitives/brand/ClaudeIcon.svelte';
  import OpenAIIcon from '../../primitives/brand/OpenAIIcon.svelte';
  import MessagesSquare from 'lucide-svelte/icons/messages-square';

  interface Props {
    favorites: ChatBarFavorite[];
    activeProvider: string | null;
    currentModel?: string;
    onSelectModel: (provider: 'claude' | 'codex', model: string) => void;
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
    <MenuItem
      label={fav.label}
      checked={fav.kind === 'model' && fav.provider === activeProvider && fav.value === currentModel}
      onSelect={() => {
        if (fav.kind === 'model' && (fav.provider === 'claude' || fav.provider === 'codex')) {
          onSelectModel(fav.provider, fav.value);
        } else if (fav.kind === 'discussion') {
          onSelectDiscussion(fav);
        }
      }}
    >
      {#snippet icon()}
        {#if fav.kind === 'model' && fav.provider === 'claude'}
          <ClaudeIcon size={13} class="text-[#d97757] opacity-95" />
        {:else if fav.kind === 'model'}
          <OpenAIIcon size={13} class="opacity-95" />
        {:else}
          <Icon icon={MessagesSquare} size={13} strokeWidth={1.75} />
        {/if}
      {/snippet}
    </MenuItem>
  {/each}
  <MenuDivider />
{/if}
