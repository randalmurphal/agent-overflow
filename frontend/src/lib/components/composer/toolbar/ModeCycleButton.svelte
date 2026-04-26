<script lang="ts">
  // Mode cycle button in the composer toolbar. Clicking cycles
  // chat → plan → design → chat. The global `mode.cycle` command
  // (Shift+Tab) calls the same backend binding so the button and the
  // keyboard shortcut stay in lockstep.
  //
  // The underlying thread may start in a mode outside the cycle (e.g.
  // "discussion" on a discussion-root thread); `cycleMode` falls back
  // to "chat" in that case, which is the right recovery for a stuck
  // mode.

  import Bot from 'lucide-svelte/icons/bot';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { UpdateThreadMode } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { cycleMode, type CycleMode } from '../../../utils/modeCycle';
  import { errString } from '../../../utils/errors';
  import Icon from '../../primitives/Icon.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let applying = $state(false);

  // Normalize the thread's current mode for display. A thread with no
  // mode value (back-compat with older rows) is presented as chat so the
  // button isn't blank; the backend writes the canonical value on the
  // next cycle.
  let currentMode = $derived<CycleMode | 'discussion'>(
    (pane.thread?.mode as CycleMode | 'discussion' | undefined) ?? 'chat',
  );

  const MODE_LABELS: Record<CycleMode | 'discussion', string> = {
    chat: 'Chat',
    plan: 'Plan',
    design: 'Design',
    discussion: 'Discussion',
  };

  let modeLabel = $derived(MODE_LABELS[currentMode] ?? MODE_LABELS.chat);

  async function handleClick(): Promise<void> {
    if (applying || !pane.thread) return;
    const next = cycleMode(currentMode);
    applying = true;
    try {
      const updated = (await UpdateThreadMode(pane.thread.id, next)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
    } catch (err) {
      console.error('mode.cycle: UpdateThreadMode failed', err);
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
  data-testid="composer-mode-cycle"
  aria-label="Cycle Interaction Mode (Shift+Tab)"
  title={`Mode: ${modeLabel} — Shift+Tab to cycle`}
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
  <span>{modeLabel}</span>
</button>
