<script lang="ts">
  // Settings > Issues > Fields: the fields on top of every ticket of the
  // project and the sections open on every ticket (docs/specs/issues.md,
  // "Setup"), by Jira site. The sites are those of the project's boards
  // and those its pins name, worked out here from the boards, the
  // connections and the setup the page holds, so a pin saved elsewhere or
  // a connection removed shows at once with no read; each site reads its
  // own catalog (TrackerFieldsSite). A Linear ticket's fields are fixed,
  // so a Linear site lists nothing here.
  import type { Setup } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import { acquireTrackerProjectBoards, refreshTrackerProjectBoards, trackerProjectBoardsOf } from '../../stores/trackerProjectBoards.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import TrackerFieldsSite from './TrackerFieldsSite.svelte';
  import TrackerProjectSettings, { type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { settingsComputer } from './settingsComputer';
  import { trackerConnectionName } from './trackerConnectionStatus';

  const { backend } = settingsComputer();
  let connections = $derived(trackerConnectionsOf(backend).list);

  function readError(project: string): string | null {
    const boards = trackerProjectBoardsOf(backend, project).error;
    return boards !== null ? `The project's boards could not be read: ${userFacingError(boards)}` : null;
  }

  /** The Jira sites the page lists for `project`, in site order: each of
   * a board the project lists, and each the setup pins fields or opens
   * sections of. */
  function sitesOf(project: string, setup: Setup): string[] {
    const sites = new Set<string>();
    for (const board of trackerProjectBoardsOf(backend, project).list) {
      const connection = connections.find((c) => c.id === board.connection);
      if (connection?.kind === 'jira') sites.add(connection.site);
    }
    for (const pins of setup.ticket_pins ?? []) {
      if (pins.tracker === 'jira') sites.add(pins.site);
    }
    return [...sites].sort();
  }

  /** The connections that reach Jira site `site`, and whether each is on:
   * what its read is held under. */
  function holdsOf(site: string): string {
    return connections.filter((c) => c.kind === 'jira' && c.site === site).map((c) => `${c.id}:${c.enabled ? 'on' : 'off'}`).join(',');
  }

  function siteName(site: string): string {
    const connection = connections.find((c) => c.kind === 'jira' && c.site === site);
    return trackerConnectionName(connection ?? { label: '', kind: 'jira', site });
  }
</script>

<TrackerProjectSettings
  page="fields"
  projectField="tracker-fields.project"
  bodyField="tracker-fields.pins"
  heading="Fields and sections"
  hint="Choose the project whose tickets to lay out."
  what="pinned fields and sections"
  section="ticket_pins"
  hold={(project) => acquireTrackerProjectBoards(backend, project)}
  {readError}
  ready={(project) => trackerProjectBoardsOf(backend, project).loaded}
  retry={(project) => void refreshTrackerProjectBoards(backend, project)}
  reading="Reading the project's fields…"
>
  {#snippet body(page: TrackerProjectPage)}
    {@const sites = sitesOf(page.project, page.setup)}
    {#if sites.length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-fields-none">
        {#if trackerProjectBoardsOf(backend, page.project).list.length === 0}
          This project lists no boards. Add one in Boards to choose what its tickets show first.
        {:else}
          A Linear ticket's fields are fixed, so this project has no fields to pin. Its sections are pinned on the ticket.
        {/if}
      </p>
    {:else}
      {#each sites as site (site)}
        <TrackerFieldsSite {page} project={page.project} tracker="jira" {site} name={siteName(site)} holds={holdsOf(site)} />
      {/each}
    {/if}
    <p class="text-[0.71875rem] text-fg-hint">The same choices can be made in place, with the pin beside a field or a section on a ticket. What cards and backlog rows show is chosen with Display on the board.</p>
  {/snippet}
</TrackerProjectSettings>
