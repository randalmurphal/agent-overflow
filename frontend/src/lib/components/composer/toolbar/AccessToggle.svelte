<script lang="ts">
  // Runtime/access mode selector. The selected value is a composer draft until
  // send; dispatchSend applies it to the thread immediately before starting the
  // provider turn.

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Lock from 'lucide-svelte/icons/lock';
  import LockOpen from 'lucide-svelte/icons/lock-open';
  import PenLine from 'lucide-svelte/icons/pen-line';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { RuntimeMode } from '../../../types/models';
  import {
    hasRuntimeModeDraft,
    runtimeModeForThread,
    setRuntimeModeDraft,
  } from '../../../stores/runtimeModeDraft.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type IconComponent = any;

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);

  interface TierMeta {
    mode: RuntimeMode;
    label: string;
    description: string;
    icon: IconComponent;
  }

  const TIERS: readonly TierMeta[] = [
    {
      mode: 'approval-required',
      label: 'Supervised',
      description: 'Ask before commands and file changes.',
      icon: Lock,
    },
    {
      mode: 'auto-accept-edits',
      label: 'Auto-accept edits',
      description: 'Auto-approve edits, ask before other actions.',
      icon: PenLine,
    },
    {
      mode: 'full-access',
      label: 'Full access',
      description: 'Allow commands and edits without prompts.',
      icon: LockOpen,
    },
  ];

  let current = $derived<RuntimeMode>(runtimeModeForThread(pane.thread));
  let staged = $derived(hasRuntimeModeDraft(pane.thread));
  let currentMeta = $derived(TIERS.find((t) => t.mode === current) ?? TIERS[2]);

  function closeMenu(): void {
    open = false;
    // Composer-toolbar pickers sit just under the textarea; after the
    // menu closes the user is almost always going to keep typing. Send
    // focus back to the textarea so Enter / Esc / chord-toggle don't
    // strand them on a trigger button. `focusPaneComposer` is a no-op
    // if the textarea is gone (pane unmounted, thread cleared).
    if (!focusPaneComposer(pane.paneId)) triggerEl?.focus();
  }

  function handleTrigger(): void {
    open = !open;
  }

  function selectMode(mode: RuntimeMode): void {
    if (!pane.thread) {
      closeMenu();
      return;
    }
    if (mode === current) {
      closeMenu();
      return;
    }
    setRuntimeModeDraft(pane.thread.id, mode);
    closeMenu();
  }

  $effect(() => {
    return registerComposerPicker(pane.paneId, 'access', {
      isOpen: () => open,
      open: () => {
        if (!pane.thread) return;
        open = true;
      },
      close: closeMenu,
    });
  });
</script>

<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread}
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label={`Runtime Access Mode: ${currentMeta.label}`}
  data-testid="composer-access-toggle"
  data-mode={current}
  data-staged={staged}
  title={currentMeta.description}
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={currentMeta.icon} size={13} strokeWidth={1.75} class="opacity-80" />
  <span data-composer-toolbar-label="collapsible">{currentMeta.label}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>

<Popover
  anchor={triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Runtime Access Mode" onClose={closeMenu}>
    {#each TIERS as tier (tier.mode)}
      <MenuItem
        label={tier.label}
        description={tier.description}
        checked={tier.mode === current}
        onSelect={() => selectMode(tier.mode)}
      >
        {#snippet icon()}
          <Icon icon={tier.icon} size={14} strokeWidth={1.75} />
        {/snippet}
      </MenuItem>
    {/each}
  </Menu>
</Popover>
