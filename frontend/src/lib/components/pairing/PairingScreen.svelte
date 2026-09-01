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
  import { suggestDeviceLabel } from '../../utils/deviceLabel';
  import {
    PairingRefusedError,
    endpointMatchesOrigin,
    probeActivation,
    redeemPairing,
    signInWithPasskey,
    type PairingPayload,
  } from '../../transport/deviceSession';

  interface Props {
    /** Null when the fragment could not be read; `parseError` then says why. */
    payload: PairingPayload | null;
    parseError?: string;
    /** Called once the pairing is confirmed and the app should boot. */
    onDone: () => void;
  }

  let { payload, parseError, onDone }: Props = $props();

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
    | { at: 'failed'; title: string; hint: string };

  // The props are set once by main.ts and never change; capturing their
  // initial value is the point.
  // svelte-ignore state_referenced_locally
  let stage = $state<Stage>(
    payload === null
      ? {
          at: 'failed',
          title: parseError || 'This pairing link could not be read.',
          hint: 'Ask for a new pairing link from the app on your computer.',
        }
      : { at: 'intro' },
  );
  let label = $state(suggestDeviceLabel());
  let probeTimer: ReturnType<typeof setTimeout> | null = null;

  const backendName = $derived(
    payload === null ? '' : payload.backendName || new URL(payload.endpoint).host,
  );

  async function pair(): Promise<void> {
    if (stage.at !== 'intro' || payload === null) return;
    if (!endpointMatchesOrigin(payload, location.origin)) {
      stage = {
        at: 'failed',
        title: 'This link belongs to a different address.',
        hint: 'Open the pairing link exactly as it was shared, without editing it.',
      };
      return;
    }
    stage = { at: 'redeeming' };
    try {
      const outcome = await redeemPairing(payload, label.trim() || suggestDeviceLabel());
      stage = { at: 'waiting', verificationNumber: outcome.verificationNumber };
      scheduleProbe(Date.now() + PROBE_DEADLINE_MS);
    } catch (err) {
      if (err instanceof PairingRefusedError) {
        const shown = presentAuthReason(err.reason);
        stage = { at: 'failed', title: shown.title, hint: shown.hint };
      } else {
        stage = {
          at: 'failed',
          title: 'Pairing did not go through.',
          hint: 'Check that this device is on the same network, then ask for a new link.',
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
      await signInWithPasskey(label.trim() || suggestDeviceLabel());
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
        stage = { at: 'failed', title: shown.title, hint: shown.hint };
        return;
      }
      stage = {
        at: 'failed',
        title: 'Signing in did not go through.',
        hint: 'Check that this device is on the same network, then try again.',
      };
    }
  }

  function scheduleProbe(deadline: number): void {
    probeTimer = setTimeout(async () => {
      probeTimer = null;
      const admitted = await probeActivation();
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
        };
        return;
      }
      scheduleProbe(deadline);
    }, PROBE_INTERVAL_MS);
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
          This device will get its own access to <span class="font-medium">{backendName}</span>,
          which you can review or revoke there at any time.
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
      <p class="max-w-72 text-sm text-text-secondary">{stage.hint}</p>
    {/if}
  </div>
</div>
