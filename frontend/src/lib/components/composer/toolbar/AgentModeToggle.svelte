<script lang="ts">
  // Composer toolbar control for chat-thread agent mode. Toggles between
  // chat and plan via the same UpdateThreadMode binding the global
  // `mode.cycle` command (Shift+Tab) calls — both go through cycleMode
  // so the keyboard shortcut and the button stay in lockstep.
  //
  // This component renders ONLY on chat threads. Design and discussion
  // threads have immutable types and their composer omits this slot
  // entirely; the in-pane ThreadModePicker in the workspace strip is
  // where the mode is surfaced. ComposerToolbar gates the render.

  import Bot from 'lucide-svelte/icons/bot';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { UpdateThreadMode } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { cycleMode, type CycleMode } from '../../../utils/modeCycle';
  import { errString } from '../../../utils/errors';
  import Icon from '../../primitives/Icon.svelte';
  import { formatChord, keybindingForCommand } from '../../../stores/keybindings.svelte';

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
    chat: 'Build',
    plan: 'Plan',
  };

  let modeLabel = $derived(MODE_LABELS[currentMode] ?? MODE_LABELS.chat);
  let cycleChord = $derived(
    formatChord(keybindingForCommand('mode.cycle') ?? 'shift+tab'),
  );

  async function handleClick(): Promise<void> {
    if (applying || !pane.thread) return;
    const next = cycleMode(currentMode);
    if (pane.hasDraftPlaceholder) {
      pane.setDraftPlaceholderMode(next);
      return;
    }
    applying = true;
    try {
      const threadId = pane.threadId;
      if (!threadId) return;
      const updated = (await UpdateThreadMode(threadId, next)) as Thread;
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
  aria-label={`Agent mode: ${modeLabel}. Toggle with ${cycleChord}`}
  title={`Agent mode: ${modeLabel} (${cycleChord})`}
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[0.6875rem] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={Bot} size={13} strokeWidth={1.75} class="opacity-80" />
  <span data-composer-toolbar-label="collapsible">{modeLabel}</span>
</button>
