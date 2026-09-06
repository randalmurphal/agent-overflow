<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { call, hasScope } = settingsComputer();

  import {
    GetNetworkSettings,
    SetNetworkSettings,
    RenewCanonicalDomainCert,
    ForgetTailnetNode,
    NetworkSettings,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { tokenizeCommandLine } from '../../utils/shellArgv';

  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import NetworkDomainEditor from './NetworkDomainEditor.svelte';
  import NetworkPortEditor from './NetworkPortEditor.svelte';
  import NetworkPreviewPorts from './NetworkPreviewPorts.svelte';
  import NetworkTailnetEditor from './NetworkTailnetEditor.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, SECONDARY_BUTTON_CLASS, SECTION_PROSE_CLASS } from './styles';

  // Two independent axes, resolved the way EditorSection resolves the same
  // pair — a surface that needs both asks both (transport/AGENTS.md).
  //
  // `access:admin` is authorization, and it is the ONE thing that decides
  // whether this section loads. Managing how a backend is exposed is what
  // a paired admin device is for: `GetNetworkSettings` answers that grant,
  // and its step-up-gated write answers a passkey assertion, so a phone the
  // owner paired with full access reads and edits this. A device without
  // the grant gets the arm below instead of a refusal — the load is
  // PASSIVE, fired because a pane mounted, so it has nobody to report a
  // refusal to (stores/AGENTS.md § A PASSIVE load asks before it fires;
  // the burst was found by the harness, 2026-08-31).
  //
  // `clientMode` is a process-boot fact and is NOT a load gate. In
  // `--connect` mode the SPA is attached to another backend, and managing
  // that backend's exposure from here is the point of the mode rather than
  // a mistake; all it changes is that the header says whose settings these
  // are.
  //
  // `host` is neither. It decides only what the BACKEND put in the payload:
  // this launch's token and both ticket-bearing share URLs are withheld
  // from a caller that is not at the machine, which `shareURLWithheld`
  // below reads off the answer rather than re-deriving.
  let noAdmin = $derived(!hasScope('access:admin'));
  let onHost = $derived(hasScope('host'));

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

  let loading = false;
  async function load(): Promise<void> {
    if (noAdmin || loading) return;
    loading = true;
    try {
      const result = await call(() => GetNetworkSettings());
      settings = result;
    } catch (err) {
      addToast('error', `Failed to load network settings: ${errString(err)}`);
    } finally {
      loading = false;
    }
  }

  // SetNetworkSettings writes the whole persisted record, so every caller
  // sends the fields it is not changing as well. Sending only the edited
  // one erased the rest: toggling the bind used to clear a configured
  // domain, which un-served its certificate on the next boot.
  function writeRequest(current: NetworkSettings, patch: Partial<NetworkSettings>): NetworkSettings {
    return new NetworkSettings({
      bindAll: current.bindAll,
      listenPort: current.listenPort,
      canonicalDomain: current.canonicalDomain,
      acmeDnsHook: current.acmeDnsHook,
      externalCertFile: current.externalCertFile,
      externalKeyFile: current.externalKeyFile,
      tailnetEnabled: current.tailnetEnabled,
      tailnetControlUrl: current.tailnetControlUrl,
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
      settings = await call(() => SetNetworkSettings(writeRequest(previous, { bindAll: next })));
    } catch (err) {
      settings = previous;
      addToast('error', `Failed to update network settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  // No optimistic update, unlike the bind toggle: a port change moves the
  // listener this window is talking to, and painting the new number before
  // the backend confirms would show an address nothing answers on if the
  // rebind failed. The RPC's own answer is the only value written here.
  async function savePort(port: number): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    const previous = settings;
    try {
      settings = await call(() => SetNetworkSettings(writeRequest(previous, { listenPort: port })));
    } catch (err) {
      addToast('error', `Failed to change the port: ${errString(err)}`);
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
      settings = await call(() => SetNetworkSettings(
        writeRequest(previous, {
          canonicalDomain: draft.canonicalDomain.trim(),
          acmeDnsHook: tokenizeCommandLine(draft.dnsHook),
          externalCertFile: draft.externalCertFile.trim(),
          externalKeyFile: draft.externalKeyFile.trim(),
        }),
      ));
    } catch (err) {
      addToast('error', `Failed to update network settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  async function saveTailnet(draft: { enabled: boolean; controlURL: string }): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    const previous = settings;
    try {
      settings = await call(() => SetNetworkSettings(
        writeRequest(previous, {
          tailnetEnabled: draft.enabled,
          tailnetControlUrl: draft.controlURL.trim(),
        }),
      ));
    } catch (err) {
      addToast('error', `Failed to update tailnet settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  async function forgetTailnetNode(): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    try {
      settings = await call(() => ForgetTailnetNode());
    } catch (err) {
      addToast('error', `Failed to forget this node: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  async function renewCertificate(): Promise<void> {
    if (!settings || saving) return;
    saving = true;
    try {
      settings = await call(() => RenewCanonicalDomainCert());
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

  // The tailnet has the same shape and no push channel either: joining
  // ends in a sign-in the owner completes in a browser, so the only way
  // this screen learns the node came up is to ask again. Bounded by the
  // pane being open — an enabled node that never joins stops being polled
  // the moment the user navigates away.
  let awaitingTailnet = $derived(
    (settings?.tailnetEnabled ?? false) && !(settings?.tailnet.running ?? false),
  );
  let polling = $derived(renewing || awaitingTailnet);

  // The backend withholds this launch's token and both ticket-bearing
  // share URLs from a caller that is not at the machine
  // (internal/network.FromServerRedacted). Read off what it ANSWERED
  // rather than re-derived here: a second copy of that rule is one that
  // can disagree with the payload it is describing. Host presence is in
  // the conjunction so the owner's own screen keeps the field it always
  // had, whatever a transport that is not serving yet answers with.
  let shareURLWithheld = $derived(
    !onHost && settings !== null && settings.url === '' && settings.token === '',
  );

  // Which of the three the URL is, read off the URL itself rather than
  // off the certificate status: which host and scheme it came out as is
  // the backend's decision (network.AppURLWithLAN), and describing it
  // from a second set of inputs is how the sentence and the field
  // disagree.
  const shareURLDescription = 'Open this address in a browser. New devices still need pairing approval.';

  $effect(() => {
    void load();
    // The owner can enable HTTPS or rename the node in Tailscale's
    // browser window without changing this node's Running state.
    const refocus = () => { if (!saving) void load(); };
    window.addEventListener('focus', refocus);
    return () => {
      window.removeEventListener('focus', refocus);
      if (copyTimeout) clearTimeout(copyTimeout);
    };
  });

  $effect(() => {
    if (!polling || noAdmin) return;
    pollTimer = setInterval(() => void load(), TLS_POLL_MS);
    return () => {
      if (pollTimer) clearInterval(pollTimer);
      pollTimer = null;
    };
  });
</script>

<div
  class="flex flex-col gap-4"
  data-testid={noAdmin ? 'network-section-local-only' : undefined}
>
  <section class="rounded-xl border border-border-subtle bg-surface-0 p-4">
    <SettingsHeader title="Local network" description={noAdmin ? 'This connection cannot change network access.' : 'Use the same Wi-Fi or wired network. Tailscale is optional at home.'} />
    {#if !noAdmin}
      <div class="flex flex-col gap-1">
        <SettingsField
          id="remote.allow-remote-access"
          label="Allow LAN connections"
          hint="Allow paired devices on your Wi-Fi or wired network."
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

  {#if !noAdmin && settings}
    <div class="rounded-xl border border-border-subtle bg-surface-0 p-4">
    <NetworkTailnetEditor
      {settings}
      busy={saving}
      onsave={saveTailnet}
      onforget={forgetTailnetNode}
    />
    </div>

    <details class="rounded-[var(--radius-field)] border border-border-subtle p-3">
      <summary class="cursor-pointer text-sm font-medium text-fg">Advanced network settings</summary>
      <div class="settings-sections mt-4">
        <NetworkPortEditor {settings} busy={saving} onsave={savePort} />
    <NetworkDomainEditor
      {settings}
      busy={saving}
      onsave={saveDomain}
      onrenew={renewCertificate}
    />

    <!--
      Under the tailnet block because that is where a machine gets the
      address these ports are served on, and inside the `access:admin` arm
      with everything else that changes how this backend is exposed.
    -->
    <NetworkPreviewPorts />

    <section data-settings-field="remote.share-url" data-settings-label="Share URL">
      {#if shareURLWithheld}
        <!-- No input and no Copy button: there is nothing to copy, and a
             dead control is worse than a sentence saying where the thing
             is. The warning below lives inside the other branch for the
             same reason — it describes a URL, so it cannot render where
             there is none. -->
        <SettingsHeader title="Share URL" />
        <p class={SECTION_PROSE_CLASS} data-testid="share-url-host-only">
          The share URL and this launch's connection token are only shown at the
          computer running Agent Overflow.
        </p>
      {:else}
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
      {/if}
    </section>
      </div>
    </details>
  {/if}
</div>
