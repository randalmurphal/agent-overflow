<script lang="ts">
  // Sticky banner across the top of the app shell that surfaces the
  // wsClient's current connection state. Hidden on the happy path
  // ('connected') so it doesn't compete with the chat header for screen
  // real estate; only renders when the transport is reconnecting or has
  // settled into an idle disconnected state.
  //
  // The reconnecting state can carry a scheduled-attempt timestamp
  // (`nextAttemptAt`) when the wsClient has queued a backoff timer; we
  // show a coarse "Reconnecting in Ns" countdown driven off a 1Hz
  // interval. While an attempt is in-flight (`nextAttemptAt === null`)
  // we drop the countdown and show "Reconnecting…" so the user knows
  // the client is actively trying.
  //
  // The Retry button is a manual escape hatch for the rare case where
  // the backoff loop has settled into a long delay and the user wants
  // to force an attempt sooner. It calls wsClient.triggerReconnect via
  // the store, which resets the backoff counter.

  import { slide } from 'svelte/transition';
  import { getTransportStatus, retryTransport } from '../../stores/transportStatus.svelte';

  // Tick once per second so the countdown stays in sync. We only mount
  // when the banner is visible; on a steady-state connection the
  // interval never starts.
  let tick = $state(Date.now());
  let timer: ReturnType<typeof setInterval> | null = null;

  let snapshot = $derived(getTransportStatus());

  // Boot-time flash suppression. The wsClient starts in `disconnected`
  // (see transport/wsClient.ts), so on every SPA mount the banner would
  // briefly render a red "Disconnected from the agent backend." card
  // for the few hundred ms between this component mounting and the
  // initial WebSocket handshake completing. That looks like an error,
  // not a normal boot. Track two state flags to gate visibility:
  //
  //   - hasEverConnected — flips true once the WS has connected at
  //     least once. Subsequent disconnects in the same session are
  //     real and worth showing immediately, with no grace.
  //   - bootGraceExpired — flips true 1 s after mount if we haven't
  //     connected yet. Catches the legitimate failure case where the
  //     initial handshake never lands and the user would otherwise
  //     stare at a UI with no feedback.
  let hasEverConnected = $state(false);
  let bootGraceExpired = $state(false);

  $effect(() => {
    if (snapshot.status === 'connected') {
      hasEverConnected = true;
    }
  });

  $effect(() => {
    if (hasEverConnected) return;
    const t = setTimeout(() => {
      bootGraceExpired = true;
    }, 1000);
    return () => clearTimeout(t);
  });

  let visible = $derived(
    snapshot.status !== 'connected' && (hasEverConnected || bootGraceExpired),
  );

  $effect(() => {
    if (!visible) {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
      return;
    }
    if (timer === null) {
      timer = setInterval(() => {
        tick = Date.now();
      }, 1000);
    }
    return () => {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    };
  });

  let countdown = $derived.by(() => {
    if (snapshot.status !== 'reconnecting') return null;
    if (snapshot.nextAttemptAt === null) return null;
    const remainingMs = snapshot.nextAttemptAt - tick;
    if (remainingMs <= 0) return null;
    return Math.ceil(remainingMs / 1000);
  });

  let bannerClasses = $derived.by(() => {
    if (snapshot.status === 'reconnecting') {
      return 'bg-warning/15 border-warning/30 text-warning';
    }
    return 'bg-error/15 border-error/30 text-error';
  });

  let message = $derived.by(() => {
    if (snapshot.status === 'reconnecting') {
      if (countdown !== null) {
        return `Reconnecting in ${countdown}s…`;
      }
      return 'Reconnecting…';
    }
    return 'Disconnected from the agent backend.';
  });

  function handleRetry(): void {
    retryTransport();
  }
</script>

{#if visible}
  <div
    transition:slide={{ duration: 150 }}
    role="alert"
    aria-live="polite"
    data-testid="transport-status-banner"
    data-status={snapshot.status}
    class="border-b {bannerClasses} px-4 py-1.5 flex items-center gap-2 shrink-0 text-xs"
  >
    <p class="flex-1 line-clamp-1" title={message}>{message}</p>
    {#if snapshot.status === 'disconnected' || snapshot.status === 'reconnecting'}
      <button
        type="button"
        onclick={handleRetry}
        data-testid="transport-status-retry"
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-white/5 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Retry
      </button>
    {/if}
  </div>
{/if}
