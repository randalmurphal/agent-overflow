<script lang="ts">
  // First run on a phone: scan a code or paste the same invitation.
  // (docs/specs/remote-access.md § "Pairing and remote-only").
  //
  // Both entry choices validate the same invitation with payloadFromLink,
  // adopt its endpoint once, and hand off to the existing PairingScreen.
  import Button from '../primitives/Button.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { adoptPairingEndpoint } from '../../native/boot';
  import { scanPairingQr } from '../../native/qr';
  import { payloadFromLink } from '../../transport/backendAttach';
  import type { PairingPayload } from '../../transport/deviceSession';
  import { INPUT_CLASS } from '../settings/styles';
  import { errString } from '../../utils/errors';

  interface Props {
    /** Called with the scanned payload once the endpoint is set. */
    onScanned: (payload: PairingPayload) => void;
  }

  let { onScanned }: Props = $props();

  let scanning = $state(false);
  let problem = $state('');
  let enteringLink = $state(false);
  let link = $state('');

  function connect(text: string): void {
    problem = '';
    try {
      const payload = payloadFromLink(text.trim());
      problem = adoptPairingEndpoint(payload);
      if (problem === '') onScanned(payload);
    } catch (err) {
      problem = errString(err);
    }
  }

  async function scan(): Promise<void> {
    if (scanning) return;
    scanning = true;
    problem = '';
    try {
      const text = await scanPairingQr();
      // Null is a cancelled scan, which is not a failure and gets no
      // message: the person is back where they were, on purpose.
      if (text === null) return;
      connect(text);
    } catch (err) {
      problem = errString(err);
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
        Open Computers in Agent Overflow on your computer to create an invitation. Scan its code or paste the link.
      </p>
    </div>

    {#if scanning}
      <SteppedSpinner size={16} />
    {:else}
      <Button variant="primary" size="md" class="w-full" onclick={() => void scan()}>Scan code</Button>
    {/if}

    {#if enteringLink}
      <form class="flex w-full flex-col gap-3" onsubmit={(event) => { event.preventDefault(); connect(link); }}>
        <input
          type="text"
          inputmode="url"
          enterkeyhint="go"
          class="{INPUT_CLASS} w-full"
          aria-label="Pairing link"
          placeholder="Paste invitation link"
          autocomplete="off"
          autocapitalize="off"
          spellcheck={false}
          bind:value={link}
          disabled={scanning}
        />
        <Button type="submit" variant="secondary" size="md" disabled={scanning || !link.trim()}>Connect</Button>
      </form>
    {:else}
      <Button variant="ghost" size="sm" disabled={scanning} onclick={() => { enteringLink = true; }}>Use a link</Button>
    {/if}

    {#if problem}
      <p class="max-w-72 text-sm text-text-secondary" role="alert">{problem}</p>
    {/if}
  </div>
</div>
