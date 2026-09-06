<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  import { createAgentComputers } from '../../stores/agentComputers.svelte';
  import { backendDisplayName } from '../../stores/attachedBackends.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import Button from '../primitives/Button.svelte';
  import { INPUT_CLASS } from './styles';

  const { backend, hasScope } = settingsComputer();
  const computers = createAgentComputers(backend);
  let target = $state('');
  async function connect(): Promise<void> {
    if (await computers.connect(target)) target = '';
  }
</script>

{#if computers.capable() && hasScope('terminal:operate')}
  <section>
    <SettingsHeader title="Agent access to other computers" description="Let agents on this computer run commands in another computer’s projects." />
    {#if computers.error}
      <p role="alert" class="mb-2 text-sm text-error">{computers.error}</p>
      {#if computers.repairTarget && hasScope('access:admin')}
        <Button size="xs" variant="secondary" disabled={computers.busy} onclick={() => void computers.connect(computers.repairTarget)}>Connect again</Button>
      {:else}
        <Button size="xs" variant="ghost" disabled={computers.busy} onclick={() => void computers.load()}>Retry</Button>
      {/if}
    {/if}
    {#each computers.rows as row (row.id)}
      <div class="flex min-w-0 items-center justify-between gap-3 py-2">
        <span class="truncate text-sm">{row.name}</span>
        <Button size="xs" variant={row.enabled ? 'secondary' : 'primary'} disabled={computers.busy} onclick={() => void computers.toggle(row)} pressed={row.enabled}>{row.enabled ? 'Enabled' : 'Enable'}</Button>
      </div>
    {/each}
    {#if computers.candidates.length > 0 && hasScope('access:admin')}
      <div class="mt-2 flex gap-2">
        <select class={`${INPUT_CLASS} min-w-0 flex-1`} bind:value={target} disabled={computers.busy} aria-label="Computer for agent commands">
          <option value="">Choose a computer…</option>
          {#each computers.candidates as entry (entry.id)}<option value={computers.identity(entry.id)}>{backendDisplayName(entry)}</option>{/each}
        </select>
        <Button size="sm" variant="primary" disabled={computers.busy || !target} onclick={() => void connect()}>{computers.busy ? 'Connecting…' : 'Enable access'}</Button>
      </div>
    {:else if computers.loaded && computers.rows.length === 0}
      <p class="text-sm text-fg-muted">Add another computer in Remote access → Connections to enable agent access.</p>
    {/if}
  </section>
{:else}
  <p class="text-sm text-fg-muted">Agent access is unavailable on this connection. Use an updated host with permission to run commands.</p>
{/if}
