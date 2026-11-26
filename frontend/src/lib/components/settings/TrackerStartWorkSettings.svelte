<script lang="ts">
  // Settings > Issues > Starting work: what Start work on a ticket of the
  // project does (docs/specs/issues.md, "Start work applies per-project
  // defaults"), for each tracker project its boards name (WEB of WEB-1)
  // and each one the setup already holds defaults for: move the ticket to
  // a status, assign it to the person, name the branch after the ticket,
  // and the project the thread runs in. Each change saves at once through
  // the rule Start work's "Save as defaults" writes by
  // (stores/startWorkDefaults), so the form opens with what the page
  // saved and the page shows what the form saved.
  import { tick } from 'svelte';
  import type { Setup, SiteStatuses, StartWorkDefaults, StatusView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { getProjectLabelText } from '../../stores/projects.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { defaultsAt, withStartWorkDefaults, type DefaultsChange, type DefaultsPlace } from '../../stores/startWorkDefaults';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import { acquireTrackerProjectBoards, refreshTrackerProjectBoards, trackerProjectBoardsOf } from '../../stores/trackerProjectBoards.svelte';
  import { acquireTrackerStatuses, refreshTrackerStatuses, trackerSetupOf, trackerStatusesOf } from '../../stores/trackerSetup.svelte';
  import { ticketBranchName } from '../../utils/ticketBranch';
  import { userFacingError } from '../../utils/userFacingError';
  import { workProjects } from '../issues/ticketWorkGate';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import TrackerProjectSettings, { type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { settingsComputer } from './settingsComputer';
  import { SELECT_CLASS } from './styles';
  import { trackerConnectionName } from './trackerConnectionStatus';

  const { backend } = settingsComputer();
  let connections = $derived(trackerConnectionsOf(backend).list);

  function hold(project: string): () => void {
    const boards = acquireTrackerProjectBoards(backend, project);
    const statuses = acquireTrackerStatuses(project);
    return () => {
      boards();
      statuses();
    };
  }

  function readError(project: string): string | null {
    const boards = trackerProjectBoardsOf(backend, project).error;
    if (boards !== null) return `The project's boards could not be read: ${userFacingError(boards)}`;
    const statuses = trackerStatusesOf(project).error;
    return statuses !== null ? `The project's statuses could not be read: ${userFacingError(statuses)}` : null;
  }

  /** A tracker project as a key's prefix spells it (trackerapp's
   * isTicketProject): its defaults' place. */
  const TICKET_PROJECT = /^[A-Z][A-Z0-9_]+$/;

  /** The places the page offers: each tracker project a board of the
   * project names, in picker order, then each one the setup holds
   * defaults for that no board names. */
  function placesOf(project: string, setup: Setup): DefaultsPlace[] {
    const out: DefaultsPlace[] = [];
    const add = (place: DefaultsPlace) => {
      if (!out.some((p) => p.tracker === place.tracker && p.site === place.site && p.project === place.project)) out.push(place);
    };
    for (const board of trackerProjectBoardsOf(backend, project).list) {
      const connection = connections.find((c) => c.id === board.connection);
      if (connection && TICKET_PROJECT.test(board.tracker_project)) add({ tracker: connection.kind, site: connection.site, project: board.tracker_project });
    }
    for (const entry of setup.start_work ?? []) add({ tracker: entry.tracker, site: entry.site, project: entry.project });
    return out;
  }

  function siteName(place: DefaultsPlace): string {
    const connection = connections.find((c) => c.kind === place.tracker && c.site === place.site);
    return trackerConnectionName(connection ?? { label: '', kind: place.tracker, site: place.site });
  }

  /** The site's statuses the picker lists, as the project's boards show
   * them. */
  function siteStatuses(project: string, place: DefaultsPlace): SiteStatuses | undefined {
    return trackerStatusesOf(project).sites?.find((s) => s.tracker === place.tracker && s.site === place.site);
  }

  /** The status a move turns on with: the first the project counts in
   * progress, else the first listed. */
  function firstStatus(setup: Setup, place: DefaultsPlace, statuses: readonly StatusView[]): StatusView | undefined {
    const regroups = setup.status_groups?.find((e) => e.tracker === place.tracker && e.site === place.site)?.statuses ?? {};
    return statuses.find((s) => (regroups[s.id] ?? s.group) === 'in_progress') ?? statuses[0];
  }

  /** Saves `delta` of the entry at `place`; a picker that made it then
   * shows what is stored (`shownOf`), since a refused or failed change
   * moves no value of the page to set it back. */
  async function change(page: TrackerProjectPage, place: DefaultsPlace, delta: DefaultsChange, picker?: { control: HTMLSelectElement; shownOf: (entry: StartWorkDefaults | null) => string }): Promise<void> {
    const shown = await page.save((held) => withStartWorkDefaults(held, place, page.project, delta));
    if (!picker) return;
    await tick();
    const stored = trackerSetupOf(page.project).view?.setup;
    if (shown && stored && picker.control.isConnected) picker.control.value = picker.shownOf(defaultsAt(stored, place));
  }

  const compact = $derived(isCompactLayout());
</script>

<TrackerProjectSettings
  page="start-work"
  projectField="tracker-start-work.project"
  bodyField="tracker-start-work.defaults"
  heading="Start work"
  hint="Choose the project whose Start work to set."
  what="Start work defaults"
  section="start_work"
  {hold}
  {readError}
  ready={(project) => trackerProjectBoardsOf(backend, project).loaded && trackerStatusesOf(project).sites !== null}
  retry={(project) => {
    void refreshTrackerProjectBoards(backend, project);
    void refreshTrackerStatuses(project);
  }}
  reading="Reading the defaults…"
>
  {#snippet body(page: TrackerProjectPage)}
    {@const places = placesOf(page.project, page.setup)}
    {#if places.length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-start-work-empty">
        This project lists no boards. Add one in Boards to set what Start work does on its tickets.
      </p>
    {:else}
      {#each places as place (`${place.tracker}\u0000${place.site}\u0000${place.project}`)}
        {@render placeRows(page, place, defaultsAt(page.setup, place))}
      {/each}
    {/if}
    <p class="text-[0.71875rem] text-fg-hint">Ask is separate: it starts a thread with the ticket attached and changes nothing on the ticket.</p>
  {/snippet}
</TrackerProjectSettings>

{#snippet placeRows(page: TrackerProjectPage, place: DefaultsPlace, entry: StartWorkDefaults | null)}
  {@const locked = page.lockReason !== ''}
  {@const statuses = siteStatuses(page.project, place)?.statuses ?? []}
  {@const status = entry?.status ?? ''}
  {@const first = firstStatus(page.setup, place, statuses)}
  {@const projects = workProjects(backend, page.project)}
  {@const repository = entry?.repository ?? page.project}
  {@const named = entry?.name_branch ?? true}
  <section class="flex flex-col gap-3" data-testid="tracker-start-work-place" data-project={place.project} data-site={place.site}>
    <p class="text-[0.75rem] font-medium text-fg-muted">{place.project} tickets on {siteName(place)}</p>
    <div class="flex min-w-0 gap-x-3 gap-y-2 {compact ? 'flex-col' : 'flex-wrap items-center'}" data-testid="tracker-start-work-move">
      <div class="flex min-w-0 flex-1 items-center gap-3">
        <span class="min-w-0 flex-1 text-[0.8125rem] text-fg">Move the ticket</span>
        <ToggleSwitch
          checked={status !== ''}
          ariaLabel="Move the ticket"
          disabled={locked || (status === '' && !first)}
          title={page.lockReason || (status === '' && !first ? 'No status of this tracker is listed yet: open a board of the project once in the Issues pane.' : undefined)}
          onToggle={(on) => void change(page, place, { status: on ? first?.id ?? null : null })}
        />
      </div>
      {#if status !== ''}
        <select
          class={SELECT_CLASS}
          aria-label="Move the ticket to"
          value={status}
          disabled={locked}
          title={page.lockReason || undefined}
          onchange={(e) => {
            const control = e.currentTarget as HTMLSelectElement;
            void change(page, place, { status: control.value }, { control, shownOf: (stored) => stored?.status ?? '' });
          }}
          data-testid="tracker-start-work-status"
        >
          {#if !statuses.some((s) => s.id === status)}
            <option value={status}>{status} (on none of the project's boards)</option>
          {/if}
          {#each statuses as s (s.id)}
            <option value={s.id}>{s.name || s.id}</option>
          {/each}
        </select>
      {/if}
    </div>
    <div class="flex min-w-0 items-center gap-3" data-testid="tracker-start-work-assign">
      <span class="min-w-0 flex-1 text-[0.8125rem] text-fg">Assign it to me</span>
      <ToggleSwitch
        checked={entry?.assign ?? true}
        ariaLabel="Assign it to me"
        disabled={locked}
        title={page.lockReason || undefined}
        onToggle={(on) => void change(page, place, { assign: on })}
      />
    </div>
    <div class="flex min-w-0 items-center gap-3" data-testid="tracker-start-work-branch">
      <div class="flex min-w-0 flex-1 flex-col">
        <span class="text-[0.8125rem] text-fg">Name the branch after the ticket</span>
        <span class="text-[0.71875rem] text-fg-hint" data-testid="tracker-start-work-branch-example">
          {named ? ticketBranchName(entry?.branch ?? '', `${place.project}-123`, 'Short title') : 'Start work opens with the branch box empty, for you to name it.'}
        </span>
      </div>
      <ToggleSwitch
        checked={named}
        ariaLabel="Name the branch after the ticket"
        disabled={locked}
        title={page.lockReason || undefined}
        onToggle={(on) => void change(page, place, { nameBranch: on })}
      />
    </div>
    <label class="flex min-w-0 gap-x-3 gap-y-2 {compact ? 'flex-col' : 'flex-wrap items-center'}">
      <span class="min-w-0 flex-1 text-[0.8125rem] text-fg">Run the thread in</span>
      <select
        class={SELECT_CLASS}
        value={repository}
        disabled={locked}
        title={page.lockReason || undefined}
        onchange={(e) => {
          const control = e.currentTarget as HTMLSelectElement;
          void change(page, place, { repository: control.value }, { control, shownOf: (stored) => stored?.repository ?? page.project });
        }}
        data-testid="tracker-start-work-repository"
      >
        {#if !projects.some((p) => p.id === repository)}
          <option value={repository}>A project no longer on this computer</option>
        {/if}
        {#each projects as p (p.id)}
          <option value={p.id}>{getProjectLabelText(p.id)}</option>
        {/each}
      </select>
    </label>
  </section>
{/snippet}
