<script lang="ts">
  // Combined effort, context-window, and fast-mode menu. Auto-compact
  // thresholds live in Settings and are opened from the composer context meter.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { ContextWindowOption, ModelInfo, ReasoningEffortOption } from '../../../types/settings';
  import {
    applyThreadContextWindow,
    applyThreadFastMode,
    applyThreadReasoningEffort,
  } from '../../../stores/threadModelControls';
  import { asProviderID } from '../../../types/providers';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../../stores/providerModels.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import { formatTokens } from '../../../utils/format';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Gauge from '@lucide/svelte/icons/gauge';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';
  import { chordHintSuffix } from '../../../stores/keybindings.svelte';
  import { getFastModeReport } from '../../../stores/fastModeState.svelte';
  import { fastModeContradictionText, isFastModeContradicted } from '../../../utils/fastMode';

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
        // Same gate as the trigger's disabled state: the chord must not open a
        // menu with no rows in it either.
        if (!pane.thread || !hasMenuOptions) return;
        open = true;
        void ensureModelMetadata();
      },
      close: closeMenu,
    });
  });

  type Effort = 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max' | 'ultra';

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
  // Effort/context/fast-mode settings belong to the durable requested model.
  // A classifier fallback changes only the live model display; applying Opus
  // capabilities here would mutate the saved Fable profile with the wrong menu.
  let activeModel = $derived(pane.thread?.model ?? '');
  let activeModelInfo = $derived<ModelInfo | undefined>(
    activeProvider
      ? getProviderModels(activeProvider).find((candidate) => candidate.slug === activeModel)
      : undefined,
  );
  let contextOptions = $derived<ContextWindowOption[]>(activeModelInfo?.contextWindows ?? []);
  let reasoningOptions = $derived<ReasoningEffortOption[]>(activeModelInfo?.reasoningEfforts ?? []);
  let fastModeSupported = $derived(activeModelInfo?.capabilities?.includes('fast_mode') ?? false);
  // The tier the provider declared for this model names the toggle. It is
  // absent for Claude (no tier concept) and for a catalog cached before the
  // field existed, so every read falls back to today's literals rather than
  // rendering a blank. Support is still `capabilities`, never this.
  let fastModeTier = $derived(activeModelInfo?.fastModeTier);
  let fastModeLabel = $derived(fastModeTier?.name?.trim() || 'Fast');
  // "Fast Mode" is the section's own wording, not the tier's; only a tier that
  // calls itself something else displaces it.
  let fastModeSectionLabel = $derived(
    fastModeLabel === 'Fast' ? 'Fast Mode' : `${fastModeLabel} Mode`,
  );
  let fastModeDescription = $derived(fastModeTier?.description?.trim() || undefined);
  // What the PROVIDER says fast mode is doing right now, as opposed to
  // what the thread asked for. Absent until a session reports it (older
  // CLI, no turn finished yet, provider with no fast-mode concept), and
  // absence is treated as unknown — never as a denial.
  let fastModeReport = $derived(getFastModeReport(pane.threadId));
  let fastModeContradicted = $derived(isFastModeContradicted(currentFast, fastModeReport));
  let fastModeContradiction = $derived(
    fastModeContradicted ? fastModeContradictionText(fastModeReport) : '',
  );
  // The "On" row must not claim a benefit the provider says it is not
  // delivering: the tier blurb is displaced by the reason it is off.
  let fastModeOnTitle = $derived(fastModeContradiction || fastModeDescription);

  // "We have no catalog entry for this model" and "the catalog says this model
  // has no effort tiers" are different answers and must not share a branch.
  // The first is ignorance — the generic list is the best guess until the
  // catalog loads. The second is knowledge: Claude's own model list reports
  // Haiku with no effort support, and offering tiers the model does not have
  // is the lie this distinction removes.
  let availableEfforts = $derived(activeModelInfo ? reasoningOptions : FALLBACK_EFFORTS);
  let showEffortSelection = $derived(availableEfforts.length > 0);
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
  // A model with nothing to choose (one context tier, no fast mode, no effort
  // tiers) gets a disabled trigger rather than a menu with no rows in it.
  let hasMenuOptions = $derived(showContextSelection || fastModeSupported || showEffortSelection);
  let triggerLabel = $derived.by(() => {
    const labelParts = [];
    if (showEffortSelection) {
      labelParts.push(effortLabel(currentEffort, currentEffortOption?.label));
    }
    if (currentFast) labelParts.push(fastModeContradicted ? `${fastModeLabel} (off)` : fastModeLabel);
    // The context window is the label's fallback subject: with no effort tiers
    // to name, it is the only thing left that describes the thread.
    if (currentContextWindow > 0 && (showContextSelection || labelParts.length === 0)) {
      labelParts.push(
        currentContextOption
          ? contextOptionLabel(currentContextOption)
          : contextLabel(currentContextWindow),
      );
    }
    return labelParts.join(' · ');
  });
  let triggerTitle = $derived(triggerLabel || 'Model options');
  // Append the provider's contradiction to the trigger tooltip so the
  // explanation is reachable without opening the menu — the toolbar label
  // can only afford "(off)".
  let triggerHoverText = $derived(
    fastModeContradiction ? `Effort: ${triggerTitle} — ${fastModeContradiction}` : `Effort: ${triggerTitle}`,
  );
  let pickerChordSuffix = $derived(chordHintSuffix('composer.picker.effort'));

  // Eagerly load the model catalog as soon as the active thread's
  // provider/model is known, so the trigger label shows the context window
  // (200k / 1M) on first render. Without this the catalog loads only when a
  // picker opens, leaving the context segment of the label hidden until the
  // user opens this menu or the model picker. ensureProviderModels is cached +
  // single-flight, so this is a no-op once loaded.
  $effect(() => {
    if (!activeProvider || !activeModel) return;
    // Best-effort prefetch: a failure here is non-actionable, so swallow it
    // rather than logging. The real, actionable failure surfaces on the
    // user-initiated open path (ensureModelMetadata re-runs the load and
    // raises a toast). Logging here would be production noise on every
    // transient blip — and, because Composer mounts this menu, it fired a
    // console.error on every Composer render in tests that don't mock the
    // model-catalog binding.
    ensureProviderModels(activeProvider).catch(() => {});
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

  // Each handler drives the shared apply path in threadModelControls, which
  // owns the placeholder-vs-thread branch and the no-op short circuit. The
  // menu's only remaining job is where the failure goes — a toast here, a
  // composer-local error when the same path runs from `/effort` or `/fast`.
  async function handleEffort(next: Effort): Promise<void> {
    const result = await applyThreadReasoningEffort(pane, next);
    if (!result.ok && result.error) addToast('error', result.error);
    closeMenu();
  }

  async function handleFastMode(on: boolean): Promise<void> {
    const result = await applyThreadFastMode(pane, on);
    if (!result.ok && result.error) addToast('error', result.error);
    closeMenu();
  }

  async function handleContextWindow(tokens: number): Promise<void> {
    const result = await applyThreadContextWindow(pane, tokens);
    if (!result.ok && result.error) addToast('error', result.error);
    closeMenu();
  }
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || !hasMenuOptions}
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label={`Effort: ${triggerTitle}`}
  title={`${triggerHoverText}${pickerChordSuffix}`}
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
        <MenuSectionHeader label={fastModeSectionLabel} />
        <MenuItem
          label="Off"
          checked={!currentFast}
          onSelect={() => handleFastMode(false)}
        />
        <MenuItem
          label="On"
          checked={currentFast}
          suffix={fastModeContradicted ? 'provider: off' : undefined}
          title={fastModeOnTitle}
          onSelect={() => handleFastMode(true)}
        />
        <MenuDivider />
      {/if}

      {#if showEffortSelection}
        <MenuSectionHeader label="Effort" />
        {#each availableEfforts as tier (tier.slug)}
          <MenuItem
            label={effortLabel(tier.slug, tier.label)}
            checked={tier.slug === currentEffort}
            onSelect={() => handleEffort(tier.slug)}
          />
        {/each}
      {/if}
    </Menu>
  </div>
</Popover>
