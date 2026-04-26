<script lang="ts">
  // Combined "effort · fast-mode · context-window" menu. Three sections
  // under one trigger because all three knobs conceptually control the
  // same thing: how hard the provider works on each turn.
  //
  // Context Window is disabled on Codex because Codex threads use the
  // per-model default — there's no equivalent knob to expose. The
  // disabled row still surfaces so the user understands why the column
  // is frozen, rather than disappearing without explanation.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import {
    UpdateThreadContextWindow,
    UpdateThreadFastMode,
    UpdateThreadReasoningEffort,
  } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
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

  type Effort = 'low' | 'medium' | 'high' | 'xhigh' | 'max';
  type ContextTokens = 200000 | 1000000;

  const EFFORT_COMMON: Array<{ slug: Effort; label: string }> = [
    { slug: 'low', label: 'Low' },
    { slug: 'medium', label: 'Medium' },
    { slug: 'high', label: 'High' },
    { slug: 'xhigh', label: 'XHigh' },
  ];
  const EFFORT_CLAUDE_ONLY: { slug: Effort; label: string } = { slug: 'max', label: 'Max' };

  let provider = $derived(pane.thread?.provider ?? 'claude');
  let isCodex = $derived(provider === 'codex');

  let currentEffort = $derived<Effort>(
    (pane.thread?.reasoningEffort as Effort | undefined) ?? 'medium',
  );
  let currentFast = $derived(pane.thread?.fastMode === true);
  let currentContext = $derived<ContextTokens>(
    (pane.thread?.contextWindow as ContextTokens | undefined) ?? 1000000,
  );

  // Available effort tiers per provider. Max only surfaces on Claude
  // because the Codex provider doesn't understand it.
  let availableEfforts = $derived(
    isCodex ? EFFORT_COMMON : [...EFFORT_COMMON, EFFORT_CLAUDE_ONLY],
  );

  function titleCase(slug: Effort): string {
    if (slug === 'xhigh') return 'XHigh';
    return slug[0].toUpperCase() + slug.slice(1);
  }

  function formatContext(tokens: ContextTokens): string {
    return tokens === 200000 ? '200k' : '1M';
  }

  // Trigger label: "High · 1M ▾" on Claude, "High ▾" on Codex because
  // context window isn't meaningful there.
  let triggerLabel = $derived(
    isCodex
      ? titleCase(currentEffort)
      : `${titleCase(currentEffort)} · ${formatContext(currentContext)}`,
  );

  function handleTrigger(): void {
    open = !open;
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

  async function handleContext(tokens: ContextTokens): Promise<void> {
    if (!pane.thread || tokens === currentContext || isCodex) {
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
  <Menu ariaLabel="Effort, fast mode, and context window" onClose={closeMenu}>
    <MenuSectionHeader label="Effort" />
    {#each availableEfforts as tier (tier.slug)}
      <MenuItem
        label={tier.label}
        checked={tier.slug === currentEffort}
        onSelect={() => handleEffort(tier.slug)}
      />
    {/each}

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

    <MenuDivider />
    <MenuSectionHeader label="Context Window" />
    <MenuItem
      label="200k"
      checked={!isCodex && currentContext === 200000}
      disabled={isCodex}
      onSelect={() => handleContext(200000)}
    />
    <MenuItem
      label="1M"
      checked={!isCodex && currentContext === 1000000}
      disabled={isCodex}
      onSelect={() => handleContext(1000000)}
    />
    {#if isCodex}
      <div
        class="px-3 py-1 text-[10px] text-text-secondary/60 leading-tight"
        role="presentation"
        data-testid="effort-menu-codex-note"
      >
        Codex uses per-model defaults
      </div>
    {/if}
  </Menu>
</Popover>
