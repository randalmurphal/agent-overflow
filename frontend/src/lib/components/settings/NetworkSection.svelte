<script lang="ts">
  import {
    GetNetworkSettings,
    SetNetworkSettings,
    NetworkSettings,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { isClientMode } from '../../transport/runMode';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  // In `--connect` mode the SPA is attached to a remote backend.
  // GetNetworkSettings / SetNetworkSettings would query and mutate the
  // *remote* server's bind preference, which both isn't actionable from
  // here (the user can't restart the remote process to apply the rebind)
  // and is misleading — the URL printed below would point at the remote
  // machine, not the local one. Render a placeholder instead.
  const clientMode = isClientMode();

  let settings = $state<NetworkSettings | null>(null);
  let saving = $state(false);
  let copyState = $state<'idle' | 'copied' | 'failed'>('idle');
  let copyTimeout: ReturnType<typeof setTimeout> | null = null;

  async function load(): Promise<void> {
    if (clientMode) return;
    try {
      const result = await GetNetworkSettings();
      settings = result;
    } catch (err) {
      addToast('error', `Failed to load network settings: ${errString(err)}`);
    }
  }

  async function toggleBindAll(next: boolean): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    const previous = settings;
    // Optimistic update so the toggle visually moves immediately while
    // the rebind round-trips. Any error path restores `previous`.
    settings = { ...settings, bindAll: next } as NetworkSettings;
    try {
      const updated = await SetNetworkSettings(
        new NetworkSettings({ bindAll: next, url: '', token: '' }),
      );
      settings = updated;
    } catch (err) {
      settings = previous;
      addToast('error', `Failed to update network settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  async function copyURL(): Promise<void> {
    if (!settings?.url) return;
    try {
      await navigator.clipboard.writeText(settings.url);
      copyState = 'copied';
    } catch {
      copyState = 'failed';
    }
    if (copyTimeout) clearTimeout(copyTimeout);
    copyTimeout = setTimeout(() => {
      copyState = 'idle';
    }, 1500);
  }

  $effect(() => {
    void load();
    return () => {
      if (copyTimeout) clearTimeout(copyTimeout);
    };
  });
</script>

<div class="flex flex-col gap-10">
  <section>
    <SettingsHeader
      eyebrow="Remote access"
      title="Network Binding"
    />
    {#if clientMode}
      <p class="mt-1 max-w-2xl text-[12px] leading-relaxed text-fg-muted">
        Network binding can only be edited from your local install. This window is
        attached to a remote backend, so changes here would update the remote
        machine's bind preference, not yours.
      </p>
    {:else}
      <p class="mt-1 max-w-2xl text-[12px] leading-relaxed text-fg-muted">
        By default the server binds to
        <code class="font-mono text-[11px]">127.0.0.1</code> so only this machine can
        reach it. Toggle on to listen on every network interface — other devices on
        your LAN can then open the URL below in a browser. The traffic is plain
        <code class="font-mono text-[11px]">ws://</code>; for exposure beyond a
        trusted LAN, route through Tailscale Serve or an SSH tunnel.
      </p>

      <div class="mt-4 flex flex-col gap-1">
        <SettingsField
          label="Allow remote access"
          hint="Listen on all interfaces (0.0.0.0) so devices on your network can connect. Toggling off stops new LAN connections but does not invalidate the token — restart the app to rotate it."
          align="start"
        >
          <ToggleSwitch
            checked={settings?.bindAll ?? false}
            disabled={!settings || saving}
            ariaLabel="Toggle remote access"
            onToggle={toggleBindAll}
          />
        </SettingsField>
      </div>
    {/if}
  </section>

  {#if !clientMode && settings}
    <section>
      <SettingsHeader
        eyebrow="Connection"
        title="Share URL"
        description={settings.bindAll
          ? "Copy this URL into a browser on another device to open Agent Overflow remotely. With LAN binding on, the URL points at this machine's private IP."
          : 'Copy this URL into a browser on another device. While the server is on loopback, only this machine resolves the URL.'}
      />

      <div class="mt-4 flex items-center gap-2">
        <input
          type="text"
          readonly
          value={settings.url}
          aria-label="Application URL"
          class="{INPUT_CLASS} flex-1 min-w-0 font-mono"
        />
        <button
          type="button"
          onclick={copyURL}
          disabled={!settings.url}
          class={SECONDARY_BUTTON_CLASS}
        >
          {#if copyState === 'copied'}
            Copied
          {:else if copyState === 'failed'}
            Copy failed
          {:else}
            Copy
          {/if}
        </button>
      </div>

      {#if settings.insecure}
        <div class="mt-3" data-testid="insecure-url-warning">
          <SettingsCallout tone="warn">
            The URL above is plaintext over LAN — the token in the query string
            travels in the clear and any device on this network can read it. Front
            the bind with Tailscale Serve, an SSH tunnel, or a reverse proxy with
            TLS before sharing on an untrusted network.
          </SettingsCallout>
        </div>
      {/if}
    </section>
  {/if}
</div>
