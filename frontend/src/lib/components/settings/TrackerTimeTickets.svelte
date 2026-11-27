<script lang="ts">
  // The body of Settings > Issues > Time: the ticket meetings go on and
  // the ticket time off goes on, each found by key or title on the
  // connections of the project's Jira boards (TicketFinder) and saved at
  // once by its site and key, and Never log to an epic. Each ticket set
  // is checked through one connection of its site, every ticket of a
  // connection in one lookup (stores/trackerSetup's ticket check), which
  // gives its title and says a ticket that is gone or that the
  // connection cannot see.
  import { untrack } from 'svelte';
  import type { Setup, TimeTicket } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { BackendKey } from '../../transport/backendKey';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { trackerConnectionsOf } from '../../stores/trackerConnections.svelte';
  import { trackerProjectBoardsOf } from '../../stores/trackerProjectBoards.svelte';
  import {
    acquireTrackerTicketCheck,
    neverEpicsOn,
    refreshTrackerTicketCheck,
    trackerTicketCheckOf,
    withNeverEpics,
    withTimeTicket,
    type TimeTicketRole,
  } from '../../stores/trackerSetup.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import TicketFinder, { type FinderSource, type FoundTicket } from '../issues/TicketFinder.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import type { TrackerProjectPage } from './TrackerProjectSettings.svelte';
  import { GHOST_BUTTON_CLASS } from './styles';
  import { trackerConnectionName } from './trackerConnectionStatus';

  let { page, backend }: { page: TrackerProjectPage; backend: BackendKey } = $props();

  const ROWS: readonly { role: TimeTicketRole; title: string; text: string }[] = [
    { role: 'meetings', title: 'Meetings go on', text: 'Meetings that are not about one ticket.' },
    { role: 'time_off', title: 'Time off goes on', text: '' },
  ];

  const compact = $derived(isCompactLayout());
  const connections = $derived(trackerConnectionsOf(backend).list);
  const boards = $derived(trackerProjectBoardsOf(backend, page.project).list);

  /** The Jira connections of the project's boards, in board order, each
   * once. */
  const boardConnections = $derived.by(() => {
    const ids = [...new Set(boards.map((b) => b.connection))];
    return ids.flatMap((id) => connections.filter((c) => c.id === id && c.kind === 'jira'));
  });

  /** Where the finder looks: the boards' Jira connections turned on. */
  const sources = $derived<FinderSource[]>(
    boardConnections.filter((c) => c.enabled).map((c) => ({ backend, connection: c.id, name: trackerConnectionName(c) })),
  );

  function ticketOf(setup: Setup, role: TimeTicketRole): TimeTicket | null {
    return setup.time?.[role] ?? null;
  }

  /** The connection a ticket of `site` is checked through: a board's
   * turned on first, else any turned on that reaches the site. */
  function checkConnection(site: string): string | undefined {
    const reaches = (c: { kind: string; site: string; enabled: boolean }) => c.kind === 'jira' && c.site === site && c.enabled;
    return (boardConnections.find(reaches) ?? connections.find(reaches))?.id;
  }

  /** Each connection the tickets set are checked through, with their
   * keys in row order. */
  const checks = $derived.by(() => {
    const out = new Map<string, string[]>();
    for (const { role } of ROWS) {
      const ticket = ticketOf(page.setup, role);
      const connection = ticket ? checkConnection(ticket.site) : undefined;
      if (!ticket || !connection) continue;
      const keys = out.get(connection) ?? [];
      if (!keys.includes(ticket.key)) keys.push(ticket.key);
      out.set(connection, keys);
    }
    return [...out].map(([connection, keys]) => ({ connection, keys }));
  });

  // The holds follow the reads they name: a new setup object with the
  // same tickets keeps them.
  const heldAs = $derived(JSON.stringify([page.project, backend, checks]));
  $effect(() => {
    void heldAs;
    return untrack(() => {
      const releases = checks.map((c) => acquireTrackerTicketCheck(page.project, backend, c.connection, c.keys));
      return () => releases.forEach((release) => release());
    });
  });

  /** A ticket set, as its check shows it. */
  type Shown =
    | { readonly kind: 'unreachable' }
    | { readonly kind: 'checking' }
    | { readonly kind: 'failed'; readonly error: unknown; readonly retry: () => void }
    | { readonly kind: 'missing' }
    | { readonly kind: 'found'; readonly title: string };

  function shownOf(ticket: TimeTicket): Shown {
    const connection = checkConnection(ticket.site);
    if (!connection) return { kind: 'unreachable' };
    const keys = checks.find((c) => c.connection === connection)?.keys ?? [];
    const check = trackerTicketCheckOf(page.project, backend, connection, keys);
    if (check.error !== null) return { kind: 'failed', error: check.error, retry: () => void refreshTrackerTicketCheck(page.project, backend, connection, keys) };
    if (check.found === null) return { kind: 'checking' };
    const match = check.found.found?.find((m) => m.key === ticket.key);
    return match ? { kind: 'found', title: match.card.title } : { kind: 'missing' };
  }

  function siteName(site: string): string {
    return site.replace(/^https:\/\//, '');
  }

  // The finder open, and the button each row opens it from.
  let finding = $state<TimeTicketRole | null>(null);
  const anchors = $state<Partial<Record<TimeTicketRole, HTMLElement>>>({});

  function pick(role: TimeTicketRole, found: FoundTicket): void {
    void page.save((held) => {
      const site = connections.find((c) => c.id === found.connection)?.site;
      if (!site) throw new Error(`The connection that found ${found.key} was removed; choose the ticket again.`);
      return withTimeTicket(held, role, { tracker: 'jira', site, key: found.key });
    });
  }
</script>

{#if boardConnections.length === 0 && !ticketOf(page.setup, 'meetings') && !ticketOf(page.setup, 'time_off')}
  <p class="text-[0.75rem] text-fg-muted" data-testid="settings-tracker-time-none">
    {#if boards.length === 0}
      This project lists no boards. Add a Jira board in Boards to choose where its time goes.
    {:else}
      A Linear project has no time settings: time is logged on Jira.
    {/if}
  </p>
{:else}
  <ul class="flex flex-col gap-3">
    {#each ROWS as row (row.role)}
      {@const ticket = ticketOf(page.setup, row.role)}
      {@const shown = ticket ? shownOf(ticket) : null}
      <li class="flex min-w-0 flex-col gap-1" data-testid="tracker-time-ticket" data-role={row.role}>
        <div class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 {compact ? 'min-h-9' : ''}">
          <div class="flex min-w-0 flex-1 flex-col">
            <span class="text-[0.8125rem] text-fg">{row.title}</span>
            {#if row.text}<span class="text-[0.71875rem] text-fg-hint">{row.text}</span>{/if}
          </div>
          {#if ticket}
            <span class="flex min-w-0 items-baseline gap-1.5 text-[0.75rem]" data-testid="tracker-time-ticket-value">
              <span class="shrink-0 font-medium text-fg">{ticket.key}</span>
              {#if shown?.kind === 'found'}<span class="min-w-0 truncate text-fg-muted">{shown.title}</span>{/if}
            </span>
          {/if}
          <div class="flex shrink-0 items-center gap-1">
            <button
              type="button"
              class={GHOST_BUTTON_CLASS}
              bind:this={anchors[row.role]}
              disabled={page.lockReason !== '' || sources.length === 0}
              title={page.lockReason || (sources.length === 0 ? 'No Jira board of this project has a connection turned on.' : undefined)}
              onclick={() => { finding = row.role; }}
              data-testid="tracker-time-ticket-find"
            >{ticket ? 'Change' : 'Choose a ticket'}</button>
            {#if ticket}
              <button
                type="button"
                class={GHOST_BUTTON_CLASS}
                disabled={page.lockReason !== ''}
                title={page.lockReason || undefined}
                onclick={() => void page.save((held) => withTimeTicket(held, row.role, null))}
                data-testid="tracker-time-ticket-clear"
              >Clear</button>
            {/if}
          </div>
        </div>
        {#if ticket && shown}
          {#if shown.kind === 'unreachable'}
            <p class="text-[0.71875rem] text-warning" data-testid="tracker-time-ticket-note">No Jira connection turned on reaches {siteName(ticket.site)}. Turn one on in Connections, or choose another ticket.</p>
          {:else if shown.kind === 'missing'}
            <p class="text-[0.71875rem] text-warning" data-testid="tracker-time-ticket-note">{ticket.key} cannot be found on {siteName(ticket.site)}, or this connection cannot see it. Choose another ticket.</p>
          {:else if shown.kind === 'failed'}
            <div class="flex flex-wrap items-center gap-2">
              <p class="text-[0.71875rem] text-error" data-testid="tracker-time-ticket-note">{ticket.key} could not be checked: {userFacingError(shown.error)}</p>
              <button type="button" class={GHOST_BUTTON_CLASS} onclick={shown.retry} data-testid="tracker-time-ticket-retry">Try again</button>
            </div>
          {/if}
        {/if}
      </li>
    {/each}
    <li class="flex min-w-0 items-center gap-3 {compact ? 'min-h-9' : ''}" data-testid="tracker-time-epics">
      <div class="flex min-w-0 flex-1 flex-col">
        <span class="text-[0.8125rem] text-fg">Never log to an epic</span>
        <span class="text-[0.71875rem] text-fg-hint">Your agent's time draft puts no hours on an epic, only on a ticket under it.</span>
      </div>
      <ToggleSwitch
        checked={neverEpicsOn(page.setup)}
        ariaLabel="Never log to an epic"
        disabled={page.lockReason !== ''}
        title={page.lockReason || undefined}
        onToggle={(next) => void page.save((held) => withNeverEpics(held, next))}
      />
    </li>
  </ul>
  <TicketFinder
    anchor={finding ? anchors[finding] : undefined}
    open={finding !== null}
    {sources}
    onPick={(found) => { if (finding) pick(finding, found); }}
    onClose={() => { finding = null; }}
  />
{/if}
