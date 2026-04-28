<script lang="ts">
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import type { ModelInfo } from '../../../types/settings';
  import type { ChatBarFavorite } from '../../../stores/bindings';
  import {
    GetModelsForProvider,
    ListChatBarFavorites,
    SetChatBarFavorite,
    StartDiscussionByID,
    GetThread,
    UpdateThreadModel,
    UpdateThreadProvider,
  } from '../../../stores/bindings';
  import { wailsEventOn } from '../../../stores/events';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { displayModelLabel } from '../../../utils/modelLabels';
  import MessagesSquare from 'lucide-svelte/icons/messages-square';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuSubmenuItem from '../../primitives/MenuSubmenuItem.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import ClaudeIcon from '../../primitives/brand/ClaudeIcon.svelte';
  import OpenAIIcon from '../../primitives/brand/OpenAIIcon.svelte';
  import ChatBarFavoritesSection from './ChatBarFavoritesSection.svelte';
  import ModelProviderTrigger from './ModelProviderTrigger.svelte';
  import ProviderModelsSubmenu from './ProviderModelsSubmenu.svelte';
  import DiscussionsSubmenu from './DiscussionsSubmenu.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const MODEL_CACHE: Map<string, ModelInfo[]> = new Map();
  let cacheVersion = $state(0);

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let applying = $state(false);
  let favorites: ChatBarFavorite[] = $state([]);
  let favoritesLoaded = $state(false);

  $effect(() => {
    const cleanup = wailsEventOn<{ provider?: string }>('provider:status', (evt) => {
      const p = evt?.provider;
      if (!p) return;
      MODEL_CACHE.delete(p);
      cacheVersion += 1;
    });
    return cleanup;
  });

  async function ensureModels(provider: 'claude' | 'codex'): Promise<void> {
    if (MODEL_CACHE.has(provider)) return;
    try {
      const res = (await GetModelsForProvider(provider)) as ModelInfo[] | null;
      const list = Array.isArray(res) ? res : [];
      MODEL_CACHE.set(provider, list);
      cacheVersion += 1;
    } catch (err) {
      console.error('GetModelsForProvider failed:', err);
      addToast('error', `Failed to load ${provider} models`);
      MODEL_CACHE.set(provider, []);
      cacheVersion += 1;
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

  function getModels(provider: 'claude' | 'codex'): ModelInfo[] {
    void cacheVersion;
    return MODEL_CACHE.get(provider) ?? [];
  }

  function handleTrigger(): void {
    open = !open;
    if (open) {
      const p = pane.thread?.provider;
      if (p === 'claude' || p === 'codex') void ensureModels(p);
      void ensureFavorites();
    }
  }

  function closeMenu(): void {
    open = false;
    triggerEl?.focus();
  }

  async function handleSelectModel(
    provider: 'claude' | 'codex',
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
        pane.replaceThread(afterProvider);
        replaceThread(afterProvider);
      }
      const updated = (await UpdateThreadModel(threadId, slug)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
    } catch (err) {
      console.error('model/provider update failed:', err);
      addToast('error', `Failed to switch model: ${errString(err)}`);
    } finally {
      applying = false;
      closeMenu();
    }
  }

  function isModelFavorite(provider: 'claude' | 'codex', slug: string): boolean {
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

  function toggleModelFavorite(provider: 'claude' | 'codex', model: ModelInfo): void {
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
        pane.replaceThread(refreshed);
        replaceThread(refreshed);
      } catch (refreshErr) {
        console.error('Failed to refresh thread after StartDiscussionByID:', refreshErr);
      }
      addToast('info', `Started discussion "${fav.label}"`);
    } catch (err) {
      console.error('StartDiscussionByID failed:', err);
      addToast('error', `Failed to start discussion: ${errString(err)}`);
    }
  }

  let isCodex = $derived(pane.thread?.provider === 'codex');
  let modelLabel = $derived(displayModelLabel(pane.thread?.provider ?? '', pane.thread?.model ?? 'No model'));

  let isLocked = $derived(pane.items.length > 0);
  let isDiscussion = $derived(pane.thread?.mode === 'discussion');
  let activeProvider = $derived(pane.thread?.provider ?? null);
  let showCodexSubmenu = $derived(!isDiscussion && (!isLocked || activeProvider === 'codex'));
  let showClaudeSubmenu = $derived(!isDiscussion && (!isLocked || activeProvider === 'claude'));
  let showDiscussions = $derived(!isDiscussion && !isLocked);
  let visibleFavorites = $derived(favorites.filter((fav) => {
    if (fav.kind === 'discussion') return showDiscussions;
    if (fav.provider !== 'claude' && fav.provider !== 'codex') return false;
    return !isDiscussion && (!isLocked || activeProvider === fav.provider);
  }));
</script>

<ModelProviderTrigger
  bind:buttonEl={triggerEl}
  {open}
  disabled={!pane.thread || applying}
  {isCodex}
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

    {#if showCodexSubmenu}
      <MenuSubmenuItem label="Codex">
        {#snippet icon()}
          <OpenAIIcon size={13} class="opacity-95" />
        {/snippet}
        {#snippet children()}
          <ProviderModelsSubmenu
            {pane}
            provider="codex"
            {getModels}
            {ensureModels}
            onSelect={(slug) => handleSelectModel('codex', slug)}
            isFavorite={isModelFavorite}
            onToggleFavorite={(model) => toggleModelFavorite('codex', model)}
          />
        {/snippet}
      </MenuSubmenuItem>
    {/if}

    {#if showClaudeSubmenu}
      <MenuSubmenuItem label="Claude">
        {#snippet icon()}
          <ClaudeIcon size={13} class="text-[#d97757] opacity-95" />
        {/snippet}
        {#snippet children()}
          <ProviderModelsSubmenu
            {pane}
            provider="claude"
            {getModels}
            {ensureModels}
            onSelect={(slug) => handleSelectModel('claude', slug)}
            isFavorite={isModelFavorite}
            onToggleFavorite={(model) => toggleModelFavorite('claude', model)}
          />
        {/snippet}
      </MenuSubmenuItem>
    {/if}

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
