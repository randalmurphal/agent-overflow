<script lang="ts">
  // Settings > Issues > Time: where the project's logged time goes
  // (docs/specs/issues.md, "Setup"): the ticket meetings go on, the
  // ticket time off goes on, and whether an agent's time draft may put
  // hours on an epic, the project's setup's time section
  // (trackerapp.TimeSettings). Time is logged on Jira, so the tickets are
  // found on the connections of the project's Jira boards
  // (TrackerTimeTickets).
  import { trackerProjectBoardsOf, acquireTrackerProjectBoards, refreshTrackerProjectBoards } from '../../stores/trackerProjectBoards.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import TrackerProjectSettings, { jiraOnly, type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import TrackerTimeTickets from './TrackerTimeTickets.svelte';
  import { settingsComputer } from './settingsComputer';

  const { backend } = settingsComputer();

  /** Why `project` has no Time settings: they apply only while it lists
   * a Jira board. */
  function none(project: string): string | null {
    return jiraOnly(trackerProjectBoardsOf(backend, project).list, 'to choose where its time goes', 'A Linear project has no time settings: time is logged on Jira.');
  }

  function readError(project: string): string | null {
    const boards = trackerProjectBoardsOf(backend, project).error;
    return boards !== null ? `The project's boards could not be read: ${userFacingError(boards)}` : null;
  }
</script>

<TrackerProjectSettings
  page="time"
  projectField="tracker-time.project"
  bodyField="tracker-time.logging"
  heading="Logging"
  hint="Choose the project whose time to set."
  what="time settings"
  section="time"
  hold={(project) => acquireTrackerProjectBoards(backend, project)}
  {readError}
  ready={(project) => trackerProjectBoardsOf(backend, project).loaded}
  {none}
  retry={(project) => void refreshTrackerProjectBoards(backend, project)}
  reading="Reading the project's time settings…"
>
  {#snippet body(page: TrackerProjectPage)}
    <TrackerTimeTickets {page} {backend} />
  {/snippet}
</TrackerProjectSettings>
