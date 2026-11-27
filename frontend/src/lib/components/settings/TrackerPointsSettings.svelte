<script lang="ts">
  // Settings > Issues > Points and credit: how work is counted on the
  // project's dashboards and in sprint planning (docs/specs/issues.md,
  // "Setup"), the project's setup's estimates section
  // (trackerapp.Estimates), which applies to its Jira boards. What a
  // ticket is worth picks the boards' estimates or a count of tickets,
  // which every total, the pace and the dashboards of the project follow
  // (BoardView.counts); Usual pace is the number sprint planning measures
  // against, read from this computer's copy of each Jira board
  // (ProjectTrackerPace); Planning poker is on until turned off, and off
  // the project's tickets offer none. Credit is not built, so the page
  // has no Credit group.
  import { tick } from 'svelte';
  import type { BoardPace } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { measureOf } from '../../stores/trackerBacklogModel';
  import { acquireTrackerProjectBoards, refreshTrackerProjectBoards, trackerProjectBoardsOf } from '../../stores/trackerProjectBoards.svelte';
  import {
    acquireTrackerPace,
    countsTickets,
    pokerOn,
    refreshTrackerPace,
    trackerPaceOf,
    trackerSetupOf,
    withCountTickets,
    withPoker,
  } from '../../stores/trackerSetup.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { formatEstimateSum } from '../issues/boardFormat';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import TrackerProjectSettings, { jiraOnly, type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { settingsComputer } from './settingsComputer';
  import { SELECT_CLASS } from './styles';

  const { backend } = settingsComputer();
  const compact = $derived(isCompactLayout());

  function hold(project: string): () => void {
    const boards = acquireTrackerProjectBoards(backend, project);
    const pace = acquireTrackerPace(project);
    return () => {
      boards();
      pace();
    };
  }

  function readError(project: string): string | null {
    const boards = trackerProjectBoardsOf(backend, project).error;
    if (boards !== null) return `The project's boards could not be read: ${userFacingError(boards)}`;
    const pace = trackerPaceOf(project).error;
    return pace !== null ? `The boards' pace could not be read: ${userFacingError(pace)}` : null;
  }

  /** The pick of the boards' estimates, named as the boards estimate:
   * story points when every Jira board read estimates in points. */
  function estimatesName(boards: readonly BoardPace[]): string {
    const read = boards.filter((b) => b.read);
    return read.length > 0 && read.every((b) => b.estimate === 'points') ? 'Story points' : "The board's estimates";
  }

  /** One board's usual pace as the page says it, in the unit the project
   * counts it in; the board named when the project has several. */
  function paceLine(b: BoardPace, named: boolean): string {
    const name = b.name || `Board ${b.board}`;
    if (!b.read) return `Read when ${name} is opened in Issues.`;
    if (!b.pace) return `No sprint of ${name} has finished yet.`;
    const n = measureOf(b.pace, b.counts);
    const amount =
      b.counts === 'points'
        ? `${formatEstimateSum(n, 'points') ?? '0'} ${n === 1 ? 'point' : 'points'}`
        : b.counts === 'time'
          ? `${formatEstimateSum(n, 'time') ?? '0'}`
          : `${Math.round(n * 10) / 10} ${n === 1 ? 'ticket' : 'tickets'}`;
    const from = b.pace.sprints === 1 ? 'the last sprint' : `the last ${b.pace.sprints} sprints`;
    return `${named ? `${name}: ` : ''}${amount} a sprint, from ${from}.`;
  }

  /** Saves the pick; the picker then shows what is stored, since a
   * refused or failed save moves no value of the page to set it back. */
  async function pick(page: TrackerProjectPage, control: HTMLSelectElement): Promise<void> {
    const tickets = control.value === 'tickets';
    const shown = await page.save((held) => withCountTickets(held, tickets));
    await tick();
    const stored = trackerSetupOf(page.project).view?.setup;
    if (shown && stored && control.isConnected) control.value = countsTickets(stored) ? 'tickets' : 'estimates';
  }
</script>

<TrackerProjectSettings
  page="points"
  projectField="tracker-points.project"
  bodyField="tracker-points.estimates"
  heading="Estimates"
  hint="Choose the project whose counting to set."
  what="points and credit settings"
  section="estimates"
  {hold}
  {readError}
  ready={(project) => trackerProjectBoardsOf(backend, project).loaded && trackerPaceOf(project).boards !== null}
  none={(project) => jiraOnly(trackerProjectBoardsOf(backend, project).list, 'to set how its work is counted', 'A Linear project has no points settings: they are for Jira boards.')}
  retry={(project) => {
    void refreshTrackerProjectBoards(backend, project);
    void refreshTrackerPace(project);
  }}
  reading="Reading the project's boards…"
>
  {#snippet body(page: TrackerProjectPage)}
    {@const boards = trackerPaceOf(page.project).boards ?? []}
    {@const planning = boards.filter((b) => !b.read || b.plans_sprints)}
    {@const locked = page.lockReason !== ''}
    <ul class="flex flex-col gap-3">
      <li class="flex min-w-0 gap-x-3 gap-y-2 {compact ? 'flex-col' : 'items-center'}" data-testid="tracker-points-worth">
        <div class="flex min-w-0 flex-1 flex-col">
          <span class="text-[0.8125rem] text-fg">What a ticket is worth</span>
          <span class="text-[0.71875rem] text-fg-hint">Story points are read from the board. A board with no estimates counts tickets.</span>
        </div>
        <select
          class={SELECT_CLASS}
          aria-label="What a ticket is worth"
          value={countsTickets(page.setup) ? 'tickets' : 'estimates'}
          disabled={locked}
          title={page.lockReason || undefined}
          onchange={(e) => void pick(page, e.currentTarget as HTMLSelectElement)}
          data-testid="tracker-points-count"
        >
          <option value="estimates">{estimatesName(boards)}</option>
          <option value="tickets">Count tickets</option>
        </select>
      </li>
      <li class="flex min-w-0 flex-col gap-0.5 {compact ? 'min-h-9' : ''}" data-testid="tracker-points-pace">
        <span class="text-[0.8125rem] text-fg">Usual pace</span>
        {#if planning.length === 0}
          <span class="text-[0.71875rem] text-fg-hint" data-testid="tracker-points-pace-line">No board of this project plans sprints.</span>
        {:else}
          {#each planning as b (`${b.connection}/${b.board}`)}
            <span class="text-[0.71875rem] text-fg-hint" data-testid="tracker-points-pace-line">{paceLine(b, planning.length > 1)}</span>
          {/each}
          <span class="text-[0.71875rem] text-fg-hint">Sprint planning measures against it.</span>
        {/if}
      </li>
      <li class="flex min-w-0 items-center gap-3 {compact ? 'min-h-9' : ''}" data-testid="tracker-points-poker">
        <div class="flex min-w-0 flex-1 flex-col">
          <span class="text-[0.8125rem] text-fg">Planning poker</span>
          <span class="text-[0.71875rem] text-fg-hint">Vote from the ticket through the site's poker app. It relies on calls that app does not document, so if they stop working only poker is affected.</span>
        </div>
        <ToggleSwitch
          checked={pokerOn(page.setup)}
          ariaLabel="Planning poker"
          disabled={locked}
          title={page.lockReason || undefined}
          onToggle={(next) => void page.save((held) => withPoker(held, next))}
        />
      </li>
    </ul>
  {/snippet}
</TrackerProjectSettings>
