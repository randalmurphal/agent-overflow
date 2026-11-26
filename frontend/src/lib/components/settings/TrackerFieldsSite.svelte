<script lang="ts">
  // One Jira site of Settings > Issues > Fields: the fields on top of
  // every ticket and the sections open on every ticket, the same pins as
  // the pin beside a field or a section on a ticket (withTicketPin), the
  // sections those a ticket lists (ticketSections). It holds the site's
  // own read of its field catalog (stores/trackerSetup's site fields),
  // under `holds`, the connections that reach the site, so a change of
  // them reads it again; a read that failed says why with Try again, and
  // the pins still show, by ID, to take off. A real site's catalog holds
  // several hundred fields: the pinned show at once, and one that is not
  // pinned is found by name, only while a name is typed, the exact name
  // first, at most MATCH_LIMIT drawn.
  import type { PinField, Setup } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { untrack } from 'svelte';
  import { chipClasses } from '../primitives/chipClasses';
  import { acquireTrackerSiteFields, refreshTrackerSiteFields, trackerSiteFieldsOf } from '../../stores/trackerSetup.svelte';
  import { isSection, ticketPinsOf, ticketSections, withTicketPin } from '../../stores/trackerTicketModel';
  import { userFacingError } from '../../utils/userFacingError';
  import type { TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { GHOST_BUTTON_CLASS, INPUT_CLASS } from './styles';

  /** The most matches of a name drawn at once. */
  const MATCH_LIMIT = 20;

  let { page, project, tracker, site, name, holds }: {
    page: TrackerProjectPage;
    project: string;
    tracker: string;
    site: string;
    /** The site as the page names it. */
    name: string;
    /** The connections that reach the site, as the read is held under. */
    holds: string;
  } = $props();

  // The hold follows the read it names, not the props' getters: a parent
  // that passes a field of a new object at each change of the setup must
  // not release the read and take it again.
  const heldAs = $derived([project, tracker, site, holds].join('\u0000'));
  $effect(() => {
    void heldAs;
    return untrack(() => acquireTrackerSiteFields(project, tracker, site, holds));
  });

  const read = $derived(trackerSiteFieldsOf(project, tracker, site, holds));
  // The catalog is built once per answer: `read` is a new object at each
  // change of the props, its fields the same array until the site is read
  // again.
  const answered = $derived(read.fields);
  const catalog = $derived(catalogOf(answered ?? []));

  // The name typed in each list's find box.
  let finds = $state({ fields: '', sections: '' });

  /** A field as a list shows it: its name, and its ID when another field
   * of the site has the same name. */
  interface Item {
    readonly id: string;
    readonly name: string;
    readonly shared: boolean;
  }

  /** A site's catalog as the page reads it: each field as a list shows
   * it, by ID, the fields that are not sections, which a ticket's rail
   * lists, and those that are. */
  interface Catalog {
    readonly items: ReadonlyMap<string, Item>;
    readonly rail: readonly PinField[];
    readonly sections: readonly PinField[];
  }

  const BUILT_IN: Record<string, string> = { description: 'Description', links: 'Linked tickets', attachments: 'Attachments' };

  function catalogOf(fields: readonly PinField[]): Catalog {
    const named = new Map<string, number>();
    for (const f of fields) named.set(f.name, (named.get(f.name) ?? 0) + 1);
    const items = new Map<string, Item>(Object.entries(BUILT_IN).map(([id, n]) => [id, { id, name: n, shared: false }]));
    for (const f of fields) items.set(f.id, { id: f.id, name: f.name, shared: (named.get(f.name) ?? 0) > 1 });
    return { items, rail: fields.filter((f) => !isSection(f)), sections: fields.filter(isSection) };
  }

  function itemOf(id: string): Item {
    return catalog.items.get(id) ?? { id, name: id, shared: false };
  }

  /** The first `limit` of `ids` not in `skip` whose name holds `words`:
   * names that are `words` first, then those that start with it, then the
   * rest, each in the catalog's order, and how many match in all. The
   * ranking reads the name, never the ID a shared name shows. */
  function matching(ids: Iterable<string>, skip: ReadonlySet<string>, words: string, limit: number): { shown: Item[]; total: number } {
    const ranks: Item[][] = [[], [], []];
    let total = 0;
    for (const id of ids) {
      if (skip.has(id)) continue;
      const item = itemOf(id);
      const lower = item.name.toLowerCase();
      const at = lower.indexOf(words);
      if (at < 0) continue;
      total++;
      const rank = lower === words ? 0 : at === 0 ? 1 : 2;
      if (ranks[rank].length < limit) ranks[rank].push(item);
    }
    return { shown: ranks.flat().slice(0, limit), total };
  }

  function* railIDs(): Generator<string> {
    for (const f of catalog.rail) yield f.id;
  }

  /** What a list shows: the pinned in pin order (by ID when the catalog
   * has no such field), and, only while a name is typed, the unpinned
   * whose name holds it. */
  function listOf(setup: Setup, kind: 'fields' | 'sections', find: string): { pinned: Item[]; matches: Item[]; more: number } {
    const pins = ticketPinsOf(setup, tracker, site);
    const words = find.trim().toLowerCase();
    let pinned: readonly string[];
    let rest: () => Iterable<string>;
    if (kind === 'fields') {
      pinned = pins.fields ?? [];
      rest = railIDs;
    } else {
      // Every Jira ticket has a place for attachments (TicketScreen).
      const layout = ticketSections(catalog.sections, pins.sections, true);
      pinned = [...layout.shown, ...layout.missing];
      rest = () => layout.folded;
    }
    const found = words ? matching(rest(), new Set(pinned), words, MATCH_LIMIT) : { shown: [], total: 0 };
    return { pinned: pinned.map(itemOf), matches: found.shown, more: found.total - found.shown.length };
  }

  function pin(kind: 'fields' | 'sections', id: string, on: boolean): void {
    void page.save((held) => withTicketPin(held, tracker, site, kind, id, on, true));
  }
</script>

<section class="flex flex-col gap-3" data-testid="tracker-fields-site" data-site={site}>
  <p class="text-[0.75rem] font-medium text-fg-muted">{name}</p>
  {#if read.error !== null}
    <div class="flex flex-wrap items-center gap-2">
      <p class="text-[0.75rem] text-error" data-testid="tracker-fields-site-error">Its fields could not be read: {userFacingError(read.error)}</p>
      <button type="button" class={GHOST_BUTTON_CLASS} onclick={() => void refreshTrackerSiteFields(project, tracker, site, holds)} data-testid="tracker-fields-site-retry">Try again</button>
    </div>
  {:else if read.fields === null}
    <p class="text-[0.71875rem] text-fg-hint" data-testid="tracker-fields-site-reading">Reading its fields…</p>
  {/if}
  {@render pinList('fields', 'On top of every ticket', 'No field is pinned.', 'Find a field to pin')}
  {@render pinList('sections', 'Sections open on every ticket', 'No section is open.', 'Find a section to open')}
</section>

{#snippet chip(kind: 'fields' | 'sections', item: Item, on: boolean)}
  <button
    type="button"
    class="{chipClasses(on)} max-w-full"
    aria-pressed={on}
    aria-label="{on ? 'Unpin' : 'Pin'} {item.name}{item.shared ? ` (${item.id})` : ''}"
    disabled={page.lockReason !== ''}
    title={page.lockReason || (item.shared ? `${item.name} (${item.id})` : item.name)}
    onclick={() => pin(kind, item.id, !on)}
    data-testid={on ? 'tracker-fields-pinned' : 'tracker-fields-match'}
    data-id={item.id}
  ><span class="min-w-0 truncate">{item.name}</span>{#if item.shared}<span class="shrink-0 text-fg-hint">({item.id})</span>{/if}</button>
{/snippet}

{#snippet pinList(kind: 'fields' | 'sections', title: string, none: string, findLabel: string)}
  {@const find = finds[kind]}
  {@const list = listOf(page.setup, kind, find)}
  <div class="flex flex-col gap-2" data-testid="tracker-fields-list" data-kind={kind}>
    <p class="text-[0.75rem] text-fg">{title}</p>
    {#if list.pinned.length > 0}
      <div class="flex flex-wrap gap-1.5">
        {#each list.pinned as item (item.id)}
          {@render chip(kind, item, true)}
        {/each}
      </div>
    {:else}
      <p class="text-[0.71875rem] text-fg-hint">{none}</p>
    {/if}
    {#if read.fields !== null}
      <input
        class={INPUT_CLASS}
        type="search"
        aria-label={findLabel}
        placeholder={findLabel}
        value={find}
        oninput={(e) => { finds[kind] = (e.currentTarget as HTMLInputElement).value; }}
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
              {@render chip(kind, item, false)}
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
