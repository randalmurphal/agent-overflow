<script lang="ts">
  // Composer toolbar control for chat-thread agent mode. Toggles between
  // chat and plan via the same UpdateThreadMode binding the global
  // `mode.cycle` command (Shift+Tab) calls — both go through cycleMode
  // so the keyboard shortcut and the button stay in lockstep.
  //
  // This component renders ONLY on threads where the type is mutable
  // (chat). Design and discussion threads have immutable types: their
  // composer carries DesignLockPill / no toggle instead. The owner
  // (ComposerToolbar) decides which to render based on pane.thread.mode.

  import Bot from 'lucide-svelte/icons/bot';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { UpdateThreadMode } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { cycleMode, type CycleMode } from '../../../utils/modeCycle';
  import { errString } from '../../../utils/errors';
  import Icon from '../../primitives/Icon.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let applying = $state(false);

  // Normalize the thread's current mode for display. A thread with no mode
  // value (back-compat with older rows) is presented as chat.
  let currentMode = $derived<CycleMode>(
    (pane.thread?.mode as CycleMode | undefined) ?? 'chat',
  );

  const MODE_LABELS: Record<CycleMode, string> = {
    chat: 'Chat',
    plan: 'Plan',
  };

  let modeLabel = $derived(MODE_LABELS[currentMode] ?? MODE_LABELS.chat);

  async function handleClick(): Promise<void> {
    if (applying || !pane.thread) return;
    const next = cycleMode(currentMode);
    applying = true;
    try {
      const updated = (await UpdateThreadMode(pane.thread.id, next)) as Thread;
      syncThread(updated);
    } catch (err) {
      console.error('agent mode toggle: UpdateThreadMode failed', err);
      addToast('error', `Failed to switch mode: ${errString(err)}`);
    } finally {
      applying = false;
    }
  }
</script>

<button
  type="button"
  onclick={handleClick}
  disabled={applying || !pane.thread}
  data-testid="composer-agent-mode-toggle"
  aria-label={`Agent mode: ${modeLabel}. Toggle with Shift+Tab`}
  title={`Agent mode: ${modeLabel} — Shift+Tab to toggle`}
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={Bot} size={13} strokeWidth={1.75} class="opacity-80" />
  <span data-composer-toolbar-label="collapsible">{modeLabel}</span>
</button>
