<script lang="ts">
  import {
    GetNetworkSettings,
    SetNetworkSettings,
    RenewCanonicalDomainCert,
    NetworkSettings,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { tokenizeCommandLine } from '../../utils/shellArgv';
  import { isClientMode } from '../../transport/runMode';
  import { hasScope } from '../../transport/scopes';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import NetworkDomainEditor from './NetworkDomainEditor.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  // Two independent axes, resolved the way EditorSection resolves the same
  // pair — a surface that needs both asks both (transport/AGENTS.md).
  //
  // `clientMode` is a process-boot fact: in `--connect` mode the SPA is
  // attached to a remote backend, so GetNetworkSettings /
  // SetNetworkSettings would query and mutate the *remote* server's bind
  // preference, which both isn't actionable from here (the user can't
  // restart the remote process to apply the rebind) and is misleading —
  // the URL printed below would point at the remote machine, not the local
  // one.
  //
  // `host` is authorization, and it is the one no session is ever granted:
  // the bind preference is a fact about THE MACHINE, so `GetNetworkSettings`
  // carries `//ao:scope host` and is refused for every paired device,
  // view-only and full alike. Without this arm the load fired on mount and
  // the refusal came back as `Failed to load network settings` — a passive
  // load reporting to nobody, which is exactly the burst the view-only
  // rule exists to prevent (stores/AGENTS.md § A PASSIVE load asks before
  // it fires; found by the harness, 2026-08-31).
  const clientMode = isClientMode();
  let noHost = $derived(!hasScope('host'));
  let localOnly = $derived(clientMode || noHost);

  let settings = $state<NetworkSettings | null>(null);
  let saving = $state(false);
  let copyState = $state<'idle' | 'copied' | 'failed'>('idle');
  let copyTimeout: ReturnType<typeof setTimeout> | null = null;

  // How often the screen re-reads while the backend says it is working
  // on a certificate. There is no push channel for this: a DNS-01
  // exchange is a minutes-long, once-in-a-while event, so one read every
  // few seconds costs less than a channel that would idle forever.
  const TLS_POLL_MS = 3000;
  let pollTimer: ReturnType<typeof setInterval> | null = null;

  async function load(): Promise<void> {
    if (localOnly) return;
    try {
      const result = await GetNetworkSettings();
      settings = result;
    } catch (err) {
      addToast('error', `Failed to load network settings: ${errString(err)}`);
    }
  }

  // SetNetworkSettings writes the whole persisted record, so every caller
  // sends the fields it is not changing as well. Sending only the edited
  // one erased the rest: toggling the bind used to clear a configured
  // domain, which un-served its certificate on the next boot.
  function writeRequest(current: NetworkSettings, patch: Partial<NetworkSettings>): NetworkSettings {
    return new NetworkSettings({
      bindAll: current.bindAll,
      canonicalDomain: current.canonicalDomain,
      acmeDnsHook: current.acmeDnsHook,
      externalCertFile: current.externalCertFile,
      externalKeyFile: current.externalKeyFile,
      url: '',
      token: '',
      ...patch,
    });
  }

  async function toggleBindAll(next: boolean): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    const previous = settings;
    // Optimistic update so the toggle visually moves immediately while
    // the rebind round-trips. Any error path restores `previous`.
    settings = { ...settings, bindAll: next } as NetworkSettings;
    try {
      settings = await SetNetworkSettings(writeRequest(previous, { bindAll: next }));
    } catch (err) {
      settings = previous;
      addToast('error', `Failed to update network settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  async function saveDomain(draft: {
    canonicalDomain: string;
    dnsHook: string;
    externalCertFile: string;
    externalKeyFile: string;
  }): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    const previous = settings;
    try {
      settings = await SetNetworkSettings(
        writeRequest(previous, {
          canonicalDomain: draft.canonicalDomain.trim(),
          acmeDnsHook: tokenizeCommandLine(draft.dnsHook),
          externalCertFile: draft.externalCertFile.trim(),
          externalKeyFile: draft.externalKeyFile.trim(),
        }),
      );
    } catch (err) {
      addToast('error', `Failed to update network settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  async function renewCertificate(): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    try {
      settings = await RenewCanonicalDomainCert();
    } catch (err) {
      addToast('error', `Failed to check the certificate: ${errString(err)}`);
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

  // Derived, not read off `settings` inside the poll effect: a re-read
  // that answers the same thing must not restart the timer, or the
  // interval never elapses and the screen re-reads as fast as the RPC
  // returns.
  let renewing = $derived(settings?.tls.renewing ?? false);

  // Which of the three the URL is, read off the URL itself rather than
  // off the certificate status: which host and scheme it came out as is
  // the backend's decision (network.AppURLWithLAN), and describing it
  // from a second set of inputs is how the sentence and the field
  // disagree.
  let shareURLDescription = $derived.by(() => {
    const ticket =
      ' Each copy carries a one-time ticket that only loads the page: the device still has to pair.';
    const url = settings?.url ?? '';
    if (url.startsWith('https://')) {
      return (
        'Copy this URL into a browser on another device. It uses the domain you configured, so the browser opens it over HTTPS with no warning.' +
        ticket
      );
    }
    if (settings?.bindAll) {
      return (
        "Copy this URL into a browser on another device to open Agent Overflow remotely. With LAN binding on, the URL points at this machine's private IP." +
        ticket
      );
    }
    return (
      'Copy this URL into a browser on another device. While the server is on loopback, only this machine resolves the URL.' +
      ticket
    );
  });

  $effect(() => {
    void load();
    return () => {
      if (copyTimeout) clearTimeout(copyTimeout);
    };
  });

  $effect(() => {
    if (!renewing || localOnly) return;
    pollTimer = setInterval(() => void load(), TLS_POLL_MS);
    return () => {
      if (pollTimer) clearInterval(pollTimer);
      pollTimer = null;
    };
  });
</script>

<div
  class="flex flex-col gap-6"
  data-testid={localOnly ? 'network-section-local-only' : undefined}
>
  <section>
    <SettingsHeader title="Network Binding">
      {#snippet details()}
        {#if clientMode}
          Network binding can only be edited from your local install. This window is
          attached to a remote backend, so changes here would update the remote
          machine's bind preference, not yours.
        {:else if noHost}
          Network binding belongs to the machine running Agent Overflow. This device
          reached it over the network, so the bind preference and the share URL are
          shown and changed there.
        {:else}
          By default the server binds to
          <code class="font-mono text-[0.6875rem]">127.0.0.1</code> so only this machine can
          reach it. Toggle on to listen on every network interface — other devices on
          your LAN can then open the URL below in a browser. Without a certificate the
          traffic is plain <code class="font-mono text-[0.6875rem]">ws://</code>; give
          this backend a domain below, or route through Tailscale Serve or an SSH
          tunnel, before using it beyond a trusted LAN.
        {/if}
      {/snippet}
    </SettingsHeader>
    {#if !localOnly}
      <div class="flex flex-col gap-1">
        <SettingsField
          label="Allow remote access"
          hint="Listen on all interfaces (0.0.0.0) so devices on your network can connect. Toggling off stops new LAN connections; a device already connected stays until it closes — revoke it under Devices to cut it now."
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

  {#if !localOnly && settings}
    <NetworkDomainEditor
      {settings}
      busy={saving}
      onsave={saveDomain}
      onrenew={renewCertificate}
    />

    <section>
      <SettingsHeader title="Share URL" description={shareURLDescription} />

      <div class="flex items-center gap-2">
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
            The URL above is plaintext over LAN — the ticket on it, and everything
            the device sends once it has paired, travel in the clear where any
            device on this network can read them. Front the bind with Tailscale
            Serve, an SSH tunnel, or a reverse proxy with TLS before sharing on an
            untrusted network.
          </SettingsCallout>
        </div>
      {/if}
    </section>
  {/if}
</div>
