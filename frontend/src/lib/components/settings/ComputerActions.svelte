<script lang="ts">
  import type { BackendKey } from '../../transport/backendKey';
  import { hasScope } from '../../transport/scopes';
  import Button from '../primitives/Button.svelte';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { backendReachable } from '../../stores/attachedBackends.svelte';
  import { computerSSH } from '../../stores/computerSSH.svelte';
  import { StartSSHComputer } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import ComputerTransfers from '../transfers/ComputerTransfers.svelte';
  import ComputerAddress from './ComputerAddress.svelte';
  import { isNativeShell } from '../../native/platform';
  let { backend }: { backend: BackendKey } = $props();
  let starting = $state(false);
  let profile = $derived(computerSSH(backend));
  async function start(): Promise<void> {
    if (!profile || starting) return;
    starting = true;
    try {
      await StartSSHComputer({ ...profile, startService: true, lan: false });
      addToast('success', 'Service started. Reconnecting when it is ready.');
    } catch (err) { addToast('error', errString(err)); }
    finally { starting = false; }
  }
</script>

<div class="mt-2 flex flex-wrap gap-2">
  {#if backend !== HOME_BACKEND && hasScope('host') && !backendReachable(backend) && profile}
    <Button variant="primary" size="xs" disabled={starting} onclick={() => void start()}>{starting ? 'Starting…' : 'Start over SSH'}</Button>
  {/if}
  {#if !backendReachable(backend) && (isNativeShell() || (backend !== HOME_BACKEND && hasScope('host')))}
    <ComputerAddress {backend} />
  {/if}
</div>
<ComputerTransfers {backend} />
