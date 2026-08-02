<script lang="ts">
  // Runtime/access mode selector. Placeholder changes update new-thread
  // defaults; materialized threads persist the selected runtime mode directly.

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Eye from 'lucide-svelte/icons/eye';
  import Lock from 'lucide-svelte/icons/lock';
  import LockOpen from 'lucide-svelte/icons/lock-open';
  import PenLine from 'lucide-svelte/icons/pen-line';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { RuntimeMode, Thread } from '../../../types/models';
  import { UpdateThreadRuntimeMode } from '../../../stores/bindings';
  import { updatePlaceholderDefaults } from '../../../stores/newThreadDefaults';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import Icon from '../../primitives/Icon.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { focusPaneComposer } from '../../panes/paneComposerFocus';
  import { formatChord, keybindingForCommand } from '../../../stores/keybindings.svelte';

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

  const DEFAULT_MODE: RuntimeMode = 'full-access';

  const TIERS: readonly TierMeta[] = [
    {
      mode: 'read-only',
      label: 'Read-only',
      description: 'Deny edits and mutating commands instead of asking.',
      icon: Eye,
    },
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
      mode: DEFAULT_MODE,
      label: 'Full access',
      description: 'Allow commands and edits without prompts.',
      icon: LockOpen,
    },
  ];

  // Resolved by value, not by index: the tier list is ordered most- to
  // least-restrictive, so a positional fallback silently changes meaning
  // whenever a tier is inserted.
  const DEFAULT_TIER = TIERS.find((t) => t.mode === DEFAULT_MODE) as TierMeta;

  let current = $derived<RuntimeMode>(runtimeModeForThread(pane.thread));
  let currentMeta = $derived(TIERS.find((t) => t.mode === current) ?? DEFAULT_TIER);
  let pickerChord = $derived(
    formatChord(keybindingForCommand('composer.picker.access') ?? 'mod+shift+a'),
  );

  function runtimeModeForThread(thread: Thread | null | undefined): RuntimeMode {
    return (thread?.runtimeMode as RuntimeMode | undefined) ?? DEFAULT_MODE;
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

  function handleTrigger(): void {
    open = !open;
  }

  async function selectMode(mode: RuntimeMode): Promise<void> {
    if (!pane.thread) {
      closeMenu();
      return;
    }
    if (mode === current) {
      closeMenu();
      return;
    }
    try {
      if (pane.hasDraftPlaceholder) {
        await updatePlaceholderDefaults(pane, { runtimeMode: mode });
      } else {
        const threadId = pane.threadId;
        if (!threadId) return;
        const updated = (await UpdateThreadRuntimeMode(threadId, mode)) as Thread;
        syncThread(updated);
      }
    } catch (err) {
      addToast('error', `Failed to set access mode: ${errString(err)}`);
    } finally {
      closeMenu();
    }
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
  title={`${currentMeta.description} (${pickerChord})`}
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[0.6875rem] text-fg-muted',
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
        onSelect={() => void selectMode(tier.mode)}
      >
        {#snippet icon()}
          <Icon icon={tier.icon} size={14} strokeWidth={1.75} />
        {/snippet}
      </MenuItem>
    {/each}
  </Menu>
</Popover>
