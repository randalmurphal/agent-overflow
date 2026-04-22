<script lang="ts">
  // Primary toolbar menu: picks provider + model, or opens the
  // Discussions sub-flow. Composed from the Menu / Popover primitives;
  // the submenu layout is hand-written (not driven by a tree-of-items
  // prop) because Codex and Claude have different capability
  // conventions and Discussions is its own flow.
  //
  // Model list caching: `GetModelsForProvider` is cheap but not free,
  // and the menu is expected to reopen often during a debugging
  // session. We cache by provider at module scope so re-opening the
  // menu doesn't retrigger a round-trip. The cache is cleared on
  // `provider:status` Wails events because a newly-authenticated
  // provider may report a different model set.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import type { ModelInfo } from '../../../types/settings';
  import {
    GetModelsForProvider,
    UpdateThreadModel,
    UpdateThreadProvider,
  } from '../../../stores/bindings';
  import { wailsEventOn } from '../../../stores/events';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuSubmenuItem from '../../primitives/MenuSubmenuItem.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import ClaudeIcon from '../../primitives/brand/ClaudeIcon.svelte';
  import OpenAIIcon from '../../primitives/brand/OpenAIIcon.svelte';
  import ProviderModelsSubmenu from './ProviderModelsSubmenu.svelte';
  import DiscussionsSubmenu from './DiscussionsSubmenu.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  // Module-level cache shared across every ModelProviderMenu instance.
  // Clearing per-provider on provider:status avoids a global wipe when
  // only one backend flips state.
  const MODEL_CACHE: Map<string, ModelInfo[]> = new Map();
  let cacheVersion = $state(0); // bumped to re-run the derived submenu content

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let applying = $state(false);

  // Invalidate the cache entry when a provider's detection status changes
  // (the model list often changes with auth state).
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
      // Seed an empty array so we don't refetch on every hover; the
      // provider:status listener clears it when state improves.
      MODEL_CACHE.set(provider, []);
      cacheVersion += 1;
    }
  }

  function getModels(provider: 'claude' | 'codex'): ModelInfo[] {
    void cacheVersion;
    return MODEL_CACHE.get(provider) ?? [];
  }

  function handleTrigger(): void {
    open = !open;
    if (open) {
      // Warm the cache for the currently-active provider so the first
      // submenu hover doesn't flash an empty list.
      const p = pane.thread?.provider;
      if (p === 'claude' || p === 'codex') void ensureModels(p);
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
        // Provider first so the subsequent model update writes against
        // the right provider's validation surface.
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

  // Trigger label: brand glyph + active model name (no provider word —
  // the glyph already identifies the provider, and doubling up the
  // text made the button verbose). lucide doesn't ship these marks,
  // so we inline them as dedicated Svelte components under
  // primitives/brand/.
  let isCodex = $derived(pane.thread?.provider === 'codex');
  let modelLabel = $derived(pane.thread?.model ?? 'No model');

  // Once any item lands, the thread has picked a lane: a specific
  // provider (Claude or Codex), or — after StartDiscussion — a specific
  // discussion definition. Provider sessions and discussion runtimes
  // aren't interchangeable; both transitions are rejected server-side
  // (see UpdateThreadProvider and ensureDiscussionCanStart). Mirror
  // that here so the picker doesn't offer doomed options: on a locked
  // chat only the active provider's models are reachable, and on a
  // discussion nothing in this menu applies — its runtime is driven by
  // the child participant threads, each with their own provider.
  let isLocked = $derived(pane.items.length > 0);
  let isDiscussion = $derived(pane.thread?.mode === 'discussion');
  let activeProvider = $derived(pane.thread?.provider ?? null);
  let showCodexSubmenu = $derived(
    !isDiscussion && (!isLocked || activeProvider === 'codex'),
  );
  let showClaudeSubmenu = $derived(
    !isDiscussion && (!isLocked || activeProvider === 'claude'),
  );
  let showDiscussions = $derived(!isDiscussion && !isLocked);
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || applying}
  aria-haspopup="menu"
  aria-expanded={open}
  data-provider={pane.thread?.provider ?? ''}
  data-testid="composer-model-menu-trigger"
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  {#if isCodex}
    <!-- OpenAI/Codex picks up the trigger's muted foreground so the
         glyph + label read as one piece; t3-code uses the same pattern
         (see apps/web/src/components/chat/ProviderModelPicker.tsx
         providerIconClassName). -->
    <OpenAIIcon size={13} class="opacity-95" />
  {:else}
    <!-- Claude is painted in Anthropic's signature coral (#d97757),
         again matching t3-code's providerIconClassName. -->
    <ClaudeIcon size={13} class="text-[#d97757] opacity-95" />
  {/if}
  <span class="truncate max-w-[200px] text-fg">{modelLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Model and provider" onClose={closeMenu}>
    {#if showCodexSubmenu}
      <MenuSubmenuItem label="Codex">
        {#snippet children()}
          <ProviderModelsSubmenu
            {pane}
            provider="codex"
            {getModels}
            {ensureModels}
            onSelect={(slug) => handleSelectModel('codex', slug)}
          />
        {/snippet}
      </MenuSubmenuItem>
    {/if}

    {#if showClaudeSubmenu}
      <MenuSubmenuItem label="Claude">
        {#snippet children()}
          <ProviderModelsSubmenu
            {pane}
            provider="claude"
            {getModels}
            {ensureModels}
            onSelect={(slug) => handleSelectModel('claude', slug)}
          />
        {/snippet}
      </MenuSubmenuItem>
    {/if}

    {#if showDiscussions}
      <MenuDivider />

      <MenuSubmenuItem label="Discussions">
        {#snippet children()}
          <DiscussionsSubmenu {pane} onSelect={closeMenu} />
        {/snippet}
      </MenuSubmenuItem>
    {/if}
  </Menu>
</Popover>
