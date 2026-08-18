<script lang="ts">
  // Runtime/access mode selector. Placeholder changes update new-thread
  // defaults; materialized threads persist the selected runtime mode directly.

  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Eye from '@lucide/svelte/icons/eye';
  import Lock from '@lucide/svelte/icons/lock';
  import LockOpen from '@lucide/svelte/icons/lock-open';
  import PenLine from '@lucide/svelte/icons/pen-line';
  import ShieldCheck from '@lucide/svelte/icons/shield-check';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { RuntimeMode, Thread } from '../../../types/models';
  import { asProviderID } from '../../../types/providers';
  import {
    ensureProviderModels,
    getProviderModels,
  } from '../../../stores/providerModels.svelte';
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
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import { chordHintSuffix } from '../../../stores/keybindings.svelte';

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

  // Ordered most- to least-restrictive on mutation, mirroring
  // provider.AllRuntimeModes (Go's TestAllRuntimeModesOrdering pins the same
  // sequence). Auto sits after auto-accept-edits because it lets strictly more
  // through unprompted: auto-accept-edits still stops at every shell command,
  // auto reviews it and usually allows it.
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
      mode: 'auto',
      label: 'Auto',
      // Both caveats are load-bearing, not hedging. Auto is the only tier
      // besides read-only that can REFUSE an action, and it is the only tier
      // that spends money to decide — each reviewed tool call is a billed
      // model call (Claude a Haiku classifier turn, Codex an auto_review
      // subagent). Neither is discoverable from the label, and a user who
      // reads "Auto" as "no friction" will be surprised twice.
      description: 'A model reviews each action instead of you. Costs extra tokens, and it can refuse.',
      icon: ShieldCheck,
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
  let activeProvider = $derived(asProviderID(pane.thread?.provider));
  let activeModel = $derived(pane.thread?.model ?? '');
  // Auto is withheld ONLY on the model's explicit `supportsAutoMode: false`
  // (the CLI's own per-model answer, carried three-state end to end — see
  // internal/claudemodels/AGENTS.md). Unknown — the wire didn't say, the
  // catalog isn't loaded yet, or the model isn't listed — behaves exactly
  // like supported: mis-disabling a working mode is the worse failure.
  let autoModeUnsupported = $derived.by(() => {
    if (!activeProvider || !activeModel) return false;
    const info = getProviderModels(activeProvider).find(
      (candidate) => candidate.slug === activeModel,
    );
    return info?.supportsAutoMode === false;
  });
  let pickerChordSuffix = $derived(chordHintSuffix('composer.picker.access'));

  function runtimeModeForThread(thread: Thread | null | undefined): RuntimeMode {
    return (thread?.runtimeMode as RuntimeMode | undefined) ?? DEFAULT_MODE;
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { paneId: pane.paneId, triggerEl });
  }

  // Loads the provider catalog so autoModeUnsupported has an answer while
  // the menu is up. A load failure only leaves Auto selectable — which is
  // the contract's default posture anyway — so it logs rather than toasts.
  function warmModelCapabilities(): void {
    const provider = activeProvider;
    if (!provider || !activeModel) return;
    ensureProviderModels(provider).catch((err: unknown) => {
      console.error('GetModelsForProvider failed:', err);
    });
  }

  function handleTrigger(): void {
    open = !open;
    if (open) warmModelCapabilities();
  }

  async function selectMode(mode: RuntimeMode): Promise<void> {
    if (!pane.thread) {
      closeMenu();
      return;
    }
    // Belt to the MenuItem `disabled` suspender: nothing else calls
    // selectMode today, but a future entry point must not be able to
    // persist a mode the model explicitly refused.
    if (mode === 'auto' && autoModeUnsupported) {
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
        warmModelCapabilities();
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
  title={`${currentMeta.description}${pickerChordSuffix}`}
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
      {@const unavailable = tier.mode === 'auto' && autoModeUnsupported}
      <MenuItem
        label={tier.label}
        description={unavailable ? 'Not supported by the current model.' : tier.description}
        checked={tier.mode === current}
        disabled={unavailable}
        onSelect={() => void selectMode(tier.mode)}
      >
        {#snippet icon()}
          <Icon icon={tier.icon} size={14} strokeWidth={1.75} />
        {/snippet}
      </MenuItem>
    {/each}
  </Menu>
</Popover>
