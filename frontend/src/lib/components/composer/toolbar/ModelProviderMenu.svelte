<script lang="ts">
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import type { ModelInfo } from '../../../types/settings';
  import type { ChatBarFavorite } from '../../../stores/bindings';
  import type { DiscussionDefinition } from '../../../types/discussion';
  import {
    ListDiscussionsForThread,
    StartDiscussionByID,
    GetThread,
  } from '../../../stores/bindings';
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
  import { syncThread } from '../../../stores/panes.svelte';
  import { getSettings } from '../../../stores/settings.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { hiddenModelSlugs } from '../../../utils/hiddenModels';
  import { displayModelLabel } from '../../../utils/modelLabels';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
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
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let applying = $state(false);
  // Favorites are app state, not menu state: every mounted menu derives from
  // the one shared list, so a star set in one pane lands in all of them.
  let favorites = $derived(peekChatBarFavorites());
  let favoritesError = $derived(peekChatBarFavoritesError());
  let discussionDefs: DiscussionDefinition[] = $state([]);
  let discussionDefsError: string | null = $state(null);
  let discussionsLoadGeneration = 0;

  async function ensureModels(provider: ProviderID): Promise<void> {
    try {
      await ensureProviderModels(provider);
    } catch (err) {
      const label = getProviderDefinition(provider).label;
      console.error('GetModelsForProvider failed:', err);
      addToast('error', `Failed to load ${label} models`);
    }
  }

  // Unlike the favorites store there is no shared cache: definitions can
  // be created in Settings mid-session, and this is a cheap local query,
  // so every menu open refetches. A draft placeholder can't start a
  // discussion at all, so the entry is simply hidden for it without a
  // round-trip. This must read pane.threadId (null until the draft
  // materializes) — pane.thread.id is the synthetic `draft:…` id, which
  // the backend rejects with a no-rows error, and that error would
  // force the Discussions entry visible just to display it.
  async function ensureDiscussions(): Promise<void> {
    const threadID = pane.threadId ?? '';
    if (threadID === '') {
      discussionDefs = [];
      discussionDefsError = null;
      return;
    }
    const generation = ++discussionsLoadGeneration;
    try {
      const scoped = (await ListDiscussionsForThread(threadID)) as DiscussionDefinition[] | null;
      if (generation !== discussionsLoadGeneration) return;
      // Merge + dedupe by id — the backend may surface the same row in
      // both scopes depending on how the user seeded it.
      const merged: DiscussionDefinition[] = scoped ?? [];
      const byId = new Map<string, DiscussionDefinition>();
      for (const d of merged) {
        if (!byId.has(d.id)) byId.set(d.id, d);
      }
      discussionDefs = Array.from(byId.values());
      discussionDefsError = null;
    } catch (err) {
      if (generation !== discussionsLoadGeneration) return;
      // Stale-while-revalidate: keep whatever defs were last loaded
      // successfully rather than clearing them on a transient error.
      discussionDefsError = err instanceof Error ? err.message : String(err);
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
      ensureChatBarFavorites();
      void ensureDiscussions();
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
        void ensureDiscussions();
      },
      close: closeMenu,
    });
  });

  // The selection itself lives in threadModelControls — shared with the
  // composer's `/model` command, so the placeholder branch and the
  // reconnect-on-reselect rule cannot drift between the two entry points.
  async function handleSelectModel(
    provider: ProviderID,
    slug: string,
  ): Promise<void> {
    if (!pane.thread || applying) return;
    applying = true;
    try {
      const result = await applyThreadModelSelection(pane, provider, slug);
      if (!result.ok && result.error) addToast('error', result.error);
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
      await setChatBarFavorite(fav, starred);
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
    closeMenu();
    try {
      const threadId = pane.threadId;
      if (!threadId) {
        addToast('info', 'Start the thread before adding a discussion.');
        return;
      }
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

  let modelLabel = $derived(
    pane.activeModel
      ? displayModelLabel(pane.thread?.provider ?? '', pane.activeModel)
      : 'Provider default',
  );

  let settings = $derived(getSettings());
  let isLocked = $derived(pane.isLocked);
  let isDiscussion = $derived(pane.thread?.mode === 'discussion');
  let activeProvider = $derived(asProviderID(pane.thread?.provider));
  // The error branch keeps the entry visible even with zero loaded defs
  // so a failed fetch surfaces its error inside the submenu instead of
  // silently hiding the feature — errors are user-facing state, never
  // silent.
  let showDiscussions = $derived(
    !isDiscussion && !isLocked && (discussionDefs.length > 0 || discussionDefsError !== null),
  );
  let visibleFavorites = $derived.by(() => {
    // Hidden sets built once per provider per recompute, not per
    // favorite (the list holds up to 30 entries).
    const hiddenByProvider = new Map<ProviderID, ReadonlySet<string>>();
    return favorites.filter((fav) => {
      if (fav.kind === 'discussion') return showDiscussions;
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
    if (isDiscussion) return false;
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
            definitions={discussionDefs}
            error={discussionDefsError}
            onSelect={closeMenu}
            isFavorite={isDiscussionFavorite}
            onToggleFavorite={toggleDiscussionFavorite}
          />
        {/snippet}
      </MenuSubmenuItem>
    {/if}
  </Menu>
</Popover>
