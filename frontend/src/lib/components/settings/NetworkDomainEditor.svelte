<script lang="ts">
  // The domain half of Settings -> Network: the name this backend answers
  // to, and where its certificate comes from. Its own component because
  // NetworkSection owns the load / save round trip and this owns a draft
  // with four fields, one of which is an argv line.
  //
  // The certificate is never obtained by the save: a DNS-01 exchange
  // takes minutes, so the backend reconciles on its own goroutine and
  // this polls the status while it says it is working.

  import type { NetworkSettings } from '../../stores/bindings';
  import { formatCalendarDate } from '../../utils/format';
  import { tokenizeCommandLine, formatArgv, CommandLineError } from '../../utils/shellArgv';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  type DomainDraft = {
    canonicalDomain: string;
    dnsHook: string;
    externalCertFile: string;
    externalKeyFile: string;
  };

  let {
    settings,
    busy,
    onsave,
    onrenew,
  }: {
    settings: NetworkSettings;
    busy: boolean;
    onsave: (draft: DomainDraft) => Promise<void>;
    onrenew: () => Promise<void>;
  } = $props();

  function draftOf(value: NetworkSettings): DomainDraft {
    return {
      canonicalDomain: value.canonicalDomain ?? '',
      dnsHook: formatArgv(value.acmeDnsHook ?? []),
      externalCertFile: value.externalCertFile ?? '',
      externalKeyFile: value.externalKeyFile ?? '',
    };
  }

  // Field by field rather than a joined signature: three of these four
  // values are user text that could contain any separator worth picking.
  function sameDraft(a: DomainDraft, b: DomainDraft): boolean {
    return (
      a.canonicalDomain === b.canonicalDomain &&
      a.dnsHook === b.dnsHook &&
      a.externalCertFile === b.externalCertFile &&
      a.externalKeyFile === b.externalKeyFile
    );
  }

  // Seeded once from the loaded settings and re-seeded by the effect
  // below when the STORED values move, the same shape DiscussionEditor
  // uses; capturing the initial value here is the point.
  // svelte-ignore state_referenced_locally
  let draft = $state<DomainDraft>(draftOf(settings));
  // What the backend last told us it stored. Re-seeding on THAT rather
  // than on every settings object keeps the status poll from wiping a
  // half-typed domain: a poll returns the same stored values, so nothing
  // moves.
  // svelte-ignore state_referenced_locally
  let seeded = $state<DomainDraft>(draftOf(settings));

  $effect(() => {
    const next = draftOf(settings);
    if (sameDraft(next, seeded)) return;
    seeded = next;
    draft = next;
  });

  let hookError = $derived.by(() => {
    if (draft.dnsHook.trim() === '') return null;
    try {
      return tokenizeCommandLine(draft.dnsHook).length === 0 ? 'Enter a command.' : null;
    } catch (err) {
      return err instanceof CommandLineError ? err.message : 'This command cannot be read.';
    }
  });
  let dirty = $derived(!sameDraft(draft, seeded));
  let canSave = $derived(!busy && dirty && hookError === null);

  let tls = $derived(settings.tls);
  let expiry = $derived(tls.notAfter > 0 ? formatCalendarDate(tls.notAfter) : '');
  let statusLine = $derived.by(() => {
    if (tls.renewing) {
      return 'Obtaining a certificate. This can take a few minutes while the DNS record spreads.';
    }
    switch (tls.serving) {
      case 'acme':
        return `Serving a Let's Encrypt certificate for ${settings.canonicalDomain}, valid until ${expiry}. Agent Overflow renews it 30 days before that.`;
      case 'external':
        return `Serving the certificate file you configured for ${settings.canonicalDomain}, valid until ${expiry}. Renewing it stays with whatever wrote it, and a new file is picked up within a day.`;
      case 'self-signed':
        return "Serving this install's own certificate. Paired apps pin it, and browsers do not accept it.";
      default:
        return 'No certificate is loaded, so this backend answers in the clear.';
    }
  });
  let canRenew = $derived(!busy && !tls.renewing && settings.canonicalDomain !== '');

  function revert(): void {
    draft = draftOf(settings);
  }
</script>

<section data-testid="network-domain-editor">
  <SettingsHeader title="Domain and HTTPS">
    {#snippet details()}
      Give this backend a domain name and it answers to that name. Point the
      domain at this machine in DNS, then either let Agent Overflow obtain a
      certificate from Let's Encrypt, or point it at one you already have.
      Without either, the name still works and the connection is encrypted only
      by whatever sits in front.
    {/snippet}
  </SettingsHeader>

  <div class="flex flex-col gap-1">
    <SettingsField
      id="remote.domain"
      label="Domain"
      hint="A bare hostname such as ao.example.com. No scheme, no port, no path. Leave it blank to reach this backend by address only."
      htmlFor="network-canonical-domain"
      stacked
    >
      <input
        id="network-canonical-domain"
        data-testid="network-canonical-domain"
        type="text"
        value={draft.canonicalDomain}
        placeholder="ao.example.com"
        autocomplete="off"
        spellcheck="false"
        disabled={busy}
        oninput={(e) => (draft.canonicalDomain = (e.target as HTMLInputElement).value)}
        class="{INPUT_CLASS} max-w-[22rem] font-mono"
      />
    </SettingsField>

    <SettingsField
      id="remote.dns-hook"
      label="DNS update command"
      hint="Publishes the record Let's Encrypt checks. Agent Overflow runs it with set or clear, the record name, and the value appended. There is no shell, so write sh -c '…' if you need one. Leave it blank if you are not using Let's Encrypt."
      htmlFor="network-dns-hook"
      stacked
    >
      <input
        id="network-dns-hook"
        data-testid="network-dns-hook"
        type="text"
        value={draft.dnsHook}
        placeholder="my-dns-tool --zone example.com"
        autocomplete="off"
        spellcheck="false"
        disabled={busy}
        aria-invalid={hookError !== null}
        oninput={(e) => (draft.dnsHook = (e.target as HTMLInputElement).value)}
        class="{INPUT_CLASS} font-mono"
      />
      {#if hookError}
        <p class="mt-1 text-[0.71875rem] text-error" role="alert" data-testid="network-dns-hook-error">
          {hookError}
        </p>
      {/if}
    </SettingsField>

    <SettingsField
      id="remote.certificate-file"
      label="Certificate file"
      hint="Absolute path to a PEM certificate you already have. Fill this and the key to serve them instead of obtaining a certificate."
      htmlFor="network-external-cert"
      stacked
    >
      <input
        id="network-external-cert"
        data-testid="network-external-cert"
        type="text"
        value={draft.externalCertFile}
        placeholder="/etc/ssl/ao/fullchain.pem"
        autocomplete="off"
        spellcheck="false"
        disabled={busy}
        oninput={(e) => (draft.externalCertFile = (e.target as HTMLInputElement).value)}
        class="{INPUT_CLASS} font-mono"
      />
    </SettingsField>

    <SettingsField
      id="remote.private-key-file"
      label="Private key file"
      hint="Absolute path to the matching PEM private key."
      htmlFor="network-external-key"
      stacked
    >
      <input
        id="network-external-key"
        data-testid="network-external-key"
        type="text"
        value={draft.externalKeyFile}
        placeholder="/etc/ssl/ao/privkey.pem"
        autocomplete="off"
        spellcheck="false"
        disabled={busy}
        oninput={(e) => (draft.externalKeyFile = (e.target as HTMLInputElement).value)}
        class="{INPUT_CLASS} font-mono"
      />
    </SettingsField>
  </div>

  <div class="mt-3 flex items-center gap-2">
    <button
      type="button"
      data-testid="network-domain-save"
      class={PRIMARY_BUTTON_CLASS}
      disabled={!canSave}
      onclick={() => void onsave(draft)}
    >
      {busy ? 'Saving…' : 'Save'}
    </button>
    <button
      type="button"
      data-testid="network-domain-revert"
      class={SECONDARY_BUTTON_CLASS}
      disabled={busy || !dirty}
      onclick={revert}
    >
      Revert
    </button>
    <button
      type="button"
      data-testid="network-domain-renew"
      class={SECONDARY_BUTTON_CLASS}
      disabled={!canRenew}
      onclick={() => void onrenew()}
    >
      {tls.renewing ? 'Working…' : 'Check certificate now'}
    </button>
  </div>

  <p class="mt-3 text-[0.71875rem] leading-snug text-fg-muted" data-testid="network-tls-status">
    {statusLine}
  </p>
  {#if tls.selfSignedFingerprint}
    <p class="mt-1 font-mono text-[0.6875rem] leading-snug text-fg-hint break-all">
      {tls.selfSignedFingerprint}
    </p>
  {/if}

  {#if tls.lastError}
    <div class="mt-3" data-testid="network-tls-error">
      <SettingsCallout tone="error">
        The last certificate attempt failed: {tls.lastError}
      </SettingsCallout>
    </div>
  {/if}
</section>
