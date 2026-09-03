<script lang="ts">
  // The port half of the bind block: which port this backend listens on.
  // Its own component for the same reason NetworkDomainEditor is one —
  // NetworkSection owns the load / save round trip, and a typed field
  // needs a draft that a status poll must not overwrite mid-edit.

  import type { NetworkSettings } from '../../stores/bindings';
  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  const MAX_PORT = 65535;

  let {
    settings,
    busy,
    onsave,
  }: {
    settings: NetworkSettings;
    busy: boolean;
    onsave: (port: number) => Promise<void>;
  } = $props();

  // Zero is "automatic" on the wire and an empty field on the screen. A
  // literal 0 in the box would read as a port somebody chose.
  function draftOf(value: NetworkSettings): string {
    return value.listenPort > 0 ? String(value.listenPort) : '';
  }

  // Seeded once and re-seeded when the STORED value moves, the same shape
  // NetworkDomainEditor uses: a certificate poll returns the same stored
  // port, so nothing moves and a half-typed number survives.
  // svelte-ignore state_referenced_locally
  let draft = $state(draftOf(settings));
  // svelte-ignore state_referenced_locally
  let seeded = $state(draftOf(settings));

  $effect(() => {
    const next = draftOf(settings);
    if (next === seeded) return;
    seeded = next;
    draft = next;
  });

  let trimmed = $derived(draft.trim());
  let parsed = $derived(trimmed === '' ? 0 : Number(trimmed));
  let error = $derived.by(() => {
    if (trimmed === '') return null;
    if (!/^\d+$/.test(trimmed)) return 'Enter a number.';
    if (parsed < 1 || parsed > MAX_PORT) return `Enter a port between 1 and ${MAX_PORT}.`;
    return null;
  });
  let dirty = $derived(trimmed !== seeded);
  let canSave = $derived(!busy && dirty && error === null);

  function revert(): void {
    draft = draftOf(settings);
  }
</script>

<div class="flex flex-col gap-1" data-testid="network-port-editor">
  <SettingsField
    id="remote.port"
    label="Port"
    hint="The port this backend listens on. Leave it blank to let Agent Overflow pick one and keep reusing it."
    htmlFor="network-listen-port"
    stacked
  >
    <input
      id="network-listen-port"
      data-testid="network-listen-port"
      type="text"
      inputmode="numeric"
      value={draft}
      placeholder="Automatic"
      autocomplete="off"
      spellcheck="false"
      disabled={busy}
      aria-invalid={error !== null}
      oninput={(e) => (draft = (e.target as HTMLInputElement).value)}
      class="{INPUT_CLASS} max-w-[10rem] font-mono"
    />
    {#if error}
      <p class="mt-1 text-[0.71875rem] text-error" role="alert" data-testid="network-listen-port-error">
        {error}
      </p>
    {/if}
    <p class="mt-1 text-[0.71875rem] leading-snug text-fg-muted">
      Changing it changes the share URL and every pairing link. A browser has to sign
      in again and loses what it saved for the old address. A paired app keeps trying
      the old port until you pair it again.
    </p>
  </SettingsField>

  <div class="mt-2 flex items-center gap-2">
    <button
      type="button"
      data-testid="network-port-save"
      class={PRIMARY_BUTTON_CLASS}
      disabled={!canSave}
      onclick={() => void onsave(parsed)}
    >
      {busy ? 'Saving…' : 'Save'}
    </button>
    <button
      type="button"
      data-testid="network-port-revert"
      class={SECONDARY_BUTTON_CLASS}
      disabled={busy || !dirty}
      onclick={revert}
    >
      Revert
    </button>
  </div>
</div>
