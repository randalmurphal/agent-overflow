<script lang="ts">
  // Runtime-mode cycle button: approval-required → auto-accept-edits →
  // full-access. Replaces the old RuntimeModePicker dropdown with a
  // three-tier single-click cycle to match the screenshot spec.
  //
  // The icon evolves from a closed padlock (safest) through a half-open
  // padlock to an unlocked padlock (most friction-free). The tier
  // labels and long-form descriptions are taken verbatim from the old
  // picker so users coming from that UI see the same wording.

  import Lock from 'lucide-svelte/icons/lock';
  import LockOpen from 'lucide-svelte/icons/lock-open';
  import PenLine from 'lucide-svelte/icons/pen-line';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { RuntimeMode, Thread } from '../../../types/models';
  import { UpdateThreadRuntimeMode } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import Icon from '../../primitives/Icon.svelte';

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type IconComponent = any;

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let applying = $state(false);

  interface TierMeta {
    mode: RuntimeMode;
    label: string;
    description: string;
  }

  // Order = cycle order. Safest first, friction-free last.
  const TIERS: readonly TierMeta[] = [
    {
      mode: 'approval-required',
      label: 'Approval',
      description:
        'Approval required — the agent asks before writing files or running commands.',
    },
    {
      mode: 'auto-accept-edits',
      label: 'Auto-edits',
      description:
        'Auto-accept edits — file edits in the workspace run without prompts; commands still ask.',
    },
    {
      mode: 'full-access',
      label: 'Full access',
      description: 'Full access — no prompts. The agent runs every tool unattended.',
    },
  ];

  // Pane thread may surface without a runtimeMode (pre-v12 fixture); we
  // default to full-access because that's what the backend treats as the
  // "no prompts" baseline.
  let current = $derived<RuntimeMode>(
    (pane.thread?.runtimeMode as RuntimeMode | undefined) ?? 'full-access',
  );
  let currentMeta = $derived(TIERS.find((t) => t.mode === current) ?? TIERS[2]);

  function nextTier(mode: RuntimeMode): RuntimeMode {
    const idx = TIERS.findIndex((t) => t.mode === mode);
    const nextIdx = idx < 0 ? 0 : (idx + 1) % TIERS.length;
    return TIERS[nextIdx].mode;
  }

  async function handleClick(): Promise<void> {
    if (applying || !pane.thread) return;
    const target = nextTier(current);
    applying = true;
    try {
      const updated = (await UpdateThreadRuntimeMode(pane.thread.id, target)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
    } catch (err) {
      console.error('access.toggle: UpdateThreadRuntimeMode failed', err);
      addToast('error', `Failed to change access mode: ${errString(err)}`);
    } finally {
      applying = false;
    }
  }

  // Lucide icons by tier: closed lock → pen-line (edits allowed) →
  // open lock. The graphics are distinct enough at 13px that users can
  // read state without the label, but the label is kept because
  // "Approval" vs "Auto-edits" is hard to mnemonize from icons alone.
  let icon = $derived.by<IconComponent>(() => {
    switch (current) {
      case 'approval-required': return Lock;
      case 'auto-accept-edits': return PenLine;
      case 'full-access':       return LockOpen;
    }
  });
</script>

<button
  type="button"
  onclick={handleClick}
  disabled={applying || !pane.thread}
  data-testid="composer-access-toggle"
  data-mode={current}
  aria-label="Change runtime access mode"
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
  <Icon {icon} size={13} strokeWidth={1.75} class="opacity-80" />
  <span>{currentMeta.label}</span>
</button>
