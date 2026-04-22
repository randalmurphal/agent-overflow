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

  import MessagesSquare from 'lucide-svelte/icons/messages-square';
  import ListTodo from 'lucide-svelte/icons/list-todo';
  import Palette from 'lucide-svelte/icons/palette';
  import MessageCircle from 'lucide-svelte/icons/message-circle';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { UpdateThreadMode } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { cycleMode, type CycleMode } from '../../../utils/modeCycle';
  import { errString } from '../../../utils/errors';
  import Icon from '../../primitives/Icon.svelte';

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type IconComponent = any;

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

  interface ModeMeta {
    label: string;
    icon: IconComponent;
  }

  const MODE_META: Record<CycleMode | 'discussion', ModeMeta> = {
    chat: { label: 'Chat', icon: MessagesSquare },
    plan: { label: 'Plan', icon: ListTodo },
    design: { label: 'Design', icon: Palette },
    discussion: { label: 'Discussion', icon: MessageCircle },
  };

  let meta = $derived(MODE_META[currentMode] ?? MODE_META.chat);

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
  aria-label="Cycle interaction mode (Shift+Tab)"
  title={`Mode: ${meta.label} — Shift+Tab to cycle`}
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'transition-colors cursor-pointer',
    'hover:text-fg hover:bg-surface-2/30',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    'disabled:opacity-60 disabled:cursor-not-allowed',
  ].join(' ')}
>
  <Icon icon={meta.icon} size={13} strokeWidth={1.75} class="opacity-80" />
  <span>{meta.label}</span>
</button>
