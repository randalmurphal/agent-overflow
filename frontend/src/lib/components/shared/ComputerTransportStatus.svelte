<script lang="ts">
  import { suggestDeviceLabel } from '../../utils/deviceLabel';
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
  import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
  import { backendById, attachedBackendCount } from '../../transport/backends';
  import { isNativeShell } from '../../native/platform';
  import { openSettingsOverlay } from '../../stores/settingsOverlay.svelte';
  import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  let { backend }: { backend: BackendKey } = $props();
  import {
    connectionRefusalMessage,
    isTerminalConnectionStatus,
  } from '../../transport/connectionRefusal';
  import { PasskeyAbandonedError, passkeysUsable } from '../../transport/passkey';
  import { signInWithPasskey, unpairHome } from '../../transport/deviceSession';
  import { hasHomeEndpoint } from '../../transport/homeEndpoint';
  import { errString } from '../../utils/errors';
  import { relativeTime } from '../../utils/format';
  import {
    getTransportStatus,
    getTransportStatusFor,
    redialAfterSignIn,
    retryTransport,
  } from '../../stores/transportStatus.svelte';
  // The phone shell's update channel says at most two things, and this
  // strip is where they are said: it is already the place this client
  // states facts about its relationship with its backend, and "your desk
  // has a newer app than your phone" is one of those
  // (stores/bundleNotice.svelte.ts). Empty on every other client, which
  // is every client that cannot install a bundle.
  import { dismissBundleNotice, getBundleNotice } from '../../stores/bundleNotice.svelte';

  // Tick once per second so the countdown stays in sync. We only mount
  // when the banner is visible; on a steady-state connection the
  // interval never starts.
  let tick = $state(Date.now());
  let timer: ReturnType<typeof setInterval> | null = null;

  let snapshot = $derived(backend === HOME_BACKEND ? getTransportStatus() : getTransportStatusFor(backend));
  let removed = $derived(backend !== HOME_BACKEND && !attachedBackendEntry(backend));
  let computerName = $derived.by(() => {
    const entry = attachedBackendEntry(backend);
    return entry && attachedBackendCount() > 1 ? backendDisplayName(entry) : '';
  });

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

  let bundleNotice = $derived(getBundleNotice());

  // A connection problem outranks a bundle notice: one is happening now
  // and the other is about the next launch. The notice keeps the strip
  // up on its own once the transport is healthy again.
  let visible = $derived(
    (snapshot.status !== 'connected' && (hasEverConnected || bootGraceExpired))
      || bundleNotice !== '',
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
    if (first && sawTerminal && backend === HOME_BACKEND && !isNativeShell() && attachedBackendCount() === 1) location.reload();
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
    if (snapshot.status === 'connected') {
      return 'bg-fg/10 border-fg/20 text-fg-muted';
    }
    if (snapshot.status === 'reconnecting') {
      return 'bg-warning/15 border-warning/30 text-warning';
    }
    return 'bg-error/15 border-error/30 text-error';
  });

  // A ladder that has been reconnecting for minutes goes dormant: one probe
  // every five minutes and nothing else on the network (transport/wsClient.ts).
  // The countdown is the wrong sentence there — a number ticking down from 300
  // reads as "nearly back" every five minutes forever — so the banner states
  // the fact instead, and says when the connection was last alive, which is the
  // one thing that tells a person whether this started just now or overnight.
  //
  // `lastConnectedAt` is this device's own clock reading, so `relativeTime` is
  // called with no backend id: correcting it by a backend's skew would be
  // measuring one clock against another's offset. The 1Hz tick is read so the
  // sentence ages with the banner rather than freezing at whatever it said when
  // dormancy began.
  //
  // Null when this client has NEVER connected — a page that opened against a
  // backend that was already gone. The dormant sentence still applies (the
  // cadence is the same fact), it simply drops the clause it has no answer
  // for. It does NOT fall back to the countdown: the ladder really is at one
  // probe per five minutes, and a number ticking down would misdescribe it.
  let lastSeen = $derived.by(() => {
    if (snapshot.status !== 'reconnecting') return null;
    if (!snapshot.dormant) return null;
    const at = snapshot.lastConnectedAt ?? null;
    if (at === null) return null;
    void tick;
    return relativeTime(at);
  });

  let dormant = $derived(snapshot.status === 'reconnecting' && snapshot.dormant === true);

  let message = $derived.by(() => {
    if (removed) return 'This computer was removed. Choose another computer.';
    if (snapshot.status === 'connected') return bundleNotice;
    if (snapshot.status === 'reconnecting') {
      if (dormant) {
        return lastSeen === null
          ? 'Not reachable. Checking every 5 minutes.'
          : `Not reachable. Last seen ${lastSeen}. Checking every 5 minutes.`;
      }
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
    if (backend === HOME_BACKEND) retryTransport();
    else backendById(backend)?.client.triggerReconnect();
  }

  // Offered only where it can work: a terminal state (the ladder has
  // stopped, so there is something to recover FROM), a backend with a
  // domain, and a page that can hold a credential. A page whose origin is
  // not its backend's (the phone shell) is excluded: a passkey is bound
  // to the backend's domain, and the browser refuses the ceremony from
  // any other origin, so the button could only fail.
  // The one persistent, healthy-transport thing this strip says. Unlike
  // every connection state it never resolves on its own — the resolution
  // is a restart the person chooses — so it is the one message that gets
  // a dismiss. Without it, on a phone the strip sat over the compact
  // thread header for the rest of the session, eating its taps (found on
  // the first real-phone run, 2026-09-04).
  let dismissable = $derived(snapshot.status === 'connected' && bundleNotice !== '');

  let terminal = $derived(isTerminalConnectionStatus(snapshot.status));
  let signInOffered = $derived(terminal && backend === HOME_BACKEND && !isNativeShell() && !hasHomeEndpoint() && passkeysUsable());
  // The shell's recovery instead. A browser is one navigation away from a
  // new pairing link; a fixed-origin page has nothing to navigate to, so
  // it forgets home and boots into "scan a code" again.
  let pairAgainOffered = $derived(terminal && (isNativeShell() || hasHomeEndpoint()));
  let signingIn = $state(false);
  let signInError = $state('');

  function handlePairAgain(): void {
    if (isNativeShell()) {
      openSettingsOverlay('systems', backend);
      return;
    }
    unpairHome();
    location.reload();
  }

  async function handleSignIn(): Promise<void> {
    if (signingIn) return;
    signingIn = true;
    signInError = '';
    try {
      await signInWithPasskey(suggestDeviceLabel());
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
     z-30 keeps it above pane content but below Settings/Workflows (z-40)
     and modals (z-[60]+). A warning must not block an overlay's close button
     when its full-height compact header shares the top edge.
     transition:fade animates opacity only, so entrance/exit never shifts
     layout either. (ProviderStatusBanner stays a reserved slot: it sits
     between the header and the timeline, a narrower surface where a stable
     slot is the simpler win.) -->
{#if visible}
  <!-- The outer layer is a solid surface: the strip floats over arbitrary
       content (on compact, over the thread header), and a translucent
       tint alone rendered as two surfaces z-mixed into an unreadable
       jumble on a real phone. The tinted look stays; it just gets an
       opaque ground first. -->
  <div
    transition:fade={{ duration: 150 }}
    class="absolute inset-x-0 top-0 z-30 bg-surface-1"
  >
  <div
    role="alert"
    aria-live="polite"
    data-testid="transport-status-banner"
    data-status={snapshot.status}
    class="border-b {bannerClasses} px-4 py-1.5 flex items-center gap-2 text-xs"
  >
    <!-- Wraps as far as it needs to: on a phone there is no hover to
         reveal a title tooltip, so a clamped sentence is simply
         unreadable (the two-line clamp still cut the bundle notice on a
         real phone, 2026-09-04). A backend's hostname can be one
         unbreakable token, hence the wrap anywhere. -->
    <p class="flex-1 min-w-0 [overflow-wrap:anywhere]">{computerName && snapshot.status !== 'connected' ? `${computerName}: ` : ''}{signInError || message}</p>
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
    {#if snapshot.status !== 'connected' && !removed}
      <button
        type="button"
        onclick={handleRetry}
        data-testid="transport-status-retry"
        class="text-xs px-2 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Retry
      </button>
    {/if}
    {#if dismissable}
      <button
        type="button"
        onclick={dismissBundleNotice}
        aria-label="Dismiss"
        data-testid="transport-status-dismiss"
        class="text-xs px-1.5 py-0.5 rounded border border-current/30 hover:bg-fg/10 cursor-pointer shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        ✕
      </button>
    {/if}
  </div>
  </div>
{/if}
