<script lang="ts">
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import type { ModelInfo } from '../../../types/settings';
  import type { ChatBarFavorite } from '../../../stores/bindings';
  import {
    ListChatBarFavorites,
    SetChatBarFavorite,
    StartDiscussionByID,
    GetThread,
    UpdateThreadModel,
    UpdateThreadProvider,
  } from '../../../stores/bindings';
  import {
    asProviderID,
    getProviderDefinition,
    PROVIDER_MODEL_MENU_ORDER,
    type ProviderID,
  } from '../../../providers/catalog';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../../stores/providerModels.svelte';
  import ProviderIcon from '../../shared/ProviderIcon.svelte';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { displayModelLabel } from '../../../utils/modelLabels';
  import MessagesSquare from 'lucide-svelte/icons/messages-square';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuSubmenuItem from '../../primitives/MenuSubmenuItem.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import ChatBarFavoritesSection from './ChatBarFavoritesSection.svelte';
  import ModelProviderTrigger from './ModelProviderTrigger.svelte';
  import ProviderModelsSubmenu from './ProviderModelsSubmenu.svelte';
  import DiscussionsSubmenu from './DiscussionsSubmenu.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let applying = $state(false);
  let favorites: ChatBarFavorite[] = $state([]);
  let favoritesLoaded = $state(false);

  async function ensureModels(provider: ProviderID): Promise<void> {
    try {
      await ensureProviderModels(provider);
    } catch (err) {
      const label = getProviderDefinition(provider).label;
      console.error('GetModelsForProvider failed:', err);
      addToast('error', `Failed to load ${label} models`);
    }
  }

  async function ensureFavorites(): Promise<void> {
    if (favoritesLoaded) return;
    try {
      const res = (await ListChatBarFavorites()) as ChatBarFavorite[] | null;
      favorites = Array.isArray(res) ? res : [];
      favoritesLoaded = true;
    } catch (err) {
      console.error('ListChatBarFavorites failed:', err);
      addToast('error', 'Failed to load favorites');
      favorites = [];
      favoritesLoaded = true;
    }
  }

  function getModels(provider: ProviderID): ModelInfo[] {
    return getProviderModels(provider);
  }

  function handleTrigger(): void {
    open = !open;
    if (open) {
      const provider = asProviderID(pane.thread?.provider);
      if (provider) void ensureModels(provider);
      void ensureFavorites();
    }
  }

  function closeMenu(): void {
    open = false;
    // Composer-toolbar pickers sit just under the textarea; after the
    // menu closes the user is almost always going to keep typing. Send
    // focus back to the textarea so Enter / Esc / chord-toggle don't
    // strand them on a trigger button. `focusPaneComposer` is a no-op
    // if the textarea is gone (pane unmounted, thread cleared).
    if (!focusPaneComposer(pane.paneId)) triggerEl?.focus();
  }

  $effect(() => {
    return registerComposerPicker(pane.paneId, 'model', {
      isOpen: () => open,
      open: () => {
        if (!pane.thread) return;
        open = true;
        const provider = asProviderID(pane.thread?.provider);
        if (provider) void ensureModels(provider);
        void ensureFavorites();
      },
      close: closeMenu,
    });
  });

  async function handleSelectModel(
    provider: ProviderID,
    slug: string,
  ): Promise<void> {
    if (!pane.thread || applying) return;
    const threadId = pane.thread.id;
    const currentProvider = pane.thread.provider;
    const currentModel = pane.thread.model;
    if (provider === currentProvider && slug === currentModel) {
      closeMenu();
      return;
    }

    applying = true;
    try {
      if (provider !== currentProvider) {
        const afterProvider = (await UpdateThreadProvider(threadId, provider)) as Thread;
        syncThread(afterProvider);
      }
      const updated = (await UpdateThreadModel(threadId, slug)) as Thread;
      syncThread(updated);
    } catch (err) {
      console.error('model/provider update failed:', err);
      addToast('error', `Failed to switch model: ${errString(err)}`);
    } finally {
      applying = false;
      closeMenu();
    }
  }

  function isModelFavorite(provider: ProviderID, slug: string): boolean {
    return favorites.some((fav) => fav.kind === 'model' && fav.provider === provider && fav.value === slug);
  }

  function isDiscussionFavorite(id: string): boolean {
    return favorites.some((fav) => fav.kind === 'discussion' && fav.value === id);
  }

  async function setFavorite(fav: ChatBarFavorite, starred: boolean): Promise<void> {
    try {
      const updated = (await SetChatBarFavorite(fav, starred)) as ChatBarFavorite[] | null;
      favorites = Array.isArray(updated) ? updated : [];
      favoritesLoaded = true;
    } catch (err) {
      console.error('SetChatBarFavorite failed:', err);
      addToast('error', `Failed to update favorites: ${errString(err)}`);
    }
  }

  function toggleModelFavorite(provider: ProviderID, model: ModelInfo): void {
    const starred = !isModelFavorite(provider, model.slug);
    void setFavorite({
      kind: 'model',
      provider,
      value: model.slug,
      label: displayModelLabel(provider, model.slug, model.name),
      createdAt: 0,
    }, starred);
  }

  function toggleDiscussionFavorite(def: { id: string; name: string }): void {
    const starred = !isDiscussionFavorite(def.id);
    void setFavorite({
      kind: 'discussion',
      provider: '',
      value: def.id,
      label: def.name,
      createdAt: 0,
    }, starred);
  }

  async function startFavoriteDiscussion(fav: ChatBarFavorite): Promise<void> {
    if (!pane.thread) return;
    const threadId = pane.thread.id;
    closeMenu();
    try {
      await StartDiscussionByID(threadId, fav.value);
      try {
        const refreshed = (await GetThread(threadId)) as Thread;
        syncThread(refreshed);
      } catch (refreshErr) {
        console.error('Failed to refresh thread after StartDiscussionByID:', refreshErr);
      }
      addToast('info', `Started discussion "${fav.label}"`);
    } catch (err) {
      console.error('StartDiscussionByID failed:', err);
      addToast('error', `Failed to start discussion: ${errString(err)}`);
    }
  }

  let modelLabel = $derived(displayModelLabel(pane.thread?.provider ?? '', pane.thread?.model ?? 'No model'));

  let isLocked = $derived(pane.isLocked);
  let isDiscussion = $derived(pane.thread?.mode === 'discussion');
  let activeProvider = $derived(asProviderID(pane.thread?.provider));
  let showDiscussions = $derived(!isDiscussion && !isLocked);
  let visibleFavorites = $derived(favorites.filter((fav) => {
    if (fav.kind === 'discussion') return showDiscussions;
    const provider = asProviderID(fav.provider);
    if (!provider) return false;
    return !isDiscussion && (!isLocked || activeProvider === provider);
  }));

  function showProviderSubmenu(provider: ProviderID): boolean {
    return !isDiscussion && (!isLocked || activeProvider === provider);
  }
</script>

<ModelProviderTrigger
  bind:buttonEl={triggerEl}
  {open}
  disabled={!pane.thread || applying}
  provider={pane.thread?.provider ?? ''}
  {modelLabel}
  onClick={handleTrigger}
/>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Model and Provider" onClose={closeMenu}>
    <ChatBarFavoritesSection
      favorites={visibleFavorites}
      {activeProvider}
      currentModel={pane.thread?.model}
      onSelectModel={(provider, model) => void handleSelectModel(provider, model)}
      onSelectDiscussion={(favorite) => void startFavoriteDiscussion(favorite)}
    />

    {#each PROVIDER_MODEL_MENU_ORDER as provider (provider)}
      {@const definition = getProviderDefinition(provider)}
      {#if showProviderSubmenu(provider)}
        <MenuSubmenuItem label={definition.label}>
          {#snippet icon()}
            <ProviderIcon {provider} size={13} />
          {/snippet}
          {#snippet children()}
            <ProviderModelsSubmenu
              {pane}
              {provider}
              {getModels}
              {ensureModels}
              onSelect={(slug) => handleSelectModel(provider, slug)}
              isFavorite={isModelFavorite}
              onToggleFavorite={(model) => toggleModelFavorite(provider, model)}
            />
          {/snippet}
        </MenuSubmenuItem>
      {/if}
    {/each}

    {#if showDiscussions}
      <MenuDivider />
      <MenuSubmenuItem label="Discussions">
        {#snippet icon()}
          <Icon icon={MessagesSquare} size={13} strokeWidth={1.75} />
        {/snippet}
        {#snippet children()}
          <DiscussionsSubmenu
            {pane}
            onSelect={closeMenu}
            isFavorite={isDiscussionFavorite}
            onToggleFavorite={toggleDiscussionFavorite}
          />
        {/snippet}
      </MenuSubmenuItem>
    {/if}
  </Menu>
</Popover>
