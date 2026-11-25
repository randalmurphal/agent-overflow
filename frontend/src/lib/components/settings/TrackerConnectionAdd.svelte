<script lang="ts">
  // Adding a Jira or Linear connection takes only what is not secret: the
  // site and account, or the workspace. The computer checks and normalizes
  // them, makes the row and an empty key file, and the new row shows the
  // steps that put the token in. Tempo is added from its Jira row.
  import type { BackendKey } from '../../transport/backendKey';
  import { addTrackerConnection } from '../../stores/trackerConnections.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import Segmented from '../primitives/Segmented.svelte';
  import { GHOST_BUTTON_CLASS, INPUT_CLASS, PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';

  let { backend, operateReason }: { backend: BackendKey; operateReason: string } = $props();

  const id = $props.id();
  let open = $state(false);
  let kind = $state<'jira' | 'linear'>('jira');
  let site = $state('');
  let account = $state('');
  let label = $state('');
  let busy = $state(false);
  let error = $state<unknown>(null);

  let ready = $derived(site.trim() !== '' && (kind === 'linear' || account.trim() !== ''));

  function reset(): void {
    site = '';
    account = '';
    label = '';
    error = null;
  }

  async function submit(): Promise<void> {
    busy = true;
    error = null;
    try {
      await addTrackerConnection(backend, {
        kind,
        site: site.trim(),
        account: kind === 'jira' ? account.trim() : '',
        label: label.trim(),
      });
      reset();
      open = false;
    } catch (err) {
      error = err;
    } finally {
      busy = false;
    }
  }
</script>

<div
  class="flex flex-col gap-2"
  data-settings-field="tracker-connections.add"
  data-settings-label="Add a connection"
>
  {#if !open}
    <div>
      <button
        type="button"
        class={SECONDARY_BUTTON_CLASS}
        disabled={operateReason !== ''}
        title={operateReason || undefined}
        onclick={() => (open = true)}
        data-testid="tracker-connection-add"
      >Add a connection</button>
    </div>
  {:else}
    <form
      class="flex flex-col gap-2.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/40 p-3"
      data-testid="tracker-connection-add-form"
      onsubmit={(e) => {
        e.preventDefault();
        if (ready && !busy) void submit();
      }}
    >
      <p class="text-[0.8125rem] font-medium text-fg">Add a connection</p>
      <Segmented
        ariaLabel="Tracker"
        value={kind}
        options={[
          { value: 'jira', label: 'Jira' },
          { value: 'linear', label: 'Linear' },
        ]}
        onChange={(value) => {
          kind = value as 'jira' | 'linear';
          error = null;
        }}
      />
      {#if kind === 'jira'}
        <label class="flex flex-col gap-1 text-[0.75rem] text-fg" for="{id}-site">
          Jira site
          <input id="{id}-site" class={INPUT_CLASS} bind:value={site} placeholder="example.atlassian.net" autocomplete="off" data-testid="tracker-connection-add-site" />
        </label>
        <label class="flex flex-col gap-1 text-[0.75rem] text-fg" for="{id}-account">
          Email you sign in to Jira with
          <input id="{id}-account" class={INPUT_CLASS} type="email" bind:value={account} placeholder="you@example.com" autocomplete="off" data-testid="tracker-connection-add-account" />
        </label>
      {:else}
        <label class="flex flex-col gap-1 text-[0.75rem] text-fg" for="{id}-site">
          Linear workspace
          <input id="{id}-site" class={INPUT_CLASS} bind:value={site} placeholder="linear.app/example" autocomplete="off" data-testid="tracker-connection-add-site" />
        </label>
      {/if}
      <label class="flex flex-col gap-1 text-[0.75rem] text-fg" for="{id}-label">
        Name (optional)
        <input id="{id}-label" class={INPUT_CLASS} bind:value={label} maxlength={200} autocomplete="off" data-testid="tracker-connection-add-label" />
      </label>
      <p class="text-[0.71875rem] text-fg-muted">
        No token goes here. Once the connection is added, it shows where to save the token on this computer.
      </p>
      {#if error !== null}
        <p class="text-[0.71875rem] text-error" role="alert" data-testid="tracker-connection-add-error">{userFacingError(error)}</p>
      {/if}
      <div class="flex flex-wrap gap-2">
        <button type="submit" class={PRIMARY_BUTTON_CLASS} disabled={!ready || busy || operateReason !== ''} title={operateReason || undefined} data-testid="tracker-connection-add-submit">
          {busy ? 'Adding…' : 'Add'}
        </button>
        <button
          type="button"
          class={GHOST_BUTTON_CLASS}
          onclick={() => {
            reset();
            open = false;
          }}
        >Cancel</button>
      </div>
    </form>
  {/if}
</div>
