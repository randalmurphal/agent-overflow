<script lang="ts">
  // Settings > Issues > Status groups: the group each status of a
  // project's boards counts in for the project (docs/specs/issues.md,
  // "Setup"), by tracker site. A status counts in the tracker's own group
  // until the person puts it in another, or leaves it out of counts;
  // picking the tracker's group again puts it back. Every change saves at
  // once, and what shows is what the computer stored: the project's
  // setup and statuses are the setup store's (stores/trackerSetup), read
  // while this page shows them and again when the setup changes as stored
  // or the page is shown again.
  import { onMount, tick, untrack } from 'svelte';
  import type { SiteStatuses, StatusView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { isLoaded } from '../../stores/projects.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { acquireTrackerConnections, refreshTrackerConnections, trackerConnectionsOf, trackerReadGate } from '../../stores/trackerConnections.svelte';
  import {
    acquireTrackerSetup,
    acquireTrackerStatuses,
    editTrackerSetup,
    refreshTrackerSetup,
    refreshTrackerStatuses,
    STATUS_GROUPS,
    trackerSetupOf,
    trackerStatusesOf,
    withStatusGroup,
    type StatusGroup,
  } from '../../stores/trackerSetup.svelte';
  import { asSentence, userFacingError } from '../../utils/userFacingError';
  import { settingsComputerName } from '../../stores/settings.svelte';
  import SettingsOlderComputer from './SettingsOlderComputer.svelte';
  import TrackerProjectSelect, { computerProjects } from './TrackerProjectSelect.svelte';
  import { settingsComputer } from './settingsComputer';
  import { GHOST_BUTTON_CLASS, SELECT_CLASS } from './styles';
  import { trackerConnectionName, trackerKindName } from './trackerConnectionStatus';

  const { backend, scopeGrant } = settingsComputer();

  onMount(() => acquireTrackerConnections(backend));

  let projectCount = $derived(computerProjects(backend).length);
  let projectsLoaded = $derived(isLoaded());
  let selectedId = $state('');

  $effect(() => {
    const id = selectedId;
    if (!id) return;
    return untrack(() => {
      const setup = acquireTrackerSetup(id);
      const statuses = acquireTrackerStatuses(id);
      return () => {
        setup();
        statuses();
      };
    });
  });

  let connectionsView = $derived(trackerConnectionsOf(backend));
  let gate = $derived(trackerReadGate(connectionsView));
  let setup = $derived(selectedId ? trackerSetupOf(selectedId) : null);
  let statuses = $derived(selectedId ? trackerStatusesOf(selectedId) : null);
  let groupsNewer = $derived(setup?.view?.newer?.includes('status_groups') ?? false);

  let operateGrant = $derived(scopeGrant('tracker:operate'));
  let operateReason = $derived(
    operateGrant === 'granted'
      ? ''
      : operateGrant === 'refused'
        ? "This device may not change a project's status groups on this computer."
        : (grantBlockedTitle(operateGrant) ?? ''),
  );
  // A control is never disabled for a save or a read in flight, which
  // would take the keyboard's focus from it: the store builds each change
  // from the stored setup, reading it first when it must, and refuses a
  // second save while one runs, which the page says.
  let locked = $derived(operateReason !== '');

  // What the last save said, for the project it was made for: the
  // computer's refusal or failure.
  let saveNote = $state<{ kind: 'refused'; text: string } | { kind: 'failed'; error: unknown } | null>(null);
  $effect(() => {
    void selectedId;
    saveNote = null;
  });

  /** Saves the group `control` picked, then shows in it what is stored:
   * a refused or failed pick leaves the stored setup unchanged, so no
   * value of the page moves to set the control back. */
  async function setGroup(control: HTMLSelectElement | null, site: SiteStatuses, status: StatusView, group: StatusGroup | null): Promise<void> {
    const project = selectedId;
    saveNote = null;
    try {
      const outcome = await editTrackerSetup(project, (held) => withStatusGroup(held, site.tracker, site.site, status.id, group));
      if (project === selectedId && outcome.kind === 'refused') saveNote = { kind: 'refused', text: asSentence(outcome.sentence) };
    } catch (err) {
      if (project === selectedId) saveNote = { kind: 'failed', error: err };
    } finally {
      await tick();
      if (control?.isConnected && project === selectedId) control.value = shownGroup(site, status);
    }
  }

  /** The value a status's select shows: the group the setup puts it in,
   * or '' (the tracker's) when it puts it in none or in the tracker's. */
  function shownGroup(site: SiteStatuses, status: StatusView): string {
    const stored = storedGroup(site, status.id);
    return stored !== undefined && stored !== (status.group || undefined) ? stored : '';
  }

  /** The group the setup puts `status` of `site` in; undefined for none. */
  function storedGroup(site: SiteStatuses, status: string): string | undefined {
    return setup?.view?.setup.status_groups?.find((e) => e.tracker === site.tracker && e.site === site.site)?.statuses[status];
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

  function retry(): void {
    void refreshTrackerSetup(selectedId);
    void refreshTrackerStatuses(selectedId);
  }

  const compact = $derived(isCompactLayout());
</script>

<div class="settings-sections flex flex-col gap-4" data-testid="settings-tracker-statuses">
  {#if gate === 'older'}
    <SettingsOlderComputer {backend} lacks="issue trackers" />
  {:else if gate === 'refused'}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-statuses-refused">
      This device may not read a project's status groups on this computer.
    </p>
  {:else if gate === 'waiting'}
    <p class="text-[0.75rem] text-fg-muted">Asking this computer about its trackers…</p>
  {:else if gate === 'failed'}
    <div class="flex flex-wrap items-center gap-2" role="alert">
      <p class="text-[0.75rem] text-error">{userFacingError(connectionsView.error)}</p>
      <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerConnections(backend)}>Try again</button>
    </div>
  {:else if projectsLoaded && projectCount === 0}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-statuses-no-projects">
      No projects yet. Add one from the sidebar first.
    </p>
  {:else}
    <TrackerProjectSelect
      {backend}
      field="tracker-statuses.project"
      testId="settings-tracker-statuses-project"
      hint="Choose the project whose statuses to group."
      bind:selected={selectedId}
    />

    {#if setup && statuses}
      <div class="flex flex-col gap-3" data-settings-field="tracker-statuses.groups" data-settings-label="Status groups">
        <p class="text-[0.8125rem] font-medium text-fg">Status groups</p>
        {#if setup.error !== null || statuses.error !== null}
          <div class="flex flex-wrap items-center gap-2" role="alert">
            <p class="text-[0.75rem] text-error" data-testid="settings-tracker-statuses-error">
              {#if setup.error !== null}
                This project's tracker setup cannot be read: {userFacingError(setup.error)}
              {:else}
                The project's statuses could not be read: {userFacingError(statuses.error)}
              {/if}
            </p>
            <button type="button" class={GHOST_BUTTON_CLASS} onclick={retry} data-testid="settings-tracker-statuses-retry">Try again</button>
          </div>
        {/if}
        {#if groupsNewer}
          <p class="text-[0.75rem] text-warning" data-testid="settings-tracker-statuses-newer">
            A newer version of Agent Overflow saved this project's status groups. They are kept as they are; update the app on {settingsComputerName(backend) || 'this computer'} to change them here.
          </p>
        {:else if setup.view === null || statuses.sites === null}
          {#if setup.error === null && statuses.error === null}
            <p class="text-[0.75rem] text-fg-muted">Reading the statuses…</p>
          {/if}
        {:else if statuses.sites.length === 0}
          <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-statuses-empty">
            This project lists no boards. Add one in Boards to set how its statuses count.
          </p>
        {:else}
          {#each statuses.sites as site (`${site.tracker}\u0000${site.site}`)}
            <section class="flex flex-col gap-2" data-testid="tracker-status-site" data-site={site.site}>
              <p class="text-[0.75rem] font-medium text-fg-muted">{siteName(site)}</p>
              {#if site.statuses.length > 0}
                <ul class="flex flex-col gap-2">
                  {#each site.statuses as status (status.id)}
                    {@render statusRow(site, status)}
                  {/each}
                </ul>
              {/if}
              {#each site.unread ?? [] as board (board.board)}
                <div class="flex flex-wrap items-center gap-2" data-testid="tracker-status-unread">
                  <p class="text-[0.75rem] text-fg-muted">
                    {board.name || `Board ${board.board}`} has not been read on this computer yet. Open it once in the Issues pane to list its statuses.
                  </p>
                  <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerStatuses(selectedId)} data-testid="tracker-status-unread-retry">Check again</button>
                </div>
              {/each}
              {#if (site.elsewhere?.length ?? 0) > 0}
                <p class="text-[0.75rem] text-fg-muted">Regrouped, and on none of the project's boards now:</p>
                <ul class="flex flex-col gap-2">
                  {#each site.elsewhere ?? [] as status (status.id)}
                    <li class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1" data-testid="tracker-status-elsewhere" data-status={status.id}>
                      <span class="min-w-0 truncate text-[0.8125rem] text-fg">{status.name || status.id}</span>
                      <span class="text-[0.71875rem] text-fg-hint">{groupName(storedGroup(site, status.id))}</span>
                      <button
                        type="button"
                        class={GHOST_BUTTON_CLASS}
                        disabled={locked}
                        title={operateReason || undefined}
                        onclick={() => void setGroup(null, site, status, null)}
                        data-testid="tracker-status-put-back"
                      >Put back</button>
                    </li>
                  {/each}
                </ul>
              {/if}
            </section>
          {/each}
        {/if}
        {#if saveNote}
          <p class="text-[0.75rem] text-error" role="alert" data-testid="settings-tracker-statuses-save-note">
            {saveNote.kind === 'refused' ? `${saveNote.text} Make the change again.` : userFacingError(saveNote.error)}
          </p>
        {/if}
      </div>
    {/if}
  {/if}
</div>

{#snippet statusRow(site: SiteStatuses, status: StatusView)}
  {@const shown = shownGroup(site, status)}
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
      disabled={locked}
      title={operateReason || undefined}
      onchange={(e) => {
        const control = e.currentTarget as HTMLSelectElement;
        void setGroup(control, site, status, control.value === '' ? null : (control.value as StatusGroup));
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
