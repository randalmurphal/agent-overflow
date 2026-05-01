<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { isUiRenderTraceEnabled, recordUiTrace } from '../../utils/uiRenderTrace';
  import Icon from '../primitives/Icon.svelte';
  import { dispatchInterrupt } from '../composer/composerSend';
  import Square from 'lucide-svelte/icons/square';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let now = $state(Date.now());
  let interrupting = $state(false);

  // activeTurn reflects the current WIRE ROUND, not the user-typed
  // logical turn. Each round (Claude `result` envelope, Codex
  // `turn/completed`) is its own active turn from the frontend's
  // POV — the multi-result-per-turn cascade flips this off between
  // rounds and back on for the next round, which is what makes the
  // working indicator and Stop button correctly reflect "model is
  // engaged right now" rather than "user-typed prompt is in flight."
  // The elapsed timer below naturally resets per round (anchors on
  // activeTurn.startedAt, allocated by the per-round handler in
  // turn_lifecycle.go). See internal/triage/AGENTS.md "Wire-round vs
  // logical-turn".
  let activeTurn = $derived(getActiveTurn(pane.threadId));
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

  $effect(() => {
    pane.threadId;
    activeTurn?.turnId;
    interrupting;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('working-indicator.state', {
      threadId: pane.threadId,
      activeTurn: activeTurn
        ? {
            turnId: activeTurn.turnId,
            turnIndex: activeTurn.turnIndex,
            startedAt: activeTurn.startedAt,
          }
        : null,
      interrupting,
      visible: isWorking,
    });
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
    class="group mb-6 flex items-center gap-2 py-0.5 pl-1.5 text-[11px] text-fg-hint/70"
    role="status"
    aria-live="polite"
    data-testid="chat-working-indicator"
    data-turn-id={activeTurn?.turnId}
    data-round-id={activeTurn?.turnId}
  >
    <span class="inline-flex items-center gap-[3px]" aria-hidden="true">
      <span class="h-1 w-1 rounded-full bg-fg-hint/65 animate-pulse"></span>
      <span class="h-1 w-1 rounded-full bg-fg-hint/65 animate-pulse [animation-delay:200ms]"></span>
      <span class="h-1 w-1 rounded-full bg-fg-hint/65 animate-pulse [animation-delay:400ms]"></span>
    </span>
    <span>
      Working for
      <span class="tabular-nums" data-testid="chat-working-indicator-elapsed">{elapsedLabel}</span>
    </span>
    <button
      type="button"
      class="inline-flex h-5 w-5 items-center justify-center rounded-full text-fg-hint/65 opacity-0 transition-opacity hover:bg-surface-2/50 hover:text-fg-muted focus-visible:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 disabled:cursor-not-allowed disabled:opacity-40 group-hover:opacity-100"
      onclick={interrupt}
      disabled={interrupting}
      data-testid="chat-working-indicator-interrupt"
      aria-label="Interrupt Current Turn"
      title="Interrupt Current Turn"
    >
      <Icon icon={Square} size={10} strokeWidth={2.5} class={interrupting ? 'animate-pulse' : ''} />
    </button>
  </div>
{/if}
