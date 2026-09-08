<script lang="ts">
  // The redeeming half of device pairing (docs/specs/remote-access.md §4):
  // what a pairing link opens on the NEW device. Mounted by main.ts in
  // place of the app when the URL carries a `#pair=` fragment; the app
  // shell boots only after this screen finishes or the person abandons it.
  //
  // The flow the screen walks: name the device → spend the link
  // (deviceSession.redeemPairing) → show the verification number the
  // owner compares on their own screen → probe until the owner confirms
  // → hand back to main.ts. Refusals speak through authReason, the one
  // module that turns a refusal code into a sentence.
  import { onDestroy } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { presentAuthReason } from '../../transport/authReason';
  import { PasskeyAbandonedError, passkeysUsable } from '../../transport/passkey';
  import { userFacingError } from '../../utils/userFacingError';
  import { clientDeviceName, saveClientDeviceName } from '../../stores/clientDeviceName.svelte';
  import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
  import { networkFetch } from '../../transport/networkFetch';
  import { isNativeShell } from '../../native/platform';
  import { detachAttachedBackend } from '../../transport/backendAttach';
  import {
    PairingRefusedError,
    acceptPairingEndpoint,
    hasPairedSession,
    probeActivation,
    redeemPairing,
    signInWithPasskey,
    type PairingPayload,
  } from '../../transport/deviceSession';

  interface Props {
    /** Null when the fragment could not be read; `parseError` then says why. */
    payload: PairingPayload | null;
    parseError?: string;
    backend?: BackendKey;
    /** Called once the pairing is confirmed and the app should boot. */
    onDone: () => void;
  }

  let { payload, parseError, onDone, backend = HOME_BACKEND }: Props = $props();

  // How often the waiting state asks whether the owner confirmed, and
  // for how long. The confirm window on the other side is ten minutes
  // (identity.PairingConfirmWindow); probing a beat past it just yields
  // the timeout message a little late.
  const PROBE_INTERVAL_MS = 3_000;
  const PROBE_DEADLINE_MS = 10 * 60_000;

  type Stage =
    | { at: 'intro' }
    | { at: 'redeeming' }
    | { at: 'waiting'; verificationNumber: string }
    | { at: 'ready' }
    // `retryable` is whether the same link can still work: a request that
    // never reached the computer can be sent again, a link that was spent,
    // expired, unreadable or never confirmed needs a new one.
    | { at: 'failed'; title: string; hint: string; retryable: boolean };

  // The props are set once by main.ts and never change; capturing their
  // initial value is the point.
  // svelte-ignore state_referenced_locally
  let stage = $state<Stage>(
    payload === null
      ? {
          at: 'failed',
          title: parseError || 'This pairing link could not be read.',
          hint: 'Ask for a new pairing link from the app on your computer.',
          retryable: false,
        }
      : { at: 'intro' },
  );
  // The shell's way out of a link that cannot work: back to the scan
  // screen. A slot this attempt opened is closed first so the next boot
  // does not attach a computer that never confirmed; a computer already
  // paired before this screen keeps its credential, which the failure
  // never touched. A browser has no scan screen to go back to.
  const startOverOffered = isNativeShell();
  // svelte-ignore state_referenced_locally
  const pairedBefore = hasPairedSession(backend);
  let label = $state(clientDeviceName());
  let probeTimer: ReturnType<typeof setTimeout> | null = null;

  const backendName = $derived(
    payload === null ? '' : payload.backendName || new URL(payload.endpoint).host,
  );

  async function pair(): Promise<void> {
    if (stage.at !== 'intro' || payload === null) return;
    // A browser checks the payload against the origin it is on; a shell
    // page, which can never be its backend's origin, ADOPTS what the
    // payload names. One call, because it is one decision about where
    // this redemption is going (transport/deviceSession.ts).
    if (!acceptPairingEndpoint(payload, location.origin, backend)) {
      stage = {
        at: 'failed',
        title: 'This link belongs to a different address.',
        hint: 'Open the pairing link exactly as it was shared, without editing it.',
        retryable: false,
      };
      return;
    }
    stage = { at: 'redeeming' };
    try {
      saveClientDeviceName(label);
      const outcome = await redeemPairing(payload, clientDeviceName(), networkFetch, backend);
      stage = { at: 'waiting', verificationNumber: outcome.verificationNumber };
      scheduleProbe(Date.now() + PROBE_DEADLINE_MS);
    } catch (err) {
      if (err instanceof PairingRefusedError) {
        const shown = presentAuthReason(err.reason);
        stage = { at: 'failed', title: shown.title, hint: shown.hint, retryable: shown.retryable };
      } else {
        // Nothing was spent: the request never got an answer.
        stage = {
          at: 'failed',
          title: 'Pairing did not go through.',
          hint: userFacingError(err) || 'Check that this device is on the same network, then try again.',
          retryable: true,
        };
      }
    }
  }

  // The other way in, when this backend has a passkey to offer: no link
  // to open, no number to compare, no waiting. A valid assertion is a
  // signature by a key the owner registered from a surface that already
  // held admin, so the session it mints is live on arrival and the screen
  // goes straight to `ready` — the same hand-off pairing takes, so the
  // redial main.ts awaits is not forked.
  const passkeyOffered = passkeysUsable();
  // Which way in succeeded, for the one word the ready state shows. The
  // two outcomes are genuinely different — one enrolled a device the owner
  // confirmed, the other signed in as the owner — and calling both
  // "Paired" would describe the second one wrongly.
  let signedInWithPasskey = $state(false);

  async function signIn(): Promise<void> {
    if (stage.at !== 'intro') return;
    stage = { at: 'redeeming' };
    try {
      saveClientDeviceName(label);
      await signInWithPasskey(clientDeviceName());
      signedInWithPasskey = true;
      stage = { at: 'ready' };
      setTimeout(onDone, 700);
    } catch (err) {
      if (err instanceof PasskeyAbandonedError) {
        // Nothing went wrong. Back to where they were, with no message.
        stage = { at: 'intro' };
        return;
      }
      if (err instanceof PairingRefusedError) {
        const shown = presentAuthReason(err.reason);
        stage = { at: 'failed', title: shown.title, hint: shown.hint, retryable: shown.retryable };
        return;
      }
      stage = {
        at: 'failed',
        title: 'Signing in did not go through.',
        hint: userFacingError(err) || 'Check that this device is on the same network, then try again.',
        retryable: true,
      };
    }
  }

  function scheduleProbe(deadline: number): void {
    probeTimer = setTimeout(async () => {
      probeTimer = null;
      const admitted = await probeActivation(networkFetch, backend);
      if (admitted) {
        stage = { at: 'ready' };
        // A short beat so the confirmation lands visually before the
        // screen is replaced by the app booting.
        setTimeout(onDone, 700);
        return;
      }
      if (stage.at !== 'waiting') return;
      if (Date.now() >= deadline) {
        stage = {
          at: 'failed',
          title: 'This pairing was not confirmed in time.',
          hint: 'Ask for a new pairing link from the app on your computer.',
          retryable: false,
        };
        return;
      }
      scheduleProbe(deadline);
    }, PROBE_INTERVAL_MS);
  }

  // Back to the intro with the same payload: the link is still good, and
  // the name field keeps what was typed.
  function tryAgain(): void {
    if (stage.at === 'failed' && stage.retryable) stage = { at: 'intro' };
  }

  function startOver(): void {
    if (!pairedBefore) detachAttachedBackend(backend);
    history.replaceState(null, '', location.pathname + location.search);
    location.reload();
  }

  onDestroy(() => {
    if (probeTimer !== null) clearTimeout(probeTimer);
  });
</script>

<div class="flex min-h-screen items-center justify-center bg-surface-0 p-6">
  <div class="flex w-full max-w-88 flex-col items-center gap-6 text-center">
    <div class="flex flex-col items-center gap-2">
      <MicroLabel as="p" class="text-fg-hint">Agent Overflow</MicroLabel>
      <h1 class="text-lg font-semibold text-text-primary">
        {#if stage.at === 'ready'}
          {signedInWithPasskey ? 'Signed in' : 'Paired'}
        {:else if stage.at === 'failed'}
          {stage.title}
        {:else if stage.at === 'waiting'}
          Confirm on your computer
        {:else}
          Pair this device
        {/if}
      </h1>
      {#if stage.at === 'intro'}
        <p class="text-sm text-text-secondary">
          {#if payload?.purpose === 'own-device'}
            Join your devices through <span class="font-medium">{backendName}</span>.
            Your devices will connect to each other automatically, including computers already in the group.
          {:else}
            This device will get its own access to <span class="font-medium">{backendName}</span>,
            which you can review or revoke there at any time.
          {/if}
        </p>
      {/if}
    </div>

    {#if stage.at === 'intro'}
      <form
        class="flex w-full flex-col gap-3"
        onsubmit={(e) => {
          e.preventDefault();
          void pair();
        }}
      >
        <label class="flex flex-col gap-1.5 text-left">
          <span class="text-xs font-medium text-text-secondary">Device name</span>
          <!-- svelte-ignore a11y_autofocus -->
          <input
            class="rounded-md border border-border bg-surface-1 px-3 py-2 text-sm text-text-primary outline-none focus:border-accent"
            type="text"
            bind:value={label}
            maxlength={64}
            autofocus
          />
        </label>
        <Button type="submit" variant="primary" size="md">Pair</Button>
        {#if passkeyOffered}
          <!-- The other way in, for a person who already registered a
               passkey: no number to compare, because the assertion is the
               confirmation. Offered beside pairing rather than instead of
               it — a first device has no passkey yet, and that is the case
               a link exists for. -->
          <Button variant="ghost" size="md" onclick={() => void signIn()}>
            Sign in with a passkey
          </Button>
        {/if}
      </form>
    {:else if stage.at === 'redeeming'}
      <SteppedSpinner size={16} />
    {:else if stage.at === 'waiting'}
      <div class="flex flex-col items-center gap-4">
        <p
          class="text-4xl font-semibold tracking-[0.25em] text-text-primary tabular-nums"
          aria-label="Verification number"
        >
          {stage.verificationNumber}
        </p>
        <p class="max-w-72 text-sm text-text-secondary">
          Make sure this number matches the one shown on your computer, then allow the
          pairing there.
        </p>
        <div class="flex items-center gap-2 text-xs text-fg-muted">
          <SteppedSpinner size={11} />
          <span>Waiting for confirmation</span>
        </div>
      </div>
    {:else if stage.at === 'ready'}
      <p class="text-sm text-text-secondary">Opening…</p>
    {:else if stage.at === 'failed'}
      <div class="flex w-full flex-col items-center gap-4">
        <p class="max-w-72 text-sm text-text-secondary">{stage.hint}</p>
        {#if stage.retryable}
          <Button variant="primary" size="md" onclick={tryAgain}>Try again</Button>
        {:else if startOverOffered}
          <Button variant="primary" size="md" onclick={startOver}>Start over</Button>
        {/if}
      </div>
    {/if}
  </div>
</div>
