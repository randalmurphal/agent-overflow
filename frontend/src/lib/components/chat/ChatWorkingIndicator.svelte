<script lang="ts">
  // Footer component below the Composer. Surfaces "the agent is working" —
  // derived purely from pane.isTurnActive, with an elapsed-seconds counter
  // read off the earliest streaming/running item's createdAt.
  //
  // Hidden when no turn is active. Never renders session-level status (no
  // "connecting", "disconnected", etc). The Esc hint is advisory text only;
  // the actual interrupt binding lives in the Composer.
  //
  // Spec: docs/architecture/chat-rewrite.md §Working indicator.

  import type { ThreadPane } from '../../stores/thread.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let isWorking = $derived(pane.isTurnActive);

  /**
   * Earliest createdAt across any streaming assistant_text / thinking item
   * or running tool_call. Falls back to null when nothing qualifies — in
   * that case isWorking is false too and the component is hidden, so the
   * counter never renders without an anchor.
   */
  let anchorCreatedAt = $derived.by<number | null>(() => {
    let earliest: number | null = null;
    for (const item of pane.items) {
      const streaming =
        (item.kind === 'assistant_text' || item.kind === 'thinking') &&
        item.status === 'streaming';
      const running = item.kind === 'tool_call' && item.status === 'running' && !item.isBackground;
      if (!streaming && !running) continue;
      if (earliest === null || item.createdAt < earliest) {
        earliest = item.createdAt;
      }
    }
    return earliest;
  });

  // Tick once per second while working. Tracked as state so the derived
  // `elapsedSeconds` recomputes on every tick without needing a second
  // $effect. When isWorking flips off we stop the interval so we don't
  // keep a timer alive for idle panes.
  let now = $state(Date.now());
  let timer: ReturnType<typeof setInterval> | null = null;

  $effect(() => {
    if (!isWorking) {
      if (timer) {
        clearInterval(timer);
        timer = null;
      }
      return;
    }
    // Seed immediately so the displayed seconds value is fresh on mount.
    now = Date.now();
    timer = setInterval(() => {
      now = Date.now();
    }, 1000);
    return () => {
      if (timer) {
        clearInterval(timer);
        timer = null;
      }
    };
  });

  let elapsedSeconds = $derived.by<number>(() => {
    if (!isWorking || anchorCreatedAt === null) return 0;
    // createdAt is a unix-millis timestamp on the pane's Item; clamp to
    // zero so a clock skew on the backend never renders a negative count.
    const diff = Math.max(0, now - anchorCreatedAt);
    return Math.floor(diff / 1000);
  });

  // No separate onDestroy — the $effect above returns a cleanup closure
  // that Svelte runs when isWorking flips OR the component unmounts.
  // A second onDestroy hook would only duplicate that teardown.
</script>

{#if isWorking}
  <div
    class="flex items-center gap-1.5 border-t border-border bg-surface-0 px-4 py-1 text-[11px] text-text-secondary/80"
    role="status"
    aria-live="polite"
    data-testid="chat-working-indicator"
  >
    <span aria-hidden="true">·</span>
    <span>Working</span>
    <span aria-hidden="true">·</span>
    <span class="tabular-nums" data-testid="chat-working-indicator-elapsed">{elapsedSeconds}s</span>
    <span aria-hidden="true">·</span>
    <span class="text-text-secondary/60">Esc to interrupt</span>
  </div>
{/if}
