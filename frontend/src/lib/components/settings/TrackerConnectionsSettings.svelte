<script lang="ts">
  // Settings > Issues > Connections: this computer's tracker connections.
  // The page holds the computer's list while it is shown; the store reads
  // it and follows tracker:connections_updated. It takes its computer from
  // the settings context, so any surface that provides one can host it.
  import { onMount } from 'svelte';
  import { settingsComputer } from './settingsComputer';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import {
    acquireTrackerConnections,
    refreshTrackerConnections,
    trackerConnectionsOf,
  } from '../../stores/trackerConnections.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import TrackerConnectionAdd from './TrackerConnectionAdd.svelte';
  import TrackerConnectionRow from './TrackerConnectionRow.svelte';
  import { GHOST_BUTTON_CLASS } from './styles';

  const { backend, onHost, scopeGrant } = settingsComputer();

  onMount(() => acquireTrackerConnections(backend));

  let view = $derived(trackerConnectionsOf(backend));
  let readGrant = $derived(scopeGrant('tracker:read'));
  let operateGrant = $derived(scopeGrant('tracker:operate'));
  let operateReason = $derived(
    operateGrant === 'granted'
      ? ''
      : operateGrant === 'refused'
        ? 'This device may not change tracker connections on this computer.'
        : (grantBlockedTitle(operateGrant) ?? ''),
  );
  let host = $derived(onHost());
  let roots = $derived(view.list.filter((c) => !c.parentId));
</script>

<div class="settings-sections flex flex-col gap-4" data-testid="settings-tracker-connections">
  {#if readGrant === 'refused'}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-connections-refused">
      This device may not read the tracker connections on this computer.
    </p>
  {:else}
    {#if view.error !== null}
      <div class="flex flex-wrap items-center gap-2" role="alert">
        <p class="text-[0.75rem] text-error" data-testid="settings-tracker-connections-error">{userFacingError(view.error)}</p>
        <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerConnections(backend)}>Try again</button>
      </div>
    {/if}
    {#if !view.loaded}
      <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-connections-loading">Loading connections…</p>
    {:else if roots.length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-connections-empty">
        No connections yet. Add a Jira site or a Linear workspace to see its boards in a project's Issues pane.
      </p>
    {:else}
      <ul class="flex flex-col gap-4">
        {#each roots as connection (connection.id)}
          <TrackerConnectionRow
            {backend}
            {connection}
            tempo={view.list.find((c) => c.parentId === connection.id)}
            {host}
            {operateReason}
          />
        {/each}
      </ul>
    {/if}
    {#if view.loaded}
      <TrackerConnectionAdd {backend} {operateReason} />
    {/if}
  {/if}
</div>
