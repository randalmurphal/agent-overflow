<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { dispatchInterrupt } from '../composer/composerSend';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let now = $state(Date.now());
  let interrupting = $state(false);

  let activeTurn = $derived(pane.activeTurn);
  let isWorking = $derived(activeTurn !== null);

  $effect(() => {
    if (!activeTurn) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  let elapsedLabel = $derived.by(() => {
    if (!activeTurn) return '0s';
    const elapsedSeconds = Math.max(0, Math.floor((now - activeTurn.startedAt) / 1_000));
    return formatElapsedSeconds(elapsedSeconds);
  });

  async function interrupt(): Promise<void> {
    const threadID = pane.threadId;
    if (!threadID || !activeTurn || interrupting) return;
    interrupting = true;
    try {
      await dispatchInterrupt(threadID, (message) => pane.setGeneralError(message));
    } finally {
      interrupting = false;
    }
  }
</script>

{#if isWorking}
  <div
    class="mx-6 mb-2 flex items-center justify-center gap-2 text-[11px] text-text-secondary"
    role="status"
    aria-live="polite"
    data-testid="chat-working-indicator"
    data-turn-id={activeTurn?.turnId}
  >
    <span class="h-1.5 w-1.5 rounded-full bg-accent animate-pulse" aria-hidden="true"></span>
    <span>Working</span>
    <span aria-hidden="true">·</span>
    <span class="tabular-nums" data-testid="chat-working-indicator-elapsed">{elapsedLabel}</span>
    <span aria-hidden="true">·</span>
    <button
      type="button"
      class="rounded-[var(--radius-field)] px-1.5 py-0.5 text-[11px] text-text-secondary hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-60"
      onclick={interrupt}
      disabled={interrupting}
      data-testid="chat-working-indicator-interrupt"
      aria-label="Interrupt current turn"
    >
      {interrupting ? 'Interrupting...' : 'Esc to interrupt'}
    </button>
  </div>
{/if}
