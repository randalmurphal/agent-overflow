<script lang="ts">
  import { browserLock, enableBrowserLock, disableBrowserLock } from '../../stores/browserLock.svelte';
  import { grantedScopes } from '../../transport/scopes';
  import { passkeysUsable } from '../../transport/passkey';
  import { isNativeShell } from '../../native/platform';
  import SettingsHeader from './SettingsHeader.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';

  const native = isNativeShell();
  const remoteBrowser = $derived(!native && grantedScopes().source === 'paired-session');
  const usable = $derived(remoteBrowser && passkeysUsable());
</script>

{#if remoteBrowser}
  <section class="rounded-xl border border-border-subtle bg-surface-0 p-4" data-testid="browser-lock-settings">
    <SettingsHeader title="Browser lock" description="Protect this browser with a passkey. Each tab unlocks separately." />
    <SettingsField id="remote.browser-lock" label="Require a passkey to open" hint="Lock on opening a tab and after five minutes in the background. Applies to this browser at this address." align="start">
      <ToggleSwitch checked={browserLock.enabled} disabled={browserLock.busy || (!browserLock.enabled && !usable)}
        ariaLabel="Require a passkey to open" onToggle={(enabled) => enabled ? void enableBrowserLock() : disableBrowserLock()} />
    </SettingsField>
    <p class="mt-2 text-xs text-fg-muted">
      {#if usable}
        Enabling verifies an existing passkey first. To set up your first passkey, use Security &amp; passkeys on the host at its configured HTTPS address.
      {:else}
        Open this computer at its configured HTTPS passkey domain. Register a passkey from the host before enabling the lock.
      {/if}
    </p>
    {#if browserLock.error}<SettingsCallout tone="error">{browserLock.error}</SettingsCallout>{/if}
  </section>
{/if}
