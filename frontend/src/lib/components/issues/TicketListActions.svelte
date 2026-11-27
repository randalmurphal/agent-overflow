<script lang="ts" module>
  import type { BackendKey } from '../../transport/backendKey';

  /** Where a ticket list's rows are written and threads made, and why an
   * action cannot run ('' when it can). */
  export interface TicketListSource {
    readonly backend: BackendKey;
    readonly connection: string;
    /** The project a thread on the selection is made in. */
    readonly project: string;
    /** Why Move to and Assign cannot run. */
    readonly writeBlocked: string;
    /** Why Start a thread cannot run. */
    readonly threadBlocked: string;
  }
</script>

<script lang="ts">
  // A dashboard's ticket list, its widget's or its drill's
  // (docs/specs/issues.md, "Dashboards"): each row opens its ticket, and
  // its box selects it. Selecting shows every row's box and the bar: Move
  // to, Assign and Start a thread on the selected tickets, and clear. The
  // selection is by ticket ID and keeps only the tickets the rows list, so
  // a count again keeps the rows still listed, and closes an open action
  // whose tickets it changed, which acts only on the tickets it opened
  // for. Each move and assignment is
  // the ticket write store's write of that ticket, and what became of it
  // shows under its row (or in a toast while the list is not shown); the
  // bar then says what the batch came to for a few seconds. On desktop a
  // row's box shows on hover and focus, and on every row while selecting;
  // under compact it shows always.
  import { onDestroy, untrack, type Snippet } from 'svelte';
  import { SvelteMap } from 'svelte/reactivity';
  import Check from '@lucide/svelte/icons/check';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import MessageSquare from '@lucide/svelte/icons/message-square';
  import X from '@lucide/svelte/icons/x';
  import type { TicketRow } from '../../../../bindings/agent-overflow/internal/trackerdash/models';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { askAboutTickets } from '../../stores/ticketWork.svelte';
  import { trackerTicketKey } from '../../stores/trackerTicket.svelte';
  import { fieldTarget, ticketWrites } from '../../stores/trackerTicketWrites.svelte';
  import { TOAST_MS } from '../../pageTiming';
  import { DEFAULT_MAX_ATTACHMENT_COUNT } from '../../types/attachment';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import TicketPicker, { type PickItem } from './TicketPicker.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import { cardFieldName } from './ticketEditors';
  import {
    assignablePeople,
    assignTickets,
    commonMoves,
    moveTickets,
    readMoves,
    type BatchOutcome,
    type ListStatus,
    type ListTicket,
  } from './ticketSelection';

  interface Props {
    rows: readonly TicketRow[];
    source: TicketListSource;
    /** The widget's list or the drill's, which lay out their rows apart. */
    variant: 'widget' | 'drill';
    /** The list's and each row's test ID. */
    listTestId: string;
    rowTestId: string;
    onTicket: (row: TicketRow) => void;
    /** What a row shows after its box. */
    row: Snippet<[TicketRow]>;
    /** What the list shows with no rows. */
    empty?: Snippet;
    /** A thread on the selection was made, and opens next. */
    onThread?: () => void;
  }

  let { rows, source, variant, listTestId, rowTestId, onTicket, row, empty, onThread }: Props = $props();

  /** A thread carries each ticket as one attachment of its first message. */
  const MAX_THREAD_TICKETS = DEFAULT_MAX_ATTACHMENT_COUNT;
  const NOTE_TARGETS = ['status', fieldTarget('assignee')];

  let chosen = $state.raw<ReadonlySet<string>>(new Set());
  // A count again keeps the tickets still listed.
  $effect.pre(() => {
    const listed = new Set(rows.map((r) => r.id));
    if ([...chosen].some((id) => !listed.has(id))) chosen = new Set([...chosen].filter((id) => listed.has(id)));
  });
  const selected = $derived(uniqueTickets(rows.filter((r) => chosen.has(r.id))));
  const selecting = $derived(selected.length > 0);

  function uniqueTickets(list: readonly TicketRow[]): TicketRow[] {
    const seen = new Set<string>();
    return list.filter((r) => !seen.has(r.id) && !!seen.add(r.id));
  }

  function toggle(r: TicketRow): void {
    const next = new Set(chosen);
    if (!next.delete(r.id)) next.add(r.id);
    chosen = next;
  }

  // The status each ticket this list moved is in now, by ticket ID,
  // written as each ticket's move settles, until the rows are counted
  // again: a drill's rows show their tickets as they were when it counted,
  // so its next move starts from where this one left them. One status
  // write is out per ticket at a time (the write store), so a ticket's
  // later settle is its later move.
  const movedTo = new SvelteMap<string, string>();
  $effect.pre(() => {
    void rows;
    untrack(() => movedTo.clear());
  });
  const statusOf = (r: TicketRow) => movedTo.get(r.id) ?? r.status;

  function listTicket(r: TicketRow): ListTicket {
    const ref = { backend: source.backend, connection: source.connection, issue: r.id };
    return { ctx: { key: trackerTicketKey(ref), ref, label: r.key }, status: statusOf(r) };
  }
  const keyOf = (r: TicketRow) => trackerTicketKey({ backend: source.backend, connection: source.connection, issue: r.id });

  function many(n: number): string {
    return n === 1 ? '1 ticket' : `${n} tickets`;
  }

  // ---- The note after an action ----

  let note = $state<{ readonly text: string; readonly done: boolean } | null>(null);
  let noteTimer: ReturnType<typeof setTimeout> | undefined;
  function say(text: string, done: boolean): void {
    clearTimeout(noteTimer);
    note = { text, done };
    if (done) noteTimer = setTimeout(() => { note = null; }, TOAST_MS);
  }
  onDestroy(() => clearTimeout(noteTimer));

  function madeWords(out: BatchOutcome, verb: string): string {
    return out.made === out.sent ? `${many(out.sent)} ${verb}` : `${out.made} of ${many(out.sent)} ${verb}`;
  }

  // ---- The actions ----

  type Open = 'move' | 'assign' | 'thread' | null;
  let open = $state<Open>(null);
  // The tickets the open action is for, taken when it opened, and what it
  // is for (`actingFor`): their IDs, and for Move to the statuses it
  // planned from too; its reads stop when it ends (`reads`).
  let acting = $state.raw<ListTicket[]>([]);
  let actingFor = '';
  let reads: AbortController | null = null;
  function actionFor(which: Exclude<Open, null>): string {
    return selected.map((r) => (which === 'move' ? `${r.id}\t${statusOf(r)}` : r.id)).join('\n');
  }
  // A count again that changed the selection, or a status Move to planned
  // from, ends the action it opened for.
  $effect.pre(() => {
    if (open && actionFor(open) !== actingFor) untrack(endAction);
  });
  onDestroy(() => reads?.abort());
  let moveAnchor: HTMLElement | undefined = $state();
  let assignAnchor: HTMLElement | undefined = $state();
  let threadAnchor: HTMLElement | undefined = $state();

  type MovesShown = { readonly state: 'reading' } | { readonly state: 'read'; readonly statuses: ListStatus[] } | { readonly state: 'failed'; readonly failed: readonly string[] };
  let moves = $state.raw<MovesShown>({ state: 'reading' });
  let movesRead = 0;

  function begin(which: Exclude<Open, null>): void {
    endAction();
    acting = selected.map(listTicket);
    actingFor = actionFor(which);
    reads = new AbortController();
    if (which === 'thread' && draft.for !== actingFor) draft = { for: actingFor, text: '' };
    open = which;
    if (which !== 'move') return;
    const tickets = acting;
    const read = ++movesRead;
    void readMoves(tickets, reads.signal).then((got) => {
      if (read !== movesRead) return;
      moves = 'failed' in got ? { state: 'failed', failed: got.failed } : { state: 'read', statuses: commonMoves(tickets, got.moves) };
    });
  }

  /** Ends the open action: its reads stop, and it holds no tickets. */
  function endAction(): void {
    open = null;
    reads?.abort();
    reads = null;
    acting = [];
    actingFor = '';
    movesRead += 1;
    moves = { state: 'reading' };
  }

  function close(reason?: PopoverCloseReason, anchor?: HTMLElement): void {
    endAction();
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  async function move(status: ListStatus): Promise<void> {
    const tickets = acting;
    endAction();
    chosen = new Set();
    const already = tickets.filter((t) => status.moves.get(t.ctx.key) === null).length;
    say(`Moving ${many(tickets.length - already)} to ${status.name}…`, false);
    const out = await moveTickets(tickets, status, (t) => movedTo.set(t.ctx.ref.issue, status.name));
    say(`${madeWords(out, `moved to ${status.name}`)}${already ? `, ${already} already in ${status.name}` : ''}`, true);
  }

  async function assign(person: PickItem | null): Promise<void> {
    const tickets = acting;
    endAction();
    chosen = new Set();
    say(`${person ? `Assigning ${many(tickets.length)} to ${person.name}` : `Unassigning ${many(tickets.length)}`}…`, false);
    const out = await assignTickets(tickets, person);
    say(madeWords(out, person ? `assigned to ${person.name}` : 'unassigned'), true);
  }

  // The form's text, which belongs to the tickets it was typed for: a form
  // opened for others starts empty. One start is out at a time
  // (`starting`), and it is this list's: a refused one gives back its text
  // and, when nothing is selected, its tickets; one that ends after the
  // list is gone (its drill closed) does nothing.
  let draft = $state<{ for: string; text: string }>({ for: '', text: '' });
  let starting = $state(0);
  let gone = false;
  onDestroy(() => { gone = true; });
  async function startThread(): Promise<void> {
    if (starting) return;
    const tickets = acting;
    const typed = draft.text;
    const typedFor = actingFor;
    const ids = chosen;
    draft = { for: '', text: '' };
    endAction();
    chosen = new Set();
    starting = tickets.length;
    say(`Starting a thread on ${many(tickets.length)}…`, false);
    const made = await askAboutTickets(tickets.map((t) => t.ctx), source.project, typed.trim(), () => { if (!gone) onThread?.(); });
    starting = 0;
    if (gone) return;
    if (made) {
      say(`Thread started with ${many(tickets.length)} attached`, true);
      return;
    }
    note = null;
    draft = { for: typedFor, text: typed };
    if (chosen.size === 0 && !open) chosen = ids;
  }

  const threadOff = $derived(
    source.threadBlocked ||
    (starting ? `A thread is being started on ${many(starting)}; start the next once it opens.` : '') ||
    (selected.length > MAX_THREAD_TICKETS ? `A thread takes at most ${MAX_THREAD_TICKETS} tickets, one file each; ${selected.length} are selected.` : ''),
  );
  const writeOff = $derived(source.writeBlocked);

  const ITEM = { widget: 'border-t border-border-subtle first:border-t-0', drill: 'border-b border-border-subtle last:border-b-0' };
  const PICK = { widget: 'pr-1.5', drill: 'pl-5 pr-2' };
  const ROW = { widget: 'py-1', drill: 'py-1.5 pr-5' };
  const BAR = { widget: 'border-t border-border-subtle pt-1.5', drill: 'border-t border-border-subtle px-5 py-2' };
</script>

<ul class="flex min-h-0 flex-col {variant === 'drill' ? 'flex-1 overflow-y-auto' : ''}" data-testid={listTestId} data-selecting={selecting}>
  {#each rows as r (r.row)}
    {@const on = chosen.has(r.id)}
    {@const writes = ticketWrites(keyOf(r))}
    <li class="group/row {ITEM[variant]}">
      <div class="flex min-w-0 items-center">
        <span class="flex shrink-0 items-center {PICK[variant]}">
          <button
            type="button"
            role="checkbox"
            aria-checked={on}
            aria-label="Select {r.key}"
            title={on ? 'Take out of the selection' : 'Select'}
            class="relative inline-flex size-3.5 items-center justify-center rounded-[4px] border-[1.5px] after:absolute after:-inset-2 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:size-[18px] compact:opacity-100 {on ? 'border-accent bg-accent text-accent-fg' : 'border-fg-muted bg-surface-0 hover:border-fg'} {selecting || on ? 'opacity-100' : 'opacity-0 group-hover/row:opacity-100 focus-visible:opacity-100'}"
            data-testid="ticket-list-select"
            data-ticket={r.id}
            onclick={() => toggle(r)}
          >
            {#if on}<Icon icon={Check} size={10} strokeWidth={3} />{/if}
          </button>
        </span>
        <button
          type="button"
          class="flex min-w-0 flex-1 items-baseline gap-2 text-left text-[0.75rem] hover:bg-surface-1 focus:outline-none focus-visible:bg-surface-1 {ROW[variant]}"
          data-testid={rowTestId}
          data-counted={r.counted}
          onclick={() => onTicket(r)}
        >
          {@render row(r)}
        </button>
      </div>
      {#if writes.size > 0}
        <div class="flex flex-col gap-0.5 pb-1 {variant === 'drill' ? 'px-5' : ''}" data-testid="ticket-list-notes">
          {#each NOTE_TARGETS.filter((t) => writes.has(t)) as target (target)}
            <TicketWriteNote ticketKey={keyOf(r)} {target} fieldName={cardFieldName} />
          {/each}
        </div>
      {/if}
    </li>
  {:else}
    {@render empty?.()}
  {/each}
</ul>

{#if selecting}
  <div class="flex flex-wrap items-center gap-1 text-[0.75rem] text-fg {BAR[variant]}" data-testid="ticket-list-bar">
    <span data-testid="ticket-list-count"><b class="font-semibold tabular-nums">{selected.length}</b> selected</span>
    <span class="flex-1"></span>
    <span bind:this={moveAnchor}>
      <Button size="xs" variant="ghost" testId="ticket-list-move" disabled={!!writeOff} onclick={() => begin('move')}>
        Move to <Icon icon={ChevronDown} size={12} strokeWidth={2} />
      </Button>
    </span>
    <span bind:this={assignAnchor}>
      <Button size="xs" variant="ghost" testId="ticket-list-assign" disabled={!!writeOff} onclick={() => begin('assign')}>
        Assign <Icon icon={ChevronDown} size={12} strokeWidth={2} />
      </Button>
    </span>
    <span bind:this={threadAnchor}>
      <Button size="xs" variant="ghost" testId="ticket-list-thread" disabled={!!threadOff} onclick={() => begin('thread')}>
        <Icon icon={MessageSquare} size={12} strokeWidth={2} /> Start a thread
      </Button>
    </span>
    <IconButton label="Clear the selection" size="xs" testId="ticket-list-clear" onClick={() => { chosen = new Set(); }}>
      {#snippet children()}<Icon icon={X} size={12} strokeWidth={2} />{/snippet}
    </IconButton>
    <!-- Why an action is off, in words a tap reaches as well as a pointer. -->
    {#if writeOff || threadOff}
      <p class="basis-full text-[0.6875rem] text-fg-muted" data-testid="ticket-list-off">{[...new Set([writeOff, threadOff].filter(Boolean))].join(' ')}</p>
    {/if}
  </div>
{:else if note}
  <div class="flex items-center gap-1.5 text-[0.75rem] text-fg-muted {BAR[variant]}" role="status" data-testid="ticket-list-note" data-done={note.done}>
    {#if note.done}<Icon icon={Check} size={12} strokeWidth={2.25} />{/if}
    <span>{note.text}</span>
  </div>
{/if}

<Popover anchor={moveAnchor} open={open === 'move'} onClose={(reason) => close(reason, moveAnchor?.querySelector('button') ?? undefined)} placement="bottom-end" ariaLabel="Move {many(acting.length)} to">
  <Menu ariaLabel="Move {many(acting.length)} to" onClose={() => close(undefined, moveAnchor?.querySelector('button') ?? undefined)} minWidthClass="min-w-[220px]">
    <p class="px-3 pt-2 pb-1 text-[0.75rem] font-medium text-fg" data-testid="ticket-list-move-title">Move {many(acting.length)} to</p>
    {#if moves.state === 'reading'}
      <MenuItem label="Reading where they can move…" disabled testId="ticket-list-move-reading" />
    {:else if moves.state === 'failed'}
      {#each moves.failed as line (line)}
        <MenuItem label={line} disabled wrap testId="ticket-list-move-failed" />
      {/each}
    {:else}
      {#each moves.statuses as status (status.id)}
        <MenuItem label={status.name} description={status.off || undefined} disabled={!!status.off} wrap testId="ticket-list-move-status" onSelect={() => void move(status)} />
      {:else}
        <MenuItem label="No move reaches another status" disabled testId="ticket-list-move-none" />
      {/each}
    {/if}
  </Menu>
</Popover>

<TicketPicker
  anchor={assignAnchor}
  open={open === 'assign'}
  label="Assign {many(acting.length)} to"
  load={(text) => assignablePeople(acting, text, reads?.signal ?? AbortSignal.abort())}
  checked={new Set()}
  clearLabel="Unassigned"
  onPick={(person) => void assign(person)}
  onClose={endAction}
/>

<Popover anchor={threadAnchor} open={open === 'thread'} onClose={(reason) => close(reason, threadAnchor?.querySelector('button') ?? undefined)} placement="bottom-end" ariaLabel="Start a thread">
  <form
    class="flex w-[300px] flex-col gap-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 p-2.5 shadow-menu compact:w-full"
    data-testid="ticket-list-thread-form"
    onsubmit={(e) => { e.preventDefault(); void startThread(); }}
  >
    <p class="text-[0.75rem] font-medium text-fg">Start a thread on {acting.length === 1 ? 'this ticket' : `these ${acting.length} tickets`}</p>
    <div class="flex items-center gap-1.5">
      <!-- svelte-ignore a11y_autofocus -->
      <input
        class="h-7 min-w-0 flex-1 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-2 text-xs text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-9"
        placeholder="find out why these are stuck"
        aria-label="What the thread is about"
        autofocus
        data-testid="ticket-list-thread-text"
        bind:value={draft.text}
      />
      <Button size="xs" variant="primary" type="submit" testId="ticket-list-thread-start">Start</Button>
    </div>
    <p class="text-[0.6875rem] text-fg-muted">One new thread with {acting.length === 1 ? 'the ticket' : 'the tickets'} attached. Nothing moves.</p>
  </form>
</Popover>
