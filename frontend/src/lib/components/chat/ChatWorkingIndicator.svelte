<script lang="ts">
  // Footer component below the Composer. Renders exclusively off
  // `pane.activeTurn` — the wire-pushed live-turn projection — per
  // invariant 22 (turn activity is wire-pushed, never derived from
  // items). Hidden when no turn is active. Never renders session-level
  // status (no "connecting", "disconnected", etc). The Esc hint is
  // advisory text only; the actual interrupt binding lives in the
  // Composer.
  //
  // Spec:
  //   docs/architecture/turn-lifecycle.md §UI components driven by this state
  //   docs/architecture/invariants.md §22
  //
  // The elapsed-seconds counter anchors to `pane.activeTurn.startedAt`
  // (ms since epoch, from `provider:turn_started`) and ticks via a
  // self-owned interval. The interval mounts when `activeTurn` becomes
  // non-null and unmounts when it flips to null, so an idle pane
  // doesn't keep a timer alive. Pattern adapted from t3-code's
  // WorkingTimer — see
  // /Users/randy/repos/t3-code/apps/web/src/components/chat/MessagesTimeline.tsx:490-497.

  import type { ThreadPane } from '../../stores/thread.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let isWorking = $derived(pane.isTurnActive);
  // `activeTurn.startedAt` is a unix-millis epoch stamped by the
  // provider on turn_started. Null when `activeTurn` is null — the
  // `isWorking` branch hides the DOM so we never render an elapsed
  // counter without an anchor.
  let anchor = $derived(pane.activeTurn?.startedAt ?? null);

  // `now` is re-seeded each time `activeTurn` flips on (fresh mount),
  // and ticked once per second by the interval below. `elapsedSeconds`
  // is a pure derivation off `now` and `anchor`, so a single tick
  // triggers a single DOM update — no separate `$effect` needed to
  // push the number into state.
  let now = $state(Date.now());

  $effect(() => {
    // Read the reactive inputs at the top so Svelte tracks them and
    // re-runs this effect when `anchor` flips (new turn started) or
    // `isWorking` flips off (turn settled).
    if (!isWorking || anchor === null) return;
    // Seed fresh on mount so the first rendered value reflects
    // wall-clock at the moment the turn became active, not whatever
    // stale `now` was last written.
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1000);
    // Returned cleanup fires when (a) the tracked inputs change and
    // the effect re-runs, or (b) the component unmounts. Both paths
    // must clear the interval — otherwise an idle pane keeps ticking
    // and a rapid turn → turn transition leaks the previous timer.
    return () => clearInterval(id);
  });

  let elapsedSeconds = $derived.by<number>(() => {
    if (!isWorking || anchor === null) return 0;
    // Clamp to zero so a backend clock skew can never render a
    // negative count.
    return Math.max(0, Math.floor((now - anchor) / 1000));
  });
</script>

{#if isWorking}
  <div
    class="flex items-center gap-1.5 border-t border-border-subtle px-5 py-1 text-[10px] text-fg-subtle"
    role="status"
    aria-live="polite"
    data-testid="chat-working-indicator"
    style="animation: pulse-subtle 2.4s ease-in-out infinite"
  >
    <span class="h-1.5 w-1.5 rounded-full bg-accent" aria-hidden="true"></span>
    <span class="uppercase tracking-[0.1em]">Working</span>
    <span aria-hidden="true">·</span>
    <span class="tabular-nums" data-testid="chat-working-indicator-elapsed">{elapsedSeconds}s</span>
    <span class="ml-auto text-fg-hint">Esc to interrupt</span>
  </div>
{/if}
