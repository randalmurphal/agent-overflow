<script lang="ts">
  // Combined effort, context-window, and fast-mode menu. Auto-compact
  // thresholds live in Settings and are opened from the composer context meter.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import type { ContextWindowOption, ModelInfo, ReasoningEffortOption } from '../../../types/settings';
  import {
    UpdateThreadFastMode,
    UpdateThreadContextWindow,
    UpdateThreadReasoningEffort,
  } from '../../../stores/bindings';
  import { asProviderID } from '../../../types/providers';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../../stores/providerModels.svelte';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { formatTokens } from '../../../utils/format';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Gauge from 'lucide-svelte/icons/gauge';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);

  // Publish an imperative handle so the global mod+shift+e chord can
  // toggle this picker. The registry keys by (paneId, pickerId) so
  // multi-pane mode routes the chord to whichever pane has focus.
  $effect(() => {
    return registerComposerPicker(pane.paneId, 'effort', {
      isOpen: () => open,
      open: () => {
        if (!pane.thread) return;
        open = true;
        void ensureModelMetadata();
      },
      close: closeMenu,
    });
  });

  type Effort = 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';

  const FALLBACK_EFFORTS: Array<ReasoningEffortOption> = [
    { slug: 'low', label: 'Low' },
    { slug: 'medium', label: 'Medium' },
    { slug: 'high', label: 'High' },
    { slug: 'xhigh', label: 'xHigh' },
  ];

  let currentEffort = $derived<Effort>(
    (pane.thread?.reasoningEffort as Effort | undefined) ?? 'medium',
  );
  let currentFast = $derived(pane.thread?.fastMode === true);
  let currentContextWindow = $derived(pane.thread?.contextWindow ?? 0);
  let activeProvider = $derived(asProviderID(pane.thread?.provider));
  let activeModel = $derived(pane.thread?.model ?? '');
  let activeModelInfo = $derived<ModelInfo | undefined>(
    activeProvider
      ? getProviderModels(activeProvider).find((candidate) => candidate.slug === activeModel)
      : undefined,
  );
  let contextOptions = $derived<ContextWindowOption[]>(activeModelInfo?.contextWindows ?? []);
  let reasoningOptions = $derived<ReasoningEffortOption[]>(activeModelInfo?.reasoningEfforts ?? []);
  let fastModeSupported = $derived(activeModelInfo?.capabilities?.includes('fast_mode') ?? false);

  let availableEfforts = $derived(reasoningOptions.length > 0 ? reasoningOptions : FALLBACK_EFFORTS);
  let currentEffortOption = $derived(
    availableEfforts.find((option) => option.slug === currentEffort),
  );

  function effortLabel(slug: Effort, label = ''): string {
    if (slug === 'xhigh') return 'xHigh';
    if (label.trim() !== '') return label;
    if (slug === 'none') return 'None';
    if (slug === 'minimal') return 'Minimal';
    return slug[0].toUpperCase() + slug.slice(1);
  }

  function contextLabel(tokens: number): string {
    if (tokens === 1_050_000 || tokens === 1_000_000) return '1M';
    if (tokens === 272_000) return '272k';
    if (tokens === 128_000) return '128k';
    if (tokens === 200_000) return '200k';
    return formatTokens(tokens);
  }

  function contextOptionLabel(option: ContextWindowOption): string {
    return option.label || contextLabel(option.tokens);
  }

  let showContextSelection = $derived(contextOptions.length > 1);
  let currentContextOption = $derived(
    contextOptions.find((option) => option.tokens === currentContextWindow),
  );
  let triggerLabel = $derived.by(() => {
    const labelParts = [effortLabel(currentEffort, currentEffortOption?.label)];
    if (currentFast) labelParts.push('Fast');
    if (showContextSelection && currentContextWindow > 0) {
      labelParts.push(
        currentContextOption
          ? contextOptionLabel(currentContextOption)
          : contextLabel(currentContextWindow),
      );
    }
    return labelParts.join(' · ');
  });

  async function ensureModelMetadata(): Promise<void> {
    const provider = activeProvider;
    if (!provider || !activeModel) return;
    try {
      await ensureProviderModels(provider);
    } catch (err) {
      console.error('GetModelsForProvider failed:', err);
      addToast('error', `Failed to load model capabilities: ${errString(err)}`);
    }
  }

  function handleTrigger(): void {
    open = !open;
    if (!open) return;
    void ensureModelMetadata();
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

  async function handleEffort(next: Effort): Promise<void> {
    if (!pane.thread || next === currentEffort) {
      closeMenu();
      return;
    }
    try {
      const threadId = pane.threadId ?? (await pane.ensureMaterializedThread());
      if (!threadId) return;
      const updated = (await UpdateThreadReasoningEffort(threadId, next)) as Thread;
      syncThread(updated);
    } catch (err) {
      console.error('UpdateThreadReasoningEffort failed:', err);
      addToast('error', `Failed to set effort: ${errString(err)}`);
    } finally {
      closeMenu();
    }
  }

  async function handleFastMode(on: boolean): Promise<void> {
    if (!pane.thread || on === currentFast) {
      closeMenu();
      return;
    }
    try {
      const threadId = pane.threadId ?? (await pane.ensureMaterializedThread());
      if (!threadId) return;
      const updated = (await UpdateThreadFastMode(threadId, on)) as Thread;
      syncThread(updated);
    } catch (err) {
      console.error('UpdateThreadFastMode failed:', err);
      addToast('error', `Failed to set fast mode: ${errString(err)}`);
    } finally {
      closeMenu();
    }
  }

  async function handleContextWindow(tokens: number): Promise<void> {
    if (!pane.thread || tokens === currentContextWindow) {
      closeMenu();
      return;
    }
    try {
      const threadId = pane.threadId ?? (await pane.ensureMaterializedThread());
      if (!threadId) return;
      const updated = (await UpdateThreadContextWindow(threadId, tokens)) as Thread;
      syncThread(updated);
    } catch (err) {
      console.error('UpdateThreadContextWindow failed:', err);
      addToast('error', `Failed to set context window: ${errString(err)}`);
    } finally {
      closeMenu();
    }
  }
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread}
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label={`Effort: ${triggerLabel}`}
  title={triggerLabel}
  data-testid="composer-effort-trigger"
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[0.6875rem] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={Gauge} size={13} strokeWidth={1.75} class="opacity-80" />
  <span data-composer-toolbar-label="collapsible">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <div
    class={[
      '[&_[data-menu]]:min-w-[150px] [&_[data-menu]]:py-0.5',
      '[&_[data-menuitem]]:px-2.5 [&_[data-menuitem]]:py-1 [&_[data-menuitem]]:text-xs [&_[data-menuitem]]:gap-1.5',
      '[&_[data-menu-section-header]]:px-2.5 [&_[data-menu-section-header]]:pt-1.5 [&_[data-menu-section-header]]:pb-0.5',
      '[&_[data-menu-divider]]:my-0.5',
    ].join(' ')}
  >
    <Menu ariaLabel="Effort, context, and fast mode" onClose={closeMenu}>
      {#if showContextSelection}
        <MenuSectionHeader label="Context" />
        {#each contextOptions as option (option.tokens)}
          <MenuItem
            label={contextOptionLabel(option)}
            checked={option.tokens === currentContextWindow}
            onSelect={() => handleContextWindow(option.tokens)}
          />
        {/each}
        <MenuDivider />
      {/if}

      {#if fastModeSupported}
        <MenuSectionHeader label="Fast Mode" />
        <MenuItem
          label="Off"
          checked={!currentFast}
          onSelect={() => handleFastMode(false)}
        />
        <MenuItem
          label="On"
          checked={currentFast}
          onSelect={() => handleFastMode(true)}
        />
        <MenuDivider />
      {/if}

      <MenuSectionHeader label="Effort" />
      {#each availableEfforts as tier (tier.slug)}
        <MenuItem
          label={effortLabel(tier.slug, tier.label)}
          checked={tier.slug === currentEffort}
          onSelect={() => handleEffort(tier.slug)}
        />
      {/each}
    </Menu>
  </div>
</Popover>
