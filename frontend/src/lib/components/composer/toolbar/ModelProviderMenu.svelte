<script lang="ts">
  import { threadMachine } from '../../../stores/attachedBackends.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { ModelInfo } from '../../../types/settings';
  import type { ChatBarFavorite } from '../../../stores/bindings';
  import {
    ensureChatBarFavorites,
    peekChatBarFavorites,
    peekChatBarFavoritesError,
    setChatBarFavorite,
  } from '../../../stores/chatBarFavorites.svelte';
  import { applyThreadModelSelection } from '../../../stores/threadModelControls';
  import {
    asProviderID,
    getProviderDefinition,
    providerIsEnabled,
    PROVIDER_MODEL_MENU_ORDER,
    type ProviderID,
  } from '../../../providers/catalog';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../../stores/providerModels.svelte';
  import ProviderIcon from '../../shared/ProviderIcon.svelte';
  import { getSettings } from '../../../stores/settings.svelte';
  import { addErrorToast, addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { hiddenModelSlugs } from '../../../utils/hiddenModels';
  import { displayModelLabel } from '../../../utils/modelLabels';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuSubmenuItem from '../../primitives/MenuSubmenuItem.svelte';
  import ChatBarFavoritesSection from './ChatBarFavoritesSection.svelte';
  import ModelProviderTrigger from './ModelProviderTrigger.svelte';
  import ProviderModelsSubmenu from './ProviderModelsSubmenu.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  let backend = $derived(threadMachine(pane.threadId ?? '', pane.thread?.projectId));

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let applying = $state(false);
  // Favorites are app state, not menu state: every mounted menu derives from
  // the one shared list, so a star set in one pane lands in all of them.
  let favorites = $derived(peekChatBarFavorites());
  let favoritesError = $derived(peekChatBarFavoritesError());

  async function ensureModels(provider: ProviderID): Promise<void> {
    try {
      await ensureProviderModels(provider, backend);
    } catch (err) {
      const label = getProviderDefinition(provider).label;
      console.error('GetModelsForProvider failed:', err);
      addErrorToast(`Failed to load ${label} models`, err);
    }
  }

  function getModels(provider: ProviderID): ModelInfo[] {
    return getProviderModels(provider, backend);
  }

  function handleTrigger(): void {
    open = !open;
    if (open) {
      const provider = asProviderID(pane.thread?.provider);
      if (provider) void ensureModels(provider);
      ensureChatBarFavorites();
    }
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { paneId: pane.paneId, triggerEl });
  }

  $effect(() => {
    return registerComposerPicker(pane.paneId, 'model', {
      isOpen: () => open,
      open: () => {
        if (!pane.thread) return;
        open = true;
        const provider = asProviderID(pane.thread?.provider);
        if (provider) void ensureModels(provider);
        ensureChatBarFavorites();
      },
      close: closeMenu,
    });
  });

  // The selection itself lives in threadModelControls — shared with the
  // composer's `/model` command, so the placeholder branch and the
  // live retry on reselect cannot drift between the two entry points.
  async function handleSelectModel(
    provider: ProviderID,
    slug: string,
  ): Promise<void> {
    if (!pane.thread || applying) return;
    applying = true;
    closeMenu();
    try {
      const result = await applyThreadModelSelection(pane, provider, slug);
      if (!result.ok && result.error) addToast('error', result.error);
    } finally {
      applying = false;
    }
  }

  function isModelFavorite(provider: ProviderID, slug: string): boolean {
    return favorites.some((fav) => fav.kind === 'model' && fav.provider === provider && fav.value === slug);
  }

  async function setFavorite(fav: ChatBarFavorite, starred: boolean): Promise<void> {
    try {
      await setChatBarFavorite(fav, starred);
    } catch (err) {
      console.error('SetChatBarFavorite failed:', err);
      addErrorToast(`Failed to update favorites: ${errString(err)}`, err);
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

  let modelLabel = $derived(
    pane.activeModel
      ? displayModelLabel(pane.thread?.provider ?? '', pane.activeModel)
      : 'Provider default',
  );

  let settings = $derived(getSettings(backend));
  let isLocked = $derived(pane.isLocked);
  let activeProvider = $derived(asProviderID(pane.thread?.provider));
  let visibleFavorites = $derived.by(() => {
    // Hidden sets built once per provider per recompute, not per
    // favorite (the list holds up to 30 entries).
    const hiddenByProvider = new Map<ProviderID, ReadonlySet<string>>();
    return favorites.filter((fav) => {
      const provider = asProviderID(fav.provider);
      if (!provider) return false;
      // Favorites for hidden models are filtered, not deleted — the
      // star reappears as-is when the model is re-shown in settings.
      let hidden = hiddenByProvider.get(provider);
      if (!hidden) {
        hidden = hiddenModelSlugs(settings, provider);
        hiddenByProvider.set(provider, hidden);
      }
      if (hidden.has(fav.value)) return false;
      // A starred model is reachable exactly when its provider's own
      // submenu is — one rule, so a favorite can never be the back door
      // onto a provider the picker is not offering.
      return showProviderSubmenu(provider);
    });
  });

  // This menu is where a new thread picks its provider, so it is where a
  // provider disabled in Settings has to disappear. The pane's OWN provider
  // is exempt: turning a provider off is a statement about new work, and
  // must never strand a live thread without a model picker (an existing
  // claude-tui thread keeps every affordance it had).
  function showProviderSubmenu(provider: ProviderID): boolean {
    if (activeProvider === provider) return true;
    return !isLocked && providerIsEnabled(settings, provider);
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
      error={favoritesError}
      {activeProvider}
      currentModel={pane.activeModel}
      onSelectModel={(provider, model) => void handleSelectModel(provider, model)}
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
  </Menu>
</Popover>
