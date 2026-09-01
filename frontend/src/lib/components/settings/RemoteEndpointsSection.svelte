<script lang="ts">
  // RemoteEndpointsSection: settings UI for the `--connect` target
  // list. The desktop binary attaches to a remote-hosted backend by
  // launching with `agent-overflow --connect <url>?token=<value>`;
  // this panel stores the URL+token+nickname so the user doesn't have
  // to retype them, and provides a one-click copy of the launch command.
  //
  // Tokens are masked by default but revealable, mirroring the
  // password-input pattern other settings panels use. Persistence is
  // through the existing settings JSON via the binding RPCs, so the
  // tokens are written in plaintext alongside the rest of settings —
  // see internal/settings/remote.go for the security note.
  //
  // The per-row UI lives in `RemoteEndpointRow.svelte` and the form
  // lives in `RemoteEndpointForm.svelte`. This file owns the list
  // state + add/edit mode + the binding RPCs that mutate the list.
  import {
    AddRemoteEndpoint,
    DeleteRemoteEndpoint,
    GetRemoteEndpointToken,
    ListRemoteEndpoints,
    RemoteEndpointSummary,
    UpdateRemoteEndpoint,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { isClientMode } from '../../transport/runMode';
  import { hasScope } from '../../transport/scopes';
  import RemoteEndpointRow from './RemoteEndpointRow.svelte';
  import RemoteEndpointForm from './RemoteEndpointForm.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SECONDARY_BUTTON_CLASS } from './styles';

  // In `--connect` mode the SPA is talking to a remote backend. Every
  // RPC this panel issues (List/Add/Update/Delete/TouchRemoteEndpoint)
  // would mutate the *remote* server's settings.json — not the local
  // user's. That's both confusing (the launch command the user is
  // saving lives on a different machine) and a confidentiality risk
  // (tokens for unrelated remote machines could leak into the wrong
  // settings file). Render a read-only placeholder instead and let
  // the user edit endpoints from their local install.
  const clientMode = isClientMode();
  // The authorization axis, alongside that process-boot one. The list is
  // this install's saved launch targets, so `ListRemoteEndpoints` carries
  // `//ao:scope settings:write` — a view-only device holds no such grant,
  // and the mount's load spent a refusal to find that out (found by the
  // harness, 2026-08-31; stores/AGENTS.md § A PASSIVE load asks before it
  // fires).
  let ungranted = $derived(!hasScope('settings:write'));
  let unavailable = $derived(clientMode || ungranted);

  // EndpointSummary is the wire shape ListRemoteEndpoints returns:
  // metadata only, no token. Tokens are fetched on-demand via
  // GetRemoteEndpointToken so the bulk read path doesn't carry
  // credentials over the wire (LAN-bind safety).
  interface EndpointSummary {
    id: string;
    name: string;
    url: string;
    lastUsedAt?: number;
  }

  let endpoints = $state<EndpointSummary[]>([]);
  let loading = $state(true);
  let saving = $state(false);

  // Editor state. When `editingId === null` the editor is collapsed
  // and the "Add" button shows; setting it to '' enters new-record mode,
  // and any other ID enters edit mode against that record.
  let editingId = $state<string | null>(null);
  let formName = $state('');
  let formURL = $state('');
  let formToken = $state('');
  let formError = $state<string | null>(null);

  async function load(): Promise<void> {
    if (unavailable) {
      // Skip the RPC entirely in client mode. The remote backend would
      // happily return its own endpoints, but rendering them here
      // would mislead the user into thinking they're editing local
      // state.
      loading = false;
      return;
    }
    loading = true;
    try {
      const result = (await ListRemoteEndpoints()) as EndpointSummary[] | null;
      endpoints = result ?? [];
    } catch (err) {
      addToast('error', `Failed to load remote endpoints: ${errString(err)}`);
    } finally {
      loading = false;
    }
  }

  function startNew(): void {
    editingId = '';
    formName = '';
    formURL = '';
    formToken = '';
    formError = null;
  }

  async function startEdit(endpoint: EndpointSummary): Promise<void> {
    editingId = endpoint.id;
    formName = endpoint.name;
    formURL = endpoint.url;
    formError = null;
    // Token comes from a separate fetch so the bulk list response
    // never carries credentials. A fetch failure leaves the field
    // blank; the user can still re-enter the token if the row is
    // unrecoverable, and the server-side validator catches an empty
    // submit.
    formToken = '';
    try {
      formToken = (await GetRemoteEndpointToken(endpoint.id)) as string;
    } catch (err) {
      formError = `Failed to load token: ${errString(err)}`;
    }
  }

  function cancelEdit(): void {
    editingId = null;
    formError = null;
  }

  async function handleSave(values: { name: string; url: string; token: string }): Promise<void> {
    if (saving) return;
    saving = true;
    formError = null;
    try {
      if (editingId === '') {
        // Add returns the redacted RemoteEndpointSummary. Tokens are
        // fetched on-demand via GetRemoteEndpointToken so a LAN-attached
        // peer can't enumerate stored tokens by harvesting the Add
        // response. The token persisted by the backend is what the
        // copy-launch-command path eventually retrieves.
        const created = (await AddRemoteEndpoint(
          values.name,
          values.url,
          values.token,
        )) as RemoteEndpointSummary;
        endpoints = [
          ...endpoints,
          { id: created.id, name: created.name, url: created.url, lastUsedAt: created.lastUsedAt },
        ];
      } else if (editingId !== null) {
        // Update returns the redacted RemoteEndpointSummary for the
        // same threat-model reason as Add: a no-op edit must not
        // surface the persisted token.
        const updated = (await UpdateRemoteEndpoint(
          editingId,
          values.name,
          values.url,
          values.token,
        )) as RemoteEndpointSummary;
        endpoints = endpoints.map((e) =>
          e.id === updated.id
            ? { id: updated.id, name: updated.name, url: updated.url, lastUsedAt: updated.lastUsedAt }
            : e,
        );
      }
      cancelEdit();
    } catch (err) {
      formError = errString(err);
    } finally {
      saving = false;
    }
  }

  async function remove(endpoint: EndpointSummary): Promise<void> {
    if (saving) return;
    // No browser confirm() — happy-dom in tests blocks on it and
    // delete is reversible by re-adding from the share command anyway.
    saving = true;
    try {
      await DeleteRemoteEndpoint(endpoint.id);
      endpoints = endpoints.filter((e) => e.id !== endpoint.id);
      // If the editor was open on this endpoint, close it; the row is
      // gone.
      if (editingId === endpoint.id) cancelEdit();
    } catch (err) {
      addToast('error', `Failed to delete endpoint: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  $effect(() => {
    void load();
  });
</script>

<!-- Declared out here, and passed only in local mode: an `{#if}` INSIDE the
     snippet would still render the header's empty prose paragraph. -->
{#snippet connectUsage()}
  These endpoints power
  <code class="font-mono text-[0.6875rem]">agent-overflow --connect ws://host:port?token=&lt;value&gt;</code>;
  the desktop binary opens a window attached to the remote backend instead of
  booting a local one.
{/snippet}

<div class="flex flex-col gap-6">
  <section>
    <SettingsHeader
      title="Saved --connect targets"
      description={clientMode
        ? "Remote endpoints can only be edited from your local install. This window is attached to a remote backend, so changes here would update the remote machine's settings instead of yours."
        : ungranted
          ? 'Saved launch targets were not granted to this device. They belong to the install that stores them, and are edited on its own screen.'
          : 'Store the URL + token for a remote agent-overflow backend so you can launch the desktop app against it without retyping. Tokens are stored in plaintext alongside the rest of settings.'}
      details={unavailable ? undefined : connectUsage}
    />

    {#if !unavailable}
      {#if loading}
        <p class="text-[0.75rem] text-fg-muted">Loading…</p>
      {:else if endpoints.length === 0 && editingId === null}
        <p class="text-[0.75rem] text-fg-muted">No remote endpoints saved.</p>
      {/if}
    {/if}

    {#if !unavailable && !loading && endpoints.length > 0}
      <ul
        class="divide-y divide-border-subtle/60 overflow-hidden rounded-[var(--radius-control)] border border-border-subtle"
        data-testid="remote-endpoints-list"
      >
        {#each endpoints as endpoint (endpoint.id)}
          <RemoteEndpointRow
            {endpoint}
            {saving}
            onEdit={() => startEdit(endpoint)}
            onDelete={() => remove(endpoint)}
          />
        {/each}
      </ul>
    {/if}

    {#if !unavailable}
      <div class="mt-2.5">
        {#if editingId === null}
          <button type="button" onclick={startNew} class={SECONDARY_BUTTON_CLASS}>
            Add remote endpoint
          </button>
        {:else}
          <RemoteEndpointForm
            mode={editingId === '' ? 'add' : 'edit'}
            initialName={formName}
            initialURL={formURL}
            initialToken={formToken}
            initialError={formError}
            {saving}
            onSubmit={handleSave}
            onCancel={cancelEdit}
          />
        {/if}
      </div>
    {/if}
  </section>
</div>
