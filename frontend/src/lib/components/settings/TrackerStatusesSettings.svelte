<script lang="ts">
  // Settings > Issues > Status groups: the group each status of a
  // project's boards counts in for the project (docs/specs/issues.md,
  // "Setup"), by tracker site. A status counts in the tracker's own group
  // until the person puts it in another, or leaves it out of counts;
  // picking the tracker's group again puts it back. The frame
  // (TrackerProjectSettings) holds the project's setup and says what the
  // pages share; this page also holds the project's statuses, the setup
  // store's (stores/trackerSetup), read while it shows them and again
  // when the setup changes as stored or the page is shown again.
  import { tick } from 'svelte';
  import type { Setup, SiteStatuses, StatusView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import {
    acquireTrackerStatuses,
    refreshTrackerStatuses,
    STATUS_GROUPS,
    trackerSetupOf,
    trackerStatusesOf,
    withStatusGroup,
    type StatusGroup,
  } from '../../stores/trackerSetup.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import TrackerProjectSettings, { type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { settingsComputer } from './settingsComputer';
  import { GHOST_BUTTON_CLASS, SELECT_CLASS } from './styles';
  import { trackerConnectionName, trackerKindName } from './trackerConnectionStatus';

  const { backend } = settingsComputer();
  let connectionsView = $derived(trackerConnectionsOf(backend));

  function statusesError(project: string): string | null {
    const error = trackerStatusesOf(project).error;
    return error !== null ? `The project's statuses could not be read: ${userFacingError(error)}` : null;
  }

  /** Saves the group `control` picked, then shows in it what is stored:
   * a refused or failed pick leaves the stored setup unchanged, so no
   * value of the page moves to set the control back. */
  async function setGroup(page: TrackerProjectPage, control: HTMLSelectElement | null, site: SiteStatuses, status: StatusView, group: StatusGroup | null): Promise<void> {
    const shown = await page.save((held) => withStatusGroup(held, site.tracker, site.site, status.id, group));
    await tick();
    const stored = trackerSetupOf(page.project).view?.setup;
    if (shown && stored && control?.isConnected) control.value = shownGroup(stored, site, status);
  }

  /** The value a status's select shows: the group the setup puts it in,
   * or '' (the tracker's) when it puts it in none or in the tracker's. */
  function shownGroup(setup: Setup, site: SiteStatuses, status: StatusView): string {
    const stored = storedGroup(setup, site, status.id);
    return stored !== undefined && stored !== (status.group || undefined) ? stored : '';
  }

  /** The group the setup puts `status` of `site` in; undefined for none. */
  function storedGroup(setup: Setup, site: SiteStatuses, status: string): string | undefined {
    return setup.status_groups?.find((e) => e.tracker === site.tracker && e.site === site.site)?.statuses[status];
  }

  const GROUP_NAMES: Record<string, string> = {
    backlog: 'Backlog',
    todo: 'To do',
    in_progress: 'In progress',
    done: 'Done',
    canceled: 'Canceled',
    left_out: 'Left out of counts',
  };

  function groupName(group: string | undefined): string {
    return (group && GROUP_NAMES[group]) || 'not known here';
  }

  function siteName(site: SiteStatuses): string {
    const connection = connectionsView.list.find((c) => c.kind === site.tracker && c.site === site.site);
    return connection ? trackerConnectionName(connection) : trackerConnectionName({ label: '', kind: site.tracker, site: site.site });
  }

  const compact = $derived(isCompactLayout());
</script>

<TrackerProjectSettings
  page="statuses"
  projectField="tracker-statuses.project"
  bodyField="tracker-statuses.groups"
  heading="Status groups"
  hint="Choose the project whose statuses to group."
  what="status groups"
  section="status_groups"
  hold={acquireTrackerStatuses}
  readError={statusesError}
  ready={(project) => trackerStatusesOf(project).sites !== null}
  retry={(project) => void refreshTrackerStatuses(project)}
  reading="Reading the statuses…"
>
  {#snippet body(page)}
    {@const sites = trackerStatusesOf(page.project).sites ?? []}
    {#if sites.length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-statuses-empty">
        This project lists no boards. Add one in Boards to set how its statuses count.
      </p>
    {:else}
      {#each sites as site (`${site.tracker}\u0000${site.site}`)}
        <section class="flex flex-col gap-2" data-testid="tracker-status-site" data-site={site.site}>
          <p class="text-[0.75rem] font-medium text-fg-muted">{siteName(site)}</p>
          {#if site.statuses.length > 0}
            <ul class="flex flex-col gap-2">
              {#each site.statuses as status (status.id)}
                {@render statusRow(page, site, status)}
              {/each}
            </ul>
          {/if}
          {#each site.unread ?? [] as board (board.board)}
            <div class="flex flex-wrap items-center gap-2" data-testid="tracker-status-unread">
              <p class="text-[0.75rem] text-fg-muted">
                {board.name || `Board ${board.board}`} has not been read on this computer yet. Open it once in the Issues pane to list its statuses.
              </p>
              <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerStatuses(page.project)} data-testid="tracker-status-unread-retry">Check again</button>
            </div>
          {/each}
          {#if (site.elsewhere?.length ?? 0) > 0}
            <p class="text-[0.75rem] text-fg-muted">Regrouped, and on none of the project's boards now:</p>
            <ul class="flex flex-col gap-2">
              {#each site.elsewhere ?? [] as status (status.id)}
                <li class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1" data-testid="tracker-status-elsewhere" data-status={status.id}>
                  <span class="min-w-0 truncate text-[0.8125rem] text-fg">{status.name || status.id}</span>
                  <span class="text-[0.71875rem] text-fg-hint">{groupName(storedGroup(page.setup, site, status.id))}</span>
                  <button
                    type="button"
                    class={GHOST_BUTTON_CLASS}
                    disabled={page.lockReason !== ''}
                    title={page.lockReason || undefined}
                    onclick={() => void setGroup(page, null, site, status, null)}
                    data-testid="tracker-status-put-back"
                  >Put back</button>
                </li>
              {/each}
            </ul>
          {/if}
        </section>
      {/each}
    {/if}
  {/snippet}
</TrackerProjectSettings>

{#snippet statusRow(page: TrackerProjectPage, site: SiteStatuses, status: StatusView)}
  {@const shown = shownGroup(page.setup, site, status)}
  {@const own = status.group || undefined}
  {@const changed = shown !== ''}
  {@const tracker = trackerKindName(site.tracker)}
  <li class="flex min-w-0 gap-x-3 gap-y-1 {compact ? 'flex-col' : 'flex-wrap items-center'}" data-testid="tracker-status-row" data-status={status.id}>
    <div class="flex min-w-0 flex-1 flex-col">
      <span class="flex min-w-0 items-baseline gap-2">
        <span class="truncate text-[0.8125rem] text-fg" data-testid="tracker-status-name">{status.name || status.id}</span>
        {#if changed}<span class="text-[0.6875rem] text-accent" data-testid="tracker-status-changed">Changed</span>{/if}
      </span>
      <span class="text-[0.71875rem] text-fg-hint" data-testid="tracker-status-own">{tracker}: {groupName(own)}</span>
    </div>
    <select
      class={SELECT_CLASS}
      aria-label="Group of {status.name || status.id}"
      value={shown}
      disabled={page.lockReason !== ''}
      title={page.lockReason || undefined}
      onchange={(e) => {
        const control = e.currentTarget as HTMLSelectElement;
        void setGroup(page, control, site, status, control.value === '' ? null : (control.value as StatusGroup));
      }}
      data-testid="tracker-status-group"
    >
      <option value="">{own ? `${groupName(own)} (${tracker}'s)` : `${tracker}'s group`}</option>
      {#each STATUS_GROUPS.filter((g) => g !== own) as group (group)}
        <option value={group}>{GROUP_NAMES[group]}</option>
      {/each}
    </select>
  </li>
{/snippet}
