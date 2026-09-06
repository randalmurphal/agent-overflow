<script lang="ts">
  // Which of this machine's dev-server ports the owner's other devices can
  // open. Its own component for the reason the port and domain editors are
  // their own: NetworkSection owns one load / save round trip against
  // `NetworkSettings`, and this list is fed by a different store on a push
  // channel of its own (`stores/devServers`).
  //
  // Sharing is not sending: a port on this list is reachable by a device
  // that has already paired and been granted access, through this backend,
  // and by nothing else on the network. That is why it is `access:admin` to
  // change and why it lives beside the bind toggle rather than in a thread.
  //
  // Adding a port that nothing is serving yet is allowed on purpose. The
  // usual order is that you know the port before the server is up, and a
  // control that refused until something answered would have to be pressed
  // again at the moment attention has moved on.

  import {
    allowPreviewPort,
    allowedPreviewPorts,
    attributedPreviewPorts,
    disallowPreviewPort,
    loadDevServers,
    machineDevServers,
    sharedPreviewPorts,
  } from '../../stores/devServers.svelte';
  import { settingsComputer } from './settingsComputer';
  const { backend } = settingsComputer();
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS, SECTION_PROSE_CLASS } from './styles';

  const MAX_PORT = 65535;

  let draft = $state('');

  let machine = $derived(machineDevServers(backend));
  // Two kinds of shared port, and only one of them has a control. A port
  // in the persisted set is this list's to take back; a port shared because
  // a thread is running a server on it goes away when that run does, and
  // `DisallowPreviewPort` would edit a set it is not in. `ports` is both,
  // because the field below refuses a port that is already reachable
  // whichever way it got there.
  let ports = $derived(allowedPreviewPorts(backend));
  let shared = $derived(sharedPreviewPorts(backend));
  let attributed = $derived(attributedPreviewPorts(backend));
  let previewHost = $derived(machine.list?.previewHost ?? '');
  let answered = $derived(machine.list !== null);

  let trimmed = $derived(draft.trim());
  let parsed = $derived(trimmed === '' ? 0 : Number(trimmed));
  let error = $derived.by(() => {
    if (trimmed === '') return null;
    if (!/^\d+$/.test(trimmed)) return 'Enter a number.';
    if (parsed < 1 || parsed > MAX_PORT) return `Enter a port between 1 and ${MAX_PORT}.`;
    if (ports.includes(parsed)) return 'That port is already shared.';
    return null;
  });
  let canAdd = $derived(trimmed !== '' && error === null);

  async function add(): Promise<void> {
    if (!canAdd) return;
    const port = parsed;
    // Cleared before the round trip rather than after it: the list is
    // reconciled by the backend's next frame, so there is nothing here to
    // roll back, and a field that stays full reads as a press that missed.
    draft = '';
    await allowPreviewPort(backend, port);
  }

  $effect(() => {
    void loadDevServers(backend);
  });
</script>

<section data-testid="network-preview-ports">
  <SettingsHeader
    title="Preview ports"
    description="Ports on this machine your other devices can open. A device has to be paired and signed in; nothing else on the network can reach them."
  />

  {#if answered && previewHost === ''}
    <p class={SECTION_PROSE_CLASS} data-testid="preview-ports-no-address">
      This machine has no address to serve previews on. Turn on remote access above,
      or join a tailnet, and these ports become reachable.
    </p>
  {/if}

  {#if ports.length === 0}
    <p class={SECTION_PROSE_CLASS} data-testid="preview-ports-empty">
      No ports are shared yet.
    </p>
  {:else}
    <ul class="flex flex-col gap-1" data-testid="preview-ports-list">
      {#each shared as port (port)}
        <li class="flex items-center gap-2" data-testid="preview-port-row" data-port={port}>
          <span class="font-mono text-[0.8125rem] text-fg">{port}</span>
          <button
            type="button"
            class={SECONDARY_BUTTON_CLASS}
            data-testid="preview-port-remove"
            aria-label="Stop sharing port {port}"
            onclick={() => void disallowPreviewPort(backend, port)}
          >
            Stop sharing
          </button>
        </li>
      {/each}
      {#each attributed as row (row.port)}
        <li
          class="flex items-center gap-2"
          data-testid="preview-port-attributed"
          data-port={row.port}
        >
          <span class="font-mono text-[0.8125rem] text-fg-muted">{row.port}</span>
          <span class="text-[0.71875rem] text-fg-hint">
            Shared while {row.process === '' ? 'a thread' : row.process} runs it
          </span>
        </li>
      {/each}
    </ul>
  {/if}

  <div class="mt-3">
    <SettingsField
      id="remote.preview-port-add"
      label="Share a port"
      htmlFor="preview-port-add"
      stacked
    >
      <div class="flex items-center gap-2">
        <input
          id="preview-port-add"
          data-testid="preview-port-input"
          type="text"
          inputmode="numeric"
          value={draft}
          placeholder="5173"
          autocomplete="off"
          spellcheck="false"
          aria-invalid={error !== null}
          oninput={(e) => (draft = (e.target as HTMLInputElement).value)}
          class="{INPUT_CLASS} max-w-[10rem] font-mono"
        />
        <button
          type="button"
          class={PRIMARY_BUTTON_CLASS}
          data-testid="preview-port-add"
          disabled={!canAdd}
          onclick={() => void add()}
        >
          Share
        </button>
      </div>
      {#if error}
        <p class="mt-1 text-[0.71875rem] text-error" role="alert" data-testid="preview-port-error">
          {error}
        </p>
      {/if}
    </SettingsField>
  </div>

  {#if machine.actionError}
    <p class="mt-2 text-[0.71875rem] text-error" role="alert" data-testid="preview-port-action-error">
      {machine.actionError}
    </p>
  {/if}
</section>
