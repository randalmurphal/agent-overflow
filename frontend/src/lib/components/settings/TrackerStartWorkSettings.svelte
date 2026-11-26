<script lang="ts">
  // Settings > Issues > Starting work: what Start work on a ticket of the
  // project does (docs/specs/issues.md, "Start work applies per-project
  // defaults"), for each place the computer lists (trackerapp's
  // ProjectStatuses.places: each tracker project its boards name, WEB of
  // WEB-1, and each one the setup holds defaults for, with the statuses
  // Move the ticket offers it): move the ticket to
  // a status, assign it to the person, name the branch after the ticket,
  // and the project the thread runs in. Each change saves at once through
  // the rule Start work's "Save as defaults" writes by
  // (stores/startWorkDefaults), so the form opens with what the page
  // saved and the page shows what the form saved.
  import { tick } from 'svelte';
  import type { Setup, StartPlace, StartWorkDefaults, StatusView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { getProjectLabelText } from '../../stores/projects.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { defaultsAt, withStartWorkDefaults, type DefaultsChange, type DefaultsPlace } from '../../stores/startWorkDefaults';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import { acquireTrackerStatuses, refreshTrackerStatuses, trackerSetupOf, trackerStatusesOf } from '../../stores/trackerSetup.svelte';
  import { ticketBranchName } from '../../utils/ticketBranch';
  import { userFacingError } from '../../utils/userFacingError';
  import { workProjects } from '../issues/ticketWorkGate';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import TrackerProjectSettings, { type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import TrackerUnreadBoards from './TrackerUnreadBoards.svelte';
  import { settingsComputer } from './settingsComputer';
  import { SELECT_CLASS } from './styles';
  import { trackerConnectionName } from './trackerConnectionStatus';

  const { backend } = settingsComputer();
  let connections = $derived(trackerConnectionsOf(backend).list);

  function readError(project: string): string | null {
    const statuses = trackerStatusesOf(project).error;
    return statuses !== null ? `The project's statuses could not be read: ${userFacingError(statuses)}` : null;
  }

  function siteName(place: DefaultsPlace): string {
    const connection = connections.find((c) => c.kind === place.tracker && c.site === place.site);
    return trackerConnectionName(connection ?? { label: '', kind: place.tracker, site: place.site });
  }

  /** The statuses the move picker of `place` offers, as the site lists
   * them. */
  function placeStatuses(project: string, place: StartPlace): StatusView[] {
    const site = trackerStatusesOf(project).sites?.find((s) => s.tracker === place.tracker && s.site === place.site);
    const ids = new Set(place.statuses);
    return (site?.statuses ?? []).filter((s) => ids.has(s.id));
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
  hold={acquireTrackerStatuses}
  {readError}
  ready={(project) => trackerStatusesOf(project).places !== null}
  retry={(project) => void refreshTrackerStatuses(project)}
  reading="Reading the defaults…"
>
  {#snippet body(page: TrackerProjectPage)}
    {@const places = trackerStatusesOf(page.project).places ?? []}
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

{#snippet placeRows(page: TrackerProjectPage, place: StartPlace, entry: StartWorkDefaults | null)}
  {@const locked = page.lockReason !== ''}
  {@const statuses = placeStatuses(page.project, place)}
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
          title={page.lockReason || undefined}
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
    {#if statuses.length === 0 && (place.unread ?? []).length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="tracker-start-work-unlisted">
        No board of this project lists {place.project} tickets, so there is no status to move them to. Add one in Boards.
      </p>
    {/if}
    <TrackerUnreadBoards boards={place.unread ?? []} project={page.project} />
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
