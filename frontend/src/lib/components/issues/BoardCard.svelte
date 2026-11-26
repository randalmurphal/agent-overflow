<script lang="ts">
  // One card of the Board screen: the epic or parent, the title, labels,
  // then type, priority and key, the estimate and the assignee, and how
  // long the card has sat in its status. Display hides any of them but the
  // title. A click opens its ticket, with Ctrl or Cmd in a pane of its own
  // (`opensTicketPane`), and its menu (right-click, the Menu key, or a
  // long press under compact) lists what can be done with it; the open
  // ticket's card is marked. The assignee is initials (ReviewAvatar); no
  // avatar image is fetched.
  //
  // Changing it (docs/specs/issues.md, "Board"). On a pointer screen the
  // type, priority, estimate and assignee marks (`CardMarks`, which a
  // Backlog row shares) each open that field's editor at the mark
  // (`CardEditor`, the ticket's editors), and the
  // grab bar at the top center carries the card level (BoardScreen owns
  // the carry). Under compact the marks only show, the status mark opens
  // Move to…, and the menu changes the fields. Each write is the ticket's
  // own (`stores/trackerTicketWrites.svelte.ts`), under the ticket's key,
  // so a mark waits while its write is out and its outcome shows under the
  // card, as on the ticket screen, which shows the same write.
  import CardEditor from './CardEditor.svelte';
  import CardMarks from './CardMarks.svelte';
  import StatusMark from './StatusMark.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import { cardWriteContext, movePending } from '../../stores/trackerBoardCarry.svelte';
  import { trackerTicketKey } from '../../stores/trackerTicket.svelte';
  import { BOARD_CARD_TARGETS, ticketWrites } from '../../stores/trackerTicketWrites.svelte';
  import { cardFieldName, type CardEditField } from './ticketEditors';
  import { daysInStatus } from './boardFormat';
  import { opensTicketPane } from '../../stores/ticketPanes.svelte';

  interface Props {
    row: RowView;
    /** The group its status mark draws (`markGroup`). */
    mark: string | undefined;
    /** The board the card is on. */
    boardRef: TrackerBoardRef;
    hidden: ReadonlySet<CardField>;
    /** The board's estimate unit. */
    unit: string;
    /** The tracker has issue types (not Linear). */
    typed: boolean;
    compact: boolean;
    /** Now, to the minute. */
    now: number;
    /** Its ticket is the one open. */
    open: boolean;
    /** The card is being carried: its place shows dimmed. */
    carried: boolean;
    /** The field whose editor is open on this card; null for none. */
    editing: CardEditField | null;
    /** Opens its ticket: over or beside the board, or in its own pane. */
    onOpen: (row: RowView, ownPane: boolean) => void;
    onMenu: (row: RowView, event: MouseEvent) => void;
    onEdit: (row: RowView, field: CardEditField | null) => void;
    /** Opens Move to… at `anchor` (compact's status mark). */
    onMove: (row: RowView, anchor: HTMLElement) => void;
  }

  let { row, mark, boardRef, hidden, unit, typed, compact, now, open, carried, editing, onOpen, onMenu, onEdit, onMove }: Props = $props();

  const card = $derived(row.card);
  // The ticket's writes, one read for every target the card draws; under
  // the ticket's own key, so a write shows on both surfaces.
  const ticketKey = $derived(trackerTicketKey({ backend: boardRef.backend, connection: boardRef.connection, issue: card.id }));
  const writes = $derived(ticketWrites(ticketKey));
  const days = $derived(daysInStatus(card.status_since, now));
  const parentText = $derived(card.parent ? card.parent.title || card.parent.key || '' : '');
  const moving = $derived(movePending(writes));
  const notes = $derived(writes.size === 0 ? [] : BOARD_CARD_TARGETS.filter((t) => writes.has(t)));

  // Marks edit on a pointer screen; under compact the menu does.
  const marks = $derived(!compact);

  let cardEl: HTMLElement | undefined = $state();
  let markEls: Partial<Record<CardEditField, HTMLElement>> = $state({});
  // An editor opened from the menu for a field whose mark is hidden opens
  // at the card.
  const anchor = $derived(editing ? (markEls[editing] ?? cardEl) : undefined);

  function onContextMenu(event: MouseEvent): void {
    event.preventDefault();
    // The Menu key and Shift+F10 raise it with no pointer: open at the card.
    if (event.clientX === 0 && event.clientY === 0 && cardEl) {
      const r = cardEl.getBoundingClientRect();
      onMenu(row, new MouseEvent('contextmenu', { clientX: r.left + 8, clientY: r.top + 8 }));
      return;
    }
    onMenu(row, event);
  }
</script>

<article
  bind:this={cardEl}
  class="group relative mb-1.5 flex flex-col gap-1.5 rounded-[var(--radius-field)] border bg-surface-1 px-3 py-2 text-sm {open ? 'border-accent' : 'border-border-subtle hover:border-border'} {carried ? 'opacity-40' : moving ? 'opacity-60' : ''}"
  data-testid="issues-card"
  data-issue={card.id}
  data-open={open ? '' : undefined}
  data-carried={carried ? '' : undefined}
  data-moving={moving ? '' : undefined}
  oncontextmenu={onContextMenu}
>
  <!-- The whole card opens its ticket; its text stays plain under the button. -->
  <button
    type="button"
    class="absolute inset-0 cursor-pointer rounded-[var(--radius-field)] focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none"
    aria-label="Open {card.key}: {card.title}"
    aria-current={open ? 'true' : undefined}
    data-testid="issues-card-open"
    onclick={(event) => onOpen(row, opensTicketPane(event))}
  ></button>
  {#if !compact}
    <span
      class="absolute left-1/2 top-0 z-[1] flex h-3 w-16 -translate-x-1/2 cursor-grab items-center justify-center opacity-0 group-hover:opacity-100"
      title="Grab here to carry it level"
      data-grab
      data-testid="issues-card-grab"
    ><span class="h-1 w-7 rounded-full bg-fg-muted"></span></span>
  {/if}
  {#if parentText && !hidden.has('parent')}
    <span class="max-w-full self-start truncate rounded-sm bg-accent/10 px-1.5 text-[0.6875rem] text-accent" title={card.parent?.key ? `${card.parent.key} ${parentText}` : parentText} data-testid="issues-card-parent">{parentText}</span>
  {/if}
  <span class="leading-snug text-fg [overflow-wrap:anywhere]" data-testid="issues-card-title">{card.title}</span>
  {#if card.labels?.length && !hidden.has('labels')}
    <div class="flex flex-wrap gap-1" data-testid="issues-card-labels">
      {#each card.labels as label (label.id)}
        <span class="rounded-sm bg-surface-2 px-1.5 text-[0.6875rem] text-fg-muted">{label.name || label.id}</span>
      {/each}
    </div>
  {/if}
  <div class="flex min-w-0 items-center gap-1.5 text-[0.75rem] text-fg-muted">
    {#if compact}
      <button
        type="button"
        class="relative z-[1] inline-flex shrink-0 items-center justify-center rounded-[var(--radius-control)] compact:min-h-9 compact:min-w-9 compact:select-none disabled:opacity-50"
        aria-label="Move {card.key} from {card.status.name || 'its status'}"
        disabled={moving}
        data-card-mark
        data-testid="issues-card-status"
        onclick={(event) => onMove(row, event.currentTarget)}
      ><StatusMark group={mark} /></button>
    {/if}
    <CardMarks {row} fields={['type', 'priority']} {hidden} {unit} {typed} editable={marks} {writes} bind:els={markEls} onEdit={(field) => onEdit(row, field)} />
    {#if !hidden.has('key')}
      <span class="truncate tabular-nums" data-testid="issues-card-key">{card.key}</span>
    {/if}
    <span class="flex-1"></span>
    <CardMarks {row} fields={['points', 'assignee']} {hidden} {unit} {typed} editable={marks} {writes} bind:els={markEls} onEdit={(field) => onEdit(row, field)} />
  </div>
  {#if days > 0 && !hidden.has('days')}
    <span class="text-[0.6875rem] text-fg-muted" data-testid="issues-card-days">{days} {days === 1 ? 'day' : 'days'} in {card.status.name || 'this status'}</span>
  {/if}
  {#if notes.length > 0}
    <div class="relative z-[1] flex flex-col gap-0.5" data-card-mark data-testid="issues-card-notes">
      {#each notes as target (target)}
        <TicketWriteNote {ticketKey} {target} fieldName={cardFieldName} />
      {/each}
    </div>
  {/if}
</article>
{#if editing && anchor}
  <CardEditor ctx={cardWriteContext(boardRef, row)} {row} field={editing} {unit} board={boardRef.board} {typed} {anchor} onClose={() => onEdit(row, null)} />
{/if}
