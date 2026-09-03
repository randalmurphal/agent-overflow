<script lang="ts">
  // First run on a phone: one screen, one button
  // (docs/specs/remote-access.md § "Pairing and remote-only").
  //
  // A phone never has a local backend, so there is nothing to show until
  // it knows where one is, and the answer to that is a QR code on the
  // owner's own desktop. What the camera reads is a pairing URL, the
  // same string a browser would have been navigated to, so this screen
  // reads it with the one reader that format has (`payloadFromLink`),
  // points the shell at the backend it names (`adoptPairingEndpoint`),
  // and hands the payload on. The existing `PairingScreen` runs the rest,
  // unchanged.
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { adoptPairingEndpoint } from '../../native/boot';
  import { scanPairingQr } from '../../native/qr';
  import { payloadFromLink } from '../../transport/backendAttach';
  import type { PairingPayload } from '../../transport/deviceSession';

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
      let payload: PairingPayload;
      try {
        payload = payloadFromLink(text);
      } catch (err) {
        problem = err instanceof Error ? err.message : String(err);
        return;
      }
      problem = adoptPairingEndpoint(payload);
      if (problem !== '') return;
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
        Open Agent Overflow on your computer, go to Settings and then Devices, and scan the code it shows.
      </p>
    </div>

    {#if scanning}
      <SteppedSpinner size={16} />
    {:else}
      <Button variant="primary" size="md" class="w-full" onclick={() => void scan()}>Scan code</Button>
    {/if}

    {#if problem}
      <p class="max-w-72 text-sm text-text-secondary" role="alert">{problem}</p>
    {/if}
  </div>
</div>
