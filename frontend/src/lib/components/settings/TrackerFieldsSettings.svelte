<script lang="ts">
  // Settings > Issues > Fields: the fields on top of every ticket of the
  // project and the sections open on every ticket (docs/specs/issues.md,
  // "Setup"), by tracker site, the same pins as the pin beside a field or
  // a section on a ticket: each change goes through the ticket's own rule
  // (withTicketPin), and the sections listed are those a ticket lists
  // (ticketSections). The sites are those of the project's boards and
  // those its pins name; a site whose fields could not be read says why
  // and still lists its pins, by ID, to take off. A site's fields are its
  // tracker's whole catalog, several hundred on a real site: the pinned
  // show at once, and one that is not pinned is found by name, the exact
  // name first, at most MATCH_LIMIT drawn. A Linear ticket's fields are
  // fixed, so a Linear site lists nothing here.
  import type { PinField, Setup, SiteFields } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { chipClasses } from '../primitives/chipClasses';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import { acquireTrackerProjectBoards, refreshTrackerProjectBoards, trackerProjectBoardsOf } from '../../stores/trackerProjectBoards.svelte';
  import { acquireTrackerFields, refreshTrackerFields, trackerFieldsOf } from '../../stores/trackerSetup.svelte';
  import { isSection, ticketPinsOf, ticketSections, withTicketPin } from '../../stores/trackerTicketModel';
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

  /** A site's catalog as the page reads it: each field's label, its name
   * (with its ID when another field has the same name), and the fields
   * that are not sections, which a ticket's rail lists. */
  interface Catalog {
    readonly labels: ReadonlyMap<string, string>;
    readonly rail: readonly PinField[];
  }

  function catalogOf(site: SiteFields): Catalog {
    const named = new Map<string, number>();
    for (const f of site.fields) named.set(f.name, (named.get(f.name) ?? 0) + 1);
    const labels = new Map(site.fields.map((f) => [f.id, (named.get(f.name) ?? 0) > 1 ? `${f.name} (${f.id})` : f.name]));
    labels.set('description', 'Description');
    labels.set('links', 'Linked tickets');
    labels.set('attachments', 'Attachments');
    return { labels, rail: site.fields.filter((f) => !isSection(f)) };
  }

  interface Item {
    readonly id: string;
    readonly name: string;
  }

  /** The first `limit` of `items` whose name holds `words`, the exact name
   * first, then those that start with it, then the rest, each in the
   * catalog's order, and how many match in all. */
  function matching(items: Iterable<Item>, words: string, limit: number): { shown: Item[]; total: number } {
    const ranks: Item[][] = [[], [], []];
    let total = 0;
    for (const item of items) {
      const name = item.name.toLowerCase();
      const at = name.indexOf(words);
      if (at < 0) continue;
      total++;
      const rank = name === words ? 0 : at === 0 ? 1 : 2;
      if (ranks[rank].length < limit) ranks[rank].push(item);
    }
    return { shown: ranks.flat().slice(0, limit), total };
  }

  /** What a list shows: the pinned, by label in pin order (by ID when the
   * catalog has no such field), and the unpinned whose name holds `find`
   * (none while it is empty). */
  function listOf(setup: Setup, site: SiteFields, catalog: Catalog, kind: 'fields' | 'sections', find: string): { pinned: Item[]; matches: Item[]; more: number } {
    const pins = ticketPinsOf(setup, site.tracker, site.site);
    const words = find.trim().toLowerCase();
    const named = (id: string): Item => ({ id, name: catalog.labels.get(id) ?? id });
    let pinned: readonly string[];
    let rest: Iterable<Item>;
    if (kind === 'fields') {
      pinned = pins.fields ?? [];
      const on = new Set(pinned);
      rest = catalog.rail.filter((f) => !on.has(f.id)).map((f) => named(f.id));
    } else {
      // Every Jira ticket has a place for attachments (TicketScreen).
      const layout = ticketSections(site.fields, pins.sections, true);
      pinned = [...layout.shown, ...layout.missing];
      rest = layout.folded.map(named);
    }
    const found = words ? matching(rest, words, MATCH_LIMIT) : { shown: [], total: 0 };
    return { pinned: pinned.map(named), matches: found.shown, more: found.total - found.shown.length };
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
        {@const catalog = catalogOf(site)}
        <section class="flex flex-col gap-3" data-testid="tracker-fields-site" data-site={site.site}>
          <p class="text-[0.75rem] font-medium text-fg-muted">{siteName(site)}</p>
          {#if site.error}
            <p class="text-[0.75rem] text-error" data-testid="tracker-fields-site-error">Its fields could not be read: {site.error}</p>
          {/if}
          {@render pinList(page, site, catalog, 'fields', 'On top of every ticket', 'No field is pinned.', 'Find a field to pin')}
          {@render pinList(page, site, catalog, 'sections', 'Sections open on every ticket', 'No section is open.', 'Find a section to open')}
        </section>
      {/each}
    {/if}
    <p class="text-[0.71875rem] text-fg-hint">The same choices can be made in place, with the pin beside a field or a section on a ticket. What cards and backlog rows show is chosen with Display on the board.</p>
  {/snippet}
</TrackerProjectSettings>

{#snippet pinList(page: TrackerProjectPage, site: SiteFields, catalog: Catalog, kind: 'fields' | 'sections', title: string, none: string, findLabel: string)}
  {@const key = `${site.site}\u0000${kind}`}
  {@const find = finds[key] ?? ''}
  {@const list = listOf(page.setup, site, catalog, kind, find)}
  {@const locked = page.lockReason !== ''}
  <div class="flex flex-col gap-2" data-testid="tracker-fields-list" data-kind={kind}>
    <p class="text-[0.75rem] text-fg">{title}</p>
    {#if list.pinned.length > 0}
      <div class="flex flex-wrap gap-1.5">
        {#each list.pinned as item (item.id)}
          <button
            type="button"
            class="{chipClasses(true)} max-w-full"
            aria-pressed="true"
            aria-label="Unpin {item.name}"
            disabled={locked}
            title={page.lockReason || item.name}
            onclick={() => pin(page, site, kind, item.id, false)}
            data-testid="tracker-fields-pinned"
            data-id={item.id}
          ><span class="min-w-0 truncate">{item.name}</span></button>
        {/each}
      </div>
    {:else}
      <p class="text-[0.71875rem] text-fg-hint">{none}</p>
    {/if}
    {#if !site.error}
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
            {#each list.matches as item (item.id)}
              <button
                type="button"
                class="{chipClasses(false)} max-w-full"
                aria-pressed="false"
                aria-label="Pin {item.name}"
                disabled={locked}
                title={page.lockReason || item.name}
                onclick={() => pin(page, site, kind, item.id, true)}
                data-testid="tracker-fields-match"
                data-id={item.id}
              ><span class="min-w-0 truncate">{item.name}</span></button>
            {/each}
          </div>
          {#if list.more > 0}
            <p class="text-[0.71875rem] text-fg-hint" data-testid="tracker-fields-more">{list.more} more match; type more of the name.</p>
          {/if}
        {/if}
      {/if}
    {/if}
  </div>
{/snippet}
