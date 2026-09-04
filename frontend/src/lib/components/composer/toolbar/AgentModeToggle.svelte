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

  import Bot from '@lucide/svelte/icons/bot';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { CycleMode } from '../../../utils/modeCycle';
  import { currentAgentMode, cycleAgentMode } from './agentModeCycle';
  import Icon from '../../primitives/Icon.svelte';
  import { chordHintForCommand, chordHintSuffix } from '../../../stores/keybindings.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let applying = $state(false);

  // Normalize the thread's current mode for display. A thread with no mode
  // value (back-compat with older rows) is presented as chat.
  let currentMode = $derived<CycleMode>(currentAgentMode(pane));

  const MODE_LABELS: Record<CycleMode, string> = {
    chat: 'Build',
    plan: 'Plan',
  };

  let modeLabel = $derived(MODE_LABELS[currentMode] ?? MODE_LABELS.chat);
  let cycleChord = $derived(chordHintForCommand('mode.cycle'));
  let cycleChordSuffix = $derived(chordHintSuffix('mode.cycle'));

  async function handleClick(): Promise<void> {
    if (applying || !pane.thread) return;
    applying = true;
    try {
      await cycleAgentMode(pane);
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
  aria-label={cycleChord
    ? `Agent mode: ${modeLabel}. Toggle with ${cycleChord}`
    : `Agent mode: ${modeLabel}. Toggle`}
  title={`Agent mode: ${modeLabel}${cycleChordSuffix}`}
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
