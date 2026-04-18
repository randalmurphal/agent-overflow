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

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import { UpdateThreadMode } from '../../../stores/bindings';
  import { replaceThread } from '../../../stores/threads.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { cycleMode, type CycleMode } from '../../../utils/modeCycle';

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
    icon: string;
    // Simple inline SVG path; the surrounding svg sets stroke attrs.
    iconPath: string;
  }

  const MODE_META: Record<CycleMode | 'discussion', ModeMeta> = {
    chat: {
      label: 'Chat',
      icon: '💬',
      iconPath:
        'M21 12a9 9 0 1 1-3.5-7.09L21 3v6h-6',
    },
    plan: {
      label: 'Plan',
      icon: '📋',
      iconPath:
        'M9 5h6a2 2 0 0 1 2 2v11a2 2 0 0 1-2 2H9a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zm0 0V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v1M9 11h6M9 15h4',
    },
    design: {
      label: 'Design',
      icon: '🎨',
      iconPath:
        'M12 22a10 10 0 1 1 10-10c0 2.5-1.5 4-3.5 4h-2a2 2 0 0 0-2 2 2 2 0 0 1-2 2zM7.5 10a1 1 0 1 0 0-2 1 1 0 0 0 0 2zM12 7.5a1 1 0 1 0 0-2 1 1 0 0 0 0 2zM16.5 10a1 1 0 1 0 0-2 1 1 0 0 0 0 2z',
    },
    discussion: {
      label: 'Discussion',
      icon: '💭',
      iconPath:
        'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z',
    },
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
      addToast('error', `Failed to switch mode: ${err}`);
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
    <path d={meta.iconPath} />
  </svg>
  <span>{meta.label}</span>
</button>
