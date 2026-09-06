<script lang="ts">
  // The tailnet half of Settings → Remote access: whether this backend joins the
  // owner's tailnet as its own node, and what that node currently is. Its
  // own component for the same reason NetworkDomainEditor is — the section
  // owns the load / save round trip, this owns a draft and a status.
  //
  // Nothing here waits on the node. Joining ends in a sign-in the owner
  // has to complete in a browser, so the save persists a preference and
  // the backend reconciles on its own goroutine; this renders whatever the
  // polled status says, including the link to open.

  import type { NetworkSettings } from '../../stores/bindings';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import {
    DANGER_BUTTON_CLASS,
    INPUT_CLASS,
    PRIMARY_BUTTON_CLASS,
    SECONDARY_BUTTON_CLASS,
  } from './styles';

  type TailnetDraft = {
    enabled: boolean;
    controlURL: string;
  };

  let {
    settings,
    busy,
    onsave,
    onforget,
  }: {
    settings: NetworkSettings;
    busy: boolean;
    onsave: (draft: TailnetDraft) => Promise<void>;
    onforget: () => Promise<void>;
  } = $props();

  function draftOf(value: NetworkSettings): TailnetDraft {
    return {
      enabled: value.tailnetEnabled ?? false,
      controlURL: value.tailnetControlUrl ?? '',
    };
  }

  function sameDraft(a: TailnetDraft, b: TailnetDraft): boolean {
    return a.enabled === b.enabled && a.controlURL === b.controlURL;
  }

  // Draft plus what the backend last said it stored, the pair
  // NetworkDomainEditor uses: re-seeding on the STORED values keeps the
  // status poll from wiping a half-typed control URL, since a poll answers
  // the same stored values and nothing moves.
  // svelte-ignore state_referenced_locally
  let draft = $state<TailnetDraft>(draftOf(settings));
  // svelte-ignore state_referenced_locally
  let seeded = $state<TailnetDraft>(draftOf(settings));

  $effect(() => {
    const next = draftOf(settings);
    if (sameDraft(next, seeded)) return;
    seeded = next;
    draft = next;
  });

  let dirty = $derived(!sameDraft(draft, seeded));
  let canSave = $derived(!busy && dirty);

  let tailnet = $derived(settings.tailnet);
  let enabled = $derived(settings.tailnetEnabled ?? false);

  let statusLine = $derived.by(() => {
    if (!enabled) {
      return tailnet.hasState
        ? 'Tailnet access is off. This backend keeps its place in your tailnet, so turning it back on reuses the same device.'
        : 'Tailnet access is off.';
    }
    if (tailnet.authUrl) {
      return 'Waiting for you to approve this machine.';
    }
    switch (tailnet.state) {
      case 'Running':
        return tailnet.https
          ? `On your tailnet as ${tailnet.dnsName}, over HTTPS.`
          : `On your tailnet as ${tailnet.dnsName || tailnet.ips.join(', ')}. Traffic is encrypted by the tailnet itself; turn HTTPS on in your tailnet's admin panel to also get a certificate for this name.`;
      case 'Starting':
        return 'Connecting to your tailnet.';
      case 'Stopped':
        return 'The node is stopped.';
      case '':
        return 'Starting up.';
      default:
        return `The node reports ${tailnet.state}.`;
    }
  });

  let copyState = $state<'idle' | 'copied' | 'failed'>('idle');
  let copyTimeout: ReturnType<typeof setTimeout> | null = null;

  async function copy(value: string): Promise<void> {
    if (!value) return;
    try {
      await navigator.clipboard.writeText(value);
      copyState = 'copied';
    } catch {
      copyState = 'failed';
    }
    if (copyTimeout) clearTimeout(copyTimeout);
    copyTimeout = setTimeout(() => {
      copyState = 'idle';
    }, 1500);
  }

  // Two-step forget, the same shape the device revocations use: the first
  // click arms and the second commits, so an identity is never deleted by
  // one stray press.
  let armed = $state(false);
  let armTimer: ReturnType<typeof setTimeout> | null = null;

  function forget(): void {
    if (busy) return;
    if (!armed) {
      armed = true;
      if (armTimer) clearTimeout(armTimer);
      armTimer = setTimeout(() => {
        armed = false;
      }, 4000);
      return;
    }
    if (armTimer) clearTimeout(armTimer);
    armed = false;
    void onforget();
  }

  function revert(): void {
    draft = draftOf(settings);
  }

  $effect(() => {
    return () => {
      if (copyTimeout) clearTimeout(copyTimeout);
      if (armTimer) clearTimeout(armTimer);
    };
  });
</script>

<section data-testid="network-tailnet-editor">
  <SettingsHeader title="Tailnet">
    {#snippet details()}
      Join this backend to your tailnet and it becomes one of your own devices,
      reachable from anywhere you are signed in. Nothing is opened to the public
      internet and no tunnel is involved.
    {/snippet}
  </SettingsHeader>

  <div class="flex flex-col gap-1">
    <SettingsField
      id="remote.tailnet"
      label="Join my tailnet"
      hint="The first time you turn this on, you approve this machine in your browser."
      align="start"
    >
      <ToggleSwitch
        checked={draft.enabled}
        disabled={busy}
        ariaLabel="Toggle tailnet access"
        onToggle={(next) => (draft.enabled = next)}
      />
    </SettingsField>

    <details>
      <summary class="cursor-pointer text-xs text-fg-muted">Custom coordination server</summary>
    <SettingsField
      id="remote.tailnet-control-url"
      label="Coordination server"
      hint="Leave blank for Tailscale. Set it only if you run your own coordination server."
      htmlFor="network-tailnet-control-url"
      stacked
    >
      <input
        id="network-tailnet-control-url"
        data-testid="network-tailnet-control-url"
        type="text"
        value={draft.controlURL}
        placeholder="https://headscale.example.com"
        autocomplete="off"
        spellcheck="false"
        disabled={busy}
        oninput={(e) => (draft.controlURL = (e.target as HTMLInputElement).value)}
        class="{INPUT_CLASS} max-w-[26rem] font-mono"
      />
    </SettingsField>
    </details>
  </div>

  <div class="mt-3 flex items-center gap-2">
    <button
      type="button"
      data-testid="network-tailnet-save"
      class={PRIMARY_BUTTON_CLASS}
      disabled={!canSave}
      onclick={() => void onsave(draft)}
    >
      {busy ? 'Saving…' : 'Save'}
    </button>
    <button
      type="button"
      data-testid="network-tailnet-revert"
      class={SECONDARY_BUTTON_CLASS}
      disabled={busy || !dirty}
      onclick={revert}
    >
      Revert
    </button>
    {#if !enabled && tailnet.hasState}
      <button
        type="button"
        data-testid="network-tailnet-forget"
        class={DANGER_BUTTON_CLASS}
        disabled={busy}
        onclick={forget}
      >
        {armed ? 'Confirm forget' : 'Forget this node'}
      </button>
    {/if}
  </div>

  <p class="mt-3 text-[0.71875rem] leading-snug text-fg-muted" data-testid="network-tailnet-status">
    {statusLine}
  </p>

  {#if tailnet.authUrl}
    <div class="mt-3 flex flex-col gap-2" data-testid="network-tailnet-auth">
      <p class="text-[0.71875rem] leading-snug text-fg-muted">
        Open this link and approve this machine in your tailnet.
      </p>
      <div class="flex items-center gap-2">
        <a
          href={tailnet.authUrl}
          target="_blank"
          rel="noopener noreferrer"
          class="{INPUT_CLASS} flex-1 min-w-0 truncate font-mono text-accent"
          data-testid="network-tailnet-auth-link"
        >
          {tailnet.authUrl}
        </a>
        <button
          type="button"
          data-testid="network-tailnet-auth-copy"
          class={SECONDARY_BUTTON_CLASS}
          onclick={() => void copy(tailnet.authUrl)}
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
    </div>
  {:else if tailnet.url}
    <div class="mt-3 flex items-center gap-2" data-testid="network-tailnet-url">
      <input
        type="text"
        readonly
        value={tailnet.url}
        aria-label="Tailnet URL"
        class="{INPUT_CLASS} flex-1 min-w-0 font-mono"
      />
      <button
        type="button"
        data-testid="network-tailnet-url-copy"
        class={SECONDARY_BUTTON_CLASS}
        onclick={() => void copy(tailnet.url)}
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
  {/if}

  {#if tailnet.lastError}
    <div class="mt-3" data-testid="network-tailnet-error">
      <SettingsCallout tone="error">
        {tailnet.lastError}
      </SettingsCallout>
    </div>
  {/if}
</section>
