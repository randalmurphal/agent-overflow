<script lang="ts">
  // First run on a phone: one screen, one button
  // (docs/specs/remote-access.md § "Pairing and remote-only").
  //
  // A phone never has a local backend, so there is nothing to show until
  // it knows where one is — and the answer to that is a QR code on the
  // owner's own desktop. What the camera reads is a pairing URL, and its
  // `#pair=` fragment is a payload `transport/deviceSession` already
  // parses, so this screen produces a payload and hands it on: the
  // existing `PairingScreen` runs the rest, unchanged, exactly as it does
  // for a browser that opened the same link.
  //
  // The one thing that happens HERE and not there is setting the home
  // endpoint. The pairing screen adopts it too
  // (`deviceSession.acceptPairingEndpoint`), but the endpoint has to be
  // set before that screen's first request, and it is this screen that
  // learns it.
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { setHomeEndpoint } from '../../transport/homeEndpoint';
  import { parsePairingFragment, type PairingPayload } from '../../transport/deviceSession';
  import { scanPairingQr } from '../../native/qr';

  interface Props {
    /** Called with the scanned payload once the endpoint is set. */
    onScanned: (payload: PairingPayload) => void;
  }

  let { onScanned }: Props = $props();

  let scanning = $state(false);
  let problem = $state('');

  async function scan(): Promise<void> {
    if (scanning) return;
    scanning = true;
    problem = '';
    try {
      const text = await scanPairingQr();
      // Null is a cancelled scan, which is not a failure and gets no
      // message: the person is back where they were, on purpose.
      if (text === null) return;
      const hash = text.slice(text.indexOf('#'));
      let payload: PairingPayload | null = null;
      try {
        payload = parsePairingFragment(hash);
      } catch (err) {
        problem = err instanceof Error ? err.message : String(err);
        return;
      }
      if (payload === null) {
        problem = 'That code is not an Agent Overflow pairing link.';
        return;
      }
      try {
        setHomeEndpoint(payload.endpoint);
      } catch {
        problem = 'That pairing link does not say where the app is. Ask for a new one.';
        return;
      }
      onScanned(payload);
    } finally {
      scanning = false;
    }
  }
</script>

<div class="flex min-h-screen items-center justify-center bg-surface-0 p-6">
  <div class="flex w-full max-w-88 flex-col items-center gap-6 text-center">
    <div class="flex flex-col items-center gap-2">
      <MicroLabel as="p" class="text-fg-hint">Agent Overflow</MicroLabel>
      <h1 class="text-lg font-semibold text-text-primary">Connect to your computer</h1>
      <p class="text-sm text-text-secondary">
        Scan the QR code from Agent Overflow, Settings, Devices.
      </p>
    </div>

    {#if scanning}
      <SteppedSpinner size={16} />
    {:else}
      <Button variant="primary" size="md" class="w-full" onclick={() => void scan()}>Scan</Button>
    {/if}

    {#if problem}
      <p class="max-w-72 text-sm text-text-secondary">{problem}</p>
    {/if}
  </div>
</div>
