<script lang="ts">
  import { GetNetworkSettings, SetNetworkSettings, NetworkSettings } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { isClientMode } from '../../transport/runMode';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';

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

<div class="flex flex-col gap-8">
  <section>
    <MicroLabel as="p">Remote Access</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Network Binding</h3>
    {#if clientMode}
      <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
        Network binding can only be edited from your local install. This window is attached to a
        remote backend, so changes here would update the remote machine's bind preference, not
        yours.
      </p>
    {:else}
      <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
        By default the server binds to <code class="font-mono text-[11px]">127.0.0.1</code> so only this
        machine can reach it. Toggle on to listen on every network interface — other devices on your
        LAN can then open the URL below in a browser. The traffic is plain <code class="font-mono text-[11px]">ws://</code>;
        for exposure beyond a trusted LAN, route through Tailscale Serve or an SSH tunnel.
      </p>

      <div class="mt-3 divide-y divide-border-subtle">
        <div class="flex items-center justify-between gap-4 py-2.5">
          <div>
            <p class="text-[13px] text-fg font-medium">Allow remote access</p>
            <p class="text-[12px] text-fg-muted">
              Listen on all interfaces (<code class="font-mono text-[11px]">0.0.0.0</code>) so devices on your network can connect.
            </p>
            <p class="mt-1 text-[11px] text-fg-muted">
              Toggling off stops new LAN connections but does <em>not</em> invalidate the token. Restart the app to rotate the token.
            </p>
          </div>
          <ToggleSwitch
            checked={settings?.bindAll ?? false}
            disabled={!settings || saving}
            ariaLabel="Toggle remote access"
            onToggle={toggleBindAll}
          />
        </div>
      </div>
    {/if}
  </section>

  {#if !clientMode && settings}
    <section>
      <MicroLabel as="p">Connection</MicroLabel>
      <h3 class="mt-1 text-[15px] font-semibold text-fg">Share URL</h3>
      <p class="mt-1 text-[12px] text-fg-muted">
        Copy this URL into a browser on another device to open Agent Overflow remotely.
        {#if settings.bindAll}
          When LAN binding is on, the URL points at this machine's private IP.
        {:else}
          When the server is on loopback, only this machine resolves the URL.
        {/if}
      </p>

      <div class="mt-3 flex items-center gap-2">
        <input
          type="text"
          readonly
          value={settings.url}
          aria-label="Application URL"
          class="flex-1 min-w-0 text-[12px] font-mono rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-1.5 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40"
        />
        <button
          type="button"
          onclick={copyURL}
          disabled={!settings.url}
          class="text-[12px] font-medium rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-3 py-1.5 text-fg hover:border-accent/40 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
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
        <p
          role="alert"
          data-testid="insecure-url-warning"
          class="mt-3 rounded-[var(--radius-field)] border border-warning/40 bg-warning/10 px-3 py-2 text-[12px] text-fg"
        >
          The URL above is plaintext over LAN — the token in the query string travels in the
          clear and any device on this network can read it. Front the bind with Tailscale Serve,
          an SSH tunnel, or a reverse proxy with TLS before sharing on an untrusted network.
        </p>
      {/if}
    </section>
  {/if}
</div>
