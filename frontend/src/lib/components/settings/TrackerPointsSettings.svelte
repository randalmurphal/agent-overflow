<script lang="ts">
  // Settings > Issues > Points and credit: how work is counted on the
  // project's dashboards and in sprint planning (docs/specs/issues.md,
  // "Setup"), the project's setup's estimates section
  // (trackerapp.Estimates), which applies to its Jira boards. What a
  // ticket is worth picks the boards' estimates or a count of tickets,
  // which every total, the pace and the dashboards of the project follow
  // (BoardView.counts); Usual pace is the number sprint planning measures
  // against, read from this computer's copy of each Jira board. One read
  // (ProjectTrackerPace) lists the project's boards, which say whether
  // the page applies, with each Jira board's pace; Planning poker is on until turned off, and off
  // the project's tickets offer none. The Credit group
  // (TrackerCreditSettings) is the setup's credit section, whose own
  // newer version it says in place of its rows.
  import { tick } from 'svelte';
  import type { BoardPace } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { measureOf } from '../../stores/trackerBacklogModel';
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
  import { formatPace } from '../issues/boardFormat';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import TrackerCreditSettings from './TrackerCreditSettings.svelte';
  import TrackerProjectSettings, { jiraOnly, type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { SELECT_CLASS } from './styles';

  const compact = $derived(isCompactLayout());

  function readError(project: string): string | null {
    const err = trackerPaceOf(project).error;
    return err !== null ? `The project's boards could not be read: ${userFacingError(err)}` : null;
  }

  /** The project's Jira boards, which the page's settings are for. */
  function jiraBoards(project: string): BoardPace[] {
    return (trackerPaceOf(project).boards ?? []).filter((b) => b.connection_kind === 'jira');
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
    const amount = formatPace(measureOf(b.pace, b.counts), b.counts);
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
  hold={acquireTrackerPace}
  {readError}
  ready={(project) => trackerPaceOf(project).boards !== null}
  none={(project) => jiraOnly(trackerPaceOf(project).boards ?? [], 'to set how its work is counted', 'A Linear project has no points settings: they are for Jira boards.')}
  retry={(project) => void refreshTrackerPace(project)}
  reading="Reading the project's boards…"
>
  {#snippet body(page: TrackerProjectPage)}
    {@const boards = jiraBoards(page.project)}
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
    <TrackerCreditSettings {page} {boards} />
  {/snippet}
</TrackerProjectSettings>
