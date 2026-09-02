<script lang="ts">
  // Overlay banner pinned to the top of the app shell that surfaces the
  // wsClient's current connection state. Hidden on the happy path
  // ('connected') so it reserves no space above the chat header; only
  // renders — as an absolute overlay, see the markup comment below — when
  // the transport is reconnecting or has settled into an idle
  // disconnected state.
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
  //
  // Two states the automatic loop cannot resolve, and the wsClient stops
  // retrying on both, so this banner is the whole recovery story for
  // each and has to name the action that actually works — otherwise the
  // user watches "Reconnecting…" forever on a client that is no longer
  // even trying. 'unauthorized' is a refused credential, which is what a
  // remote/LAN client sees after the backend restarts (credentials are
  // minted per launch); 'pairing-required' is a networked page this
  // backend will not open a socket for until the device is paired. The
  // sentence for either comes from transport/connectionRefusal.ts, the
  // one module that phrases them. Retry stays because triggerReconnect
  // un-latches (one attempt, user-initiated); the countdown does not,
  // because no attempt is scheduled.

  // A passkey is the one recovery either terminal state has that does not
  // need somebody at the other computer, so this banner is where it is
  // offered — a browser that has never paired, or one whose session family
  // ended, both arrive here and both are exactly who a registered
  // credential is for (docs/specs/remote-access.md §4 "Passkeys"). It
  // re-attaches through the SAME redial pairing uses rather than a second
  // recovery path; nothing about the ladder changes.
  import { fade } from 'svelte/transition';
  import {
    connectionRefusalMessage,
    isTerminalConnectionStatus,
  } from '../../transport/connectionRefusal';
  import { PasskeyAbandonedError, passkeysUsable } from '../../transport/passkey';
  import { signInWithPasskey, unpairHome } from '../../transport/deviceSession';
  import { hasHomeEndpoint } from '../../transport/homeEndpoint';
  import { errString } from '../../utils/errors';
  import {
    getTransportStatus,
    redialAfterSignIn,
    retryTransport,
  } from '../../stores/transportStatus.svelte';

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

  // A page that mounted while the transport was TERMINAL loaded nothing.
  // Every store's first fetch was refused by the latched client, and only
  // entity-keyed stores re-acquire when a connection arrives
  // (stores/entityStore.svelte.ts) — the sidebar's projects and threads,
  // settings, keybindings, the pane layout and the persisted app storage
  // each load once on mount and never again. So leaving a terminal state
  // from this banner attached the socket and left an EMPTY app (found by
  // e2e/tests/harness-passkey-lifecycle.spec.ts).
  //
  // Booting again is what the pairing-link path gets by construction:
  // main.ts mounts App only AFTER the redial, so its fan-out runs once,
  // attached. This is that same thing for a page that was already
  // mounted, and it covers every way out of a terminal state rather than
  // the sign-in button alone.
  //
  // The guard is what makes it cost nothing: it fires only on the FIRST
  // connection of a page that has been terminal, where there is no loaded
  // state and nothing anybody typed to discard. A session that dies
  // mid-use and is signed back in keeps its page. Plain `let`, not
  // `$state`: these are the effect's own memory and nothing renders from
  // them, so making them reactive would only add a dependency that can
  // re-run it.
  let sawTerminal = false;
  let connectedOnce = false;

  $effect(() => {
    const status = snapshot.status;
    if (isTerminalConnectionStatus(status)) {
      sawTerminal = true;
      return;
    }
    if (status !== 'connected') return;
    const first = !connectedOnce;
    connectedOnce = true;
    if (first && sawTerminal) location.reload();
  });

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
    if (isTerminalConnectionStatus(snapshot.status)) {
      return connectionRefusalMessage(snapshot.status);
    }
    return 'Disconnected from the agent backend.';
  });

  function handleRetry(): void {
    retryTransport();
  }

  // Offered only where it can work: a terminal state (the ladder has
  // stopped, so there is something to recover FROM), a backend with a
  // domain, and a page that can hold a credential. A page whose origin is
  // not its backend's (the phone shell) is excluded: a passkey is bound
  // to the backend's domain, and the browser refuses the ceremony from
  // any other origin, so the button could only fail.
  let terminal = $derived(isTerminalConnectionStatus(snapshot.status));
  let signInOffered = $derived(terminal && !hasHomeEndpoint() && passkeysUsable());
  // The shell's recovery instead. A browser is one navigation away from a
  // new pairing link; a fixed-origin page has nothing to navigate to, so
  // it forgets home and boots into "scan a code" again.
  let pairAgainOffered = $derived(terminal && hasHomeEndpoint());
  let signingIn = $state(false);
  let signInError = $state('');

  function handlePairAgain(): void {
    unpairHome();
    location.reload();
  }

  async function handleSignIn(): Promise<void> {
    if (signingIn) return;
    signingIn = true;
    signInError = '';
    try {
      await signInWithPasskey(navigator.platform || 'Browser');
      // Awaited, for the reason main.ts awaits it after pairing: the app
      // is already mounted here, and its stores re-acquire on the
      // reconnect this settles.
      await redialAfterSignIn();
    } catch (err) {
      // A dismissed prompt is not a failure; the banner simply stays.
      if (!(err instanceof PasskeyAbandonedError)) signInError = errString(err);
    } finally {
      signingIn = false;
    }
  }
</script>

<!-- Overlay pattern: the banner is absolutely positioned at the top of the
     app shell, so it reserves NO layout height. On the happy path nothing
     renders here and the chat header sits flush with the top of the window;
     when the transport drops the banner floats over the top edge of the
     content without changing the panes' clientHeight. A clientHeight change
     would fight the scroll controller (see
     docs/architecture/frontend-scroll.md), which is why the old reserved-slot version permanently
     held ~28px — but this banner spans the whole shell and a transport
     reconnect is rare, so overlaying beats reserving that space forever.
     z-50 keeps it above pane content and below modals (z-[60]+), matching
     the pre-overlay behavior where a full-screen modal covered the banner.
     transition:fade animates opacity only, so entrance/exit never shifts
     layout either. (ProviderStatusBanner stays a reserved slot: it sits
     between the header and the timeline, a narrower surface where a stable
     slot is the simpler win.) -->
{#if visible}
  <div
    transition:fade={{ duration: 150 }}
    role="alert"
    aria-live="polite"
    data-testid="transport-status-banner"
    data-status={snapshot.status}
    class="absolute inset-x-0 top-0 z-50 border-b {bannerClasses} px-4 py-1.5 flex items-center gap-2 text-xs"
  >
    <p class="flex-1 line-clamp-1" title={signInError || message}>{signInError || message}</p>
    {#if signInOffered}
      <button
        type="button"
        onclick={() => void handleSignIn()}
        disabled={signingIn}
        data-testid="transport-status-passkey"
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 disabled:cursor-default disabled:opacity-60"
      >
        Sign in with a passkey
      </button>
    {/if}
    {#if pairAgainOffered}
      <button
        type="button"
        onclick={handlePairAgain}
        data-testid="transport-status-pair-again"
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Pair again
      </button>
    {/if}
    {#if snapshot.status !== 'connected'}
      <button
        type="button"
        onclick={handleRetry}
        data-testid="transport-status-retry"
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Retry
      </button>
    {/if}
  </div>
{/if}
