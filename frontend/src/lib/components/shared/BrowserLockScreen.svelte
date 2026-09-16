<script lang="ts">
  import LockScreen from './LockScreen.svelte';
  import { browserLock, setBrowserLockLocalPage, unlockBrowser } from '../../stores/browserLock.svelte';
  import { grantedScopes } from '../../transport/scopes';

  $effect(() => setBrowserLockLocalPage(grantedScopes().source === 'local-page'));
</script>

{#if browserLock.locked}
  <LockScreen
    onUnlock={() => void unlockBrowser()}
    description="Use your passkey to open Agent Overflow."
    busy={browserLock.busy}
    error={browserLock.error}
    help="If your session expired, open a new pairing link from your computer. If your passkey was removed, register a replacement from the host."
  />
{/if}
