<script lang="ts">
  // Runtime-mode cycle button: approval-required → auto-accept-edits →
  // full-access. Replaces the old RuntimeModePicker dropdown with a
  // three-tier single-click cycle to match the screenshot spec.
  //
  // The icon evolves from a closed padlock (safest) through a half-open
  // padlock to an unlocked padlock (most friction-free). The tier
  // labels and long-form descriptions are taken verbatim from the old
  // picker so users coming from that UI see the same wording.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { RuntimeMode, Thread } from '../../../types/models';
  import { UpdateThreadRuntimeMode } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';

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
      addToast('error', `Failed to change access mode: ${err}`);
    } finally {
      applying = false;
    }
  }

  // Icon per tier. Drawn inline so the border / shackle transforms
  // consistently as the user cycles. All three share the lock body; only
  // the shackle changes so the control feels like one growing icon
  // rather than three unrelated glyphs.
  let iconPath = $derived.by(() => {
    switch (current) {
      case 'approval-required':
        // Closed shackle
        return 'M7 11V7a5 5 0 0 1 10 0v4';
      case 'auto-accept-edits':
        // Half-open shackle (tilted)
        return 'M7 11V7a5 5 0 0 1 9-3';
      case 'full-access':
        // Fully open shackle (offset right)
        return 'M17 11V7a5 5 0 0 0-10 0';
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
    'inline-flex items-center gap-1.5 rounded-md border border-border',
    'px-2 py-1 text-xs text-text-secondary',
    'transition-colors cursor-pointer',
    'hover:border-text-secondary hover:text-text-primary',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <svg
    viewBox="0 0 24 24"
    class="h-3.5 w-3.5"
    fill="none"
    stroke="currentColor"
    stroke-width="1.75"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <rect x="5" y="11" width="14" height="10" rx="2" />
    <path d={iconPath} />
  </svg>
  <span>{currentMeta.label}</span>
</button>
