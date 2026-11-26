<script lang="ts">
  // Settings > Issues > Fields: the fields on top of every ticket of the
  // project and the sections open on every ticket (docs/specs/issues.md,
  // "Setup"), by tracker site, the same pins as the pin beside a field or
  // a section on a ticket: each change goes through the ticket's own rule
  // (withTicketPin), and what each lists is what a ticket lists
  // (railFields, ticketSections). A site's fields are its tracker's whole
  // catalog, several hundred on a real site: the pinned show at once, and
  // one that is not pinned is found by name, at most MATCH_LIMIT drawn. A
  // Linear ticket's fields are fixed, so a Linear site lists nothing here.
  import type { FieldView, Setup, SiteFields } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { chipClasses } from '../primitives/chipClasses';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import { acquireTrackerProjectBoards, refreshTrackerProjectBoards, trackerProjectBoardsOf } from '../../stores/trackerProjectBoards.svelte';
  import { acquireTrackerFields, refreshTrackerFields, trackerFieldsOf } from '../../stores/trackerSetup.svelte';
  import { railFields, ticketPinsOf, ticketSections, withTicketPin } from '../../stores/trackerTicketModel';
  import { userFacingError } from '../../utils/userFacingError';
  import TrackerProjectSettings, { type TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { settingsComputer } from './settingsComputer';
  import { INPUT_CLASS } from './styles';
  import { trackerConnectionName } from './trackerConnectionStatus';

  /** The most matches of a name drawn at once. */
  const MATCH_LIMIT = 20;

  const { backend } = settingsComputer();
  let connections = $derived(trackerConnectionsOf(backend).list);

  function hold(project: string): () => void {
    const boards = acquireTrackerProjectBoards(backend, project);
    const fields = acquireTrackerFields(project);
    return () => {
      boards();
      fields();
    };
  }

  function readError(project: string): string | null {
    const boards = trackerProjectBoardsOf(backend, project).error;
    if (boards !== null) return `The project's boards could not be read: ${userFacingError(boards)}`;
    const fields = trackerFieldsOf(project).error;
    return fields !== null ? `The project's fields could not be read from its tracker: ${userFacingError(fields)}` : null;
  }

  // The name typed in each list's find box, by site and list.
  let finds = $state<Record<string, string>>({});

  function siteName(site: SiteFields): string {
    const connection = connections.find((c) => c.kind === site.tracker && c.site === site.site);
    return trackerConnectionName(connection ?? { label: '', kind: site.tracker, site: site.site });
  }

  /** A section's name as a ticket names it. */
  function sectionName(fields: readonly FieldView[], id: string): string {
    if (id === 'description') return 'Description';
    if (id === 'links') return 'Linked tickets';
    if (id === 'attachments') return 'Attachments';
    return fields.find((f) => f.id === id)?.name ?? id;
  }

  /** What a list shows: the pinned, by ID and name in pin order, and the
   * unpinned whose name holds `find` (none while it is empty). */
  function listOf(setup: Setup, site: SiteFields, kind: 'fields' | 'sections', find: string) {
    const pins = ticketPinsOf(setup, site.tracker, site.site);
    const words = find.trim().toLowerCase();
    if (kind === 'fields') {
      const rail = railFields(site.fields, pins.fields, words);
      return {
        pinned: [...rail.pinned.map((f) => ({ id: f.id, name: f.name })), ...rail.missing.map((id) => ({ id, name: id }))],
        matches: words ? rail.more.map((f) => ({ id: f.id, name: f.name })) : [],
      };
    }
    // Every Jira ticket has a place for attachments (TicketScreen).
    const layout = ticketSections(site.fields, pins.sections, true);
    const named = (id: string) => ({ id, name: sectionName(site.fields, id) });
    return {
      pinned: [...layout.shown, ...layout.missing].map(named),
      matches: words ? layout.folded.map(named).filter((s) => s.name.toLowerCase().includes(words)) : [],
    };
  }

  function pin(page: TrackerProjectPage, site: SiteFields, kind: 'fields' | 'sections', id: string, on: boolean): void {
    void page.save((held) => withTicketPin(held, site.tracker, site.site, kind, id, on, true));
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
  {hold}
  {readError}
  ready={(project) => trackerProjectBoardsOf(backend, project).loaded && trackerFieldsOf(project).sites !== null}
  retry={(project) => {
    void refreshTrackerProjectBoards(backend, project);
    void refreshTrackerFields(project);
  }}
  reading="Reading the project's fields…"
>
  {#snippet body(page: TrackerProjectPage)}
    {@const sites = trackerFieldsOf(page.project).sites ?? []}
    {@const boards = trackerProjectBoardsOf(backend, page.project).list}
    {#if sites.length === 0}
      <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-fields-none">
        {#if boards.length === 0}
          This project lists no boards. Add one in Boards to choose what its tickets show first.
        {:else}
          A Linear ticket's fields are fixed, so this project has no fields to pin. Its sections are pinned on the ticket.
        {/if}
      </p>
    {:else}
      {#each sites as site (`${site.tracker}\u0000${site.site}`)}
        <section class="flex flex-col gap-3" data-testid="tracker-fields-site" data-site={site.site}>
          <p class="text-[0.75rem] font-medium text-fg-muted">{siteName(site)}</p>
          {@render pinList(page, site, 'fields', 'On top of every ticket', 'No field is pinned.', 'Find a field to pin')}
          {@render pinList(page, site, 'sections', 'Sections open on every ticket', 'No section is open.', 'Find a section to open')}
        </section>
      {/each}
    {/if}
    <p class="text-[0.71875rem] text-fg-hint">The same choices can be made in place, with the pin beside a field or a section on a ticket. What cards and backlog rows show is chosen with Display on the board.</p>
  {/snippet}
</TrackerProjectSettings>

{#snippet pinList(page: TrackerProjectPage, site: SiteFields, kind: 'fields' | 'sections', title: string, none: string, findLabel: string)}
  {@const key = `${site.site}\u0000${kind}`}
  {@const find = finds[key] ?? ''}
  {@const list = listOf(page.setup, site, kind, find)}
  {@const locked = page.lockReason !== ''}
  <div class="flex flex-col gap-2" data-testid="tracker-fields-list" data-kind={kind}>
    <p class="text-[0.75rem] text-fg">{title}</p>
    {#if list.pinned.length > 0}
      <div class="flex flex-wrap gap-1.5">
        {#each list.pinned as item (item.id)}
          <button
            type="button"
            class={chipClasses(true)}
            aria-pressed="true"
            aria-label="Unpin {item.name}"
            disabled={locked}
            title={page.lockReason || undefined}
            onclick={() => pin(page, site, kind, item.id, false)}
            data-testid="tracker-fields-pinned"
            data-id={item.id}
          >{item.name}</button>
        {/each}
      </div>
    {:else}
      <p class="text-[0.71875rem] text-fg-hint">{none}</p>
    {/if}
    <input
      class={INPUT_CLASS}
      type="search"
      aria-label={findLabel}
      placeholder={findLabel}
      value={find}
      oninput={(e) => { finds[key] = (e.currentTarget as HTMLInputElement).value; }}
      spellcheck="false"
      autocomplete="off"
      data-testid="tracker-fields-find"
    />
    {#if find.trim()}
      {#if list.matches.length === 0}
        <p class="text-[0.71875rem] text-fg-hint">Nothing else is named like that.</p>
      {:else}
        <div class="flex flex-wrap gap-1.5">
          {#each list.matches.slice(0, MATCH_LIMIT) as item (item.id)}
            <button
              type="button"
              class={chipClasses(false)}
              aria-pressed="false"
              aria-label="Pin {item.name}"
              disabled={locked}
              title={page.lockReason || undefined}
              onclick={() => pin(page, site, kind, item.id, true)}
              data-testid="tracker-fields-match"
              data-id={item.id}
            >{item.name}</button>
          {/each}
        </div>
        {#if list.matches.length > MATCH_LIMIT}
          <p class="text-[0.71875rem] text-fg-hint" data-testid="tracker-fields-more">{list.matches.length - MATCH_LIMIT} more match; type more of the name.</p>
        {/if}
      {/if}
    {/if}
  </div>
{/snippet}
