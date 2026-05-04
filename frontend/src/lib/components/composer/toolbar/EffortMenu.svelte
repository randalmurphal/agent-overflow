<script lang="ts">
  // Combined effort, context-window, and fast-mode menu. Auto-compact
  // thresholds live in Settings and are opened from the composer context meter.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import type { ContextWindowOption, ModelInfo, ReasoningEffortOption } from '../../../types/settings';
  import {
    GetModelsForProvider,
    UpdateThreadFastMode,
    UpdateThreadContextWindow,
    UpdateThreadReasoningEffort,
  } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
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

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let contextOptions = $state<ContextWindowOption[]>([]);
  let reasoningOptions = $state<ReasoningEffortOption[]>([]);
  let fastModeSupported = $state(false);
  let loadedContextKey = '';

  type Effort = 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';

  const FALLBACK_EFFORTS: Array<ReasoningEffortOption> = [
    { slug: 'low', label: 'Low' },
    { slug: 'medium', label: 'Medium' },
    { slug: 'high', label: 'High' },
    { slug: 'xhigh', label: 'Extra High' },
  ];

  let currentEffort = $derived<Effort>(
    (pane.thread?.reasoningEffort as Effort | undefined) ?? 'medium',
  );
  let currentFast = $derived(pane.thread?.fastMode === true);
  let currentContextWindow = $derived(pane.thread?.contextWindow ?? 0);

  let availableEfforts = $derived(reasoningOptions.length > 0 ? reasoningOptions : FALLBACK_EFFORTS);

  function titleCase(slug: Effort): string {
    if (slug === 'xhigh') return 'Extra High';
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

  let triggerLabel = $derived(
    currentContextWindow > 0
      ? `${titleCase(currentEffort)} · ${contextLabel(currentContextWindow)}`
      : titleCase(currentEffort),
  );

  async function ensureContextOptions(): Promise<void> {
    const p = pane.thread?.provider;
    const model = pane.thread?.model;
    if ((p !== 'claude' && p !== 'codex') || !model) {
      contextOptions = [];
      reasoningOptions = [];
      fastModeSupported = false;
      loadedContextKey = '';
      return;
    }
    const key = `${p}:${model}`;
    if (loadedContextKey === key) return;
    try {
      const models = (await GetModelsForProvider(p)) as ModelInfo[] | null;
      const current = (Array.isArray(models) ? models : []).find((candidate) => candidate.slug === model);
      contextOptions = current?.contextWindows ?? [];
      reasoningOptions = current?.reasoningEfforts ?? [];
      fastModeSupported = current?.capabilities?.includes('fast_mode') ?? false;
      loadedContextKey = key;
    } catch (err) {
      console.error('GetModelsForProvider failed:', err);
      addToast('error', `Failed to load context windows: ${errString(err)}`);
      contextOptions = [];
      reasoningOptions = [];
      fastModeSupported = false;
      loadedContextKey = key;
    }
  }

  function handleTrigger(): void {
    open = !open;
    if (!open) return;
    void ensureContextOptions();
  }

  function closeMenu(): void {
    open = false;
    triggerEl?.focus();
  }

  async function handleEffort(next: Effort): Promise<void> {
    if (!pane.thread || next === currentEffort) {
      closeMenu();
      return;
    }
    try {
      const updated = (await UpdateThreadReasoningEffort(pane.thread.id, next)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
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
      const updated = (await UpdateThreadFastMode(pane.thread.id, on)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
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
      const updated = (await UpdateThreadContextWindow(pane.thread.id, tokens)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
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
  data-testid="composer-effort-trigger"
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={Gauge} size={13} strokeWidth={1.75} class="opacity-80" />
  <span class="@max-[519px]:hidden">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Effort, context, and fast mode" onClose={closeMenu}>
    <MenuSectionHeader label="Effort" />
    {#each availableEfforts as tier (tier.slug)}
      <MenuItem
        label={tier.label}
        checked={tier.slug === currentEffort}
        onSelect={() => handleEffort(tier.slug)}
      />
    {/each}

    {#if contextOptions.length > 1}
      <MenuDivider />
      <MenuSectionHeader label="Context" />
      {#each contextOptions as option (option.tokens)}
        <MenuItem
          label={contextOptionLabel(option)}
          checked={option.tokens === currentContextWindow}
          onSelect={() => handleContextWindow(option.tokens)}
        />
      {/each}
    {/if}

    {#if fastModeSupported}
      <MenuDivider />
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
    {/if}

  </Menu>
</Popover>
