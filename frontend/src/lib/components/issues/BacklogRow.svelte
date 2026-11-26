<script lang="ts">
  // One row of the Backlog screen (docs/specs/issues.md, "Backlog and
  // sprint planning"). On a pointer screen it is one line: status, type,
  // key, title, labels, the epic or parent, priority, points and assignee;
  // under compact two: the title, then type, key, epic, priority, points
  // and assignee, with a menu button (labels show on the ticket only).
  // Display hides any of them but the title. A click opens its ticket, with
  // Ctrl or Cmd in a pane of its own (`opensTicketPane`), and its menu
  // (right-click, the Menu key, a long press or the button under compact)
  // is the card's (`BoardCardMenu`). The marks are the card's
  // (`CardMarks`): they open the ticket's editors on a pointer screen, and
  // each write is the ticket's own, so the row, a card and the open ticket
  // show one write, its outcome under the row.
  import EllipsisVertical from '@lucide/svelte/icons/ellipsis-vertical';
  import CardEditor from './CardEditor.svelte';
  import CardMarks from './CardMarks.svelte';
  import StatusMark from './StatusMark.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import Icon from '../primitives/Icon.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import { cardWriteContext, movePending } from '../../stores/trackerBoardCarry.svelte';
  import { trackerTicketKey } from '../../stores/trackerTicket.svelte';
  import { BOARD_CARD_TARGETS, ticketWrites } from '../../stores/trackerTicketWrites.svelte';
  import { opensTicketPane } from '../../stores/ticketPanes.svelte';
  import { cardFieldName, type CardEditField } from './ticketEditors';

  interface Props {
    row: RowView;
    /** The group its status mark draws (`markGroup`). */
    mark: string | undefined;
    boardRef: TrackerBoardRef;
    hidden: ReadonlySet<CardField>;
    unit: string;
    typed: boolean;
    compact: boolean;
    /** Its ticket is the one open. */
    open: boolean;
    /** The row is being carried: its place shows dimmed. */
    carried: boolean;
    /** The field whose editor is open on this row; null for none. */
    editing: CardEditField | null;
    onOpen: (row: RowView, ownPane: boolean) => void;
    onMenu: (row: RowView, event: MouseEvent) => void;
    onEdit: (row: RowView, field: CardEditField | null) => void;
  }

  let { row, mark, boardRef, hidden, unit, typed, compact, open, carried, editing, onOpen, onMenu, onEdit }: Props = $props();

  const card = $derived(row.card);
  const ticketKey = $derived(trackerTicketKey({ backend: boardRef.backend, connection: boardRef.connection, issue: card.id }));
  const writes = $derived(ticketWrites(ticketKey));
  const moving = $derived(movePending(writes));
  const notes = $derived(writes.size === 0 ? [] : BOARD_CARD_TARGETS.filter((t) => writes.has(t)));
  const parentText = $derived(card.parent ? card.parent.title || card.parent.key || '' : '');

  let rowEl: HTMLElement | undefined = $state();
  let markEls: Partial<Record<CardEditField, HTMLElement>> = $state({});
  const anchor = $derived(editing ? (markEls[editing] ?? rowEl) : undefined);

  function onContextMenu(event: MouseEvent): void {
    event.preventDefault();
    // The Menu key and Shift+F10 raise it with no pointer: open at the row.
    if (event.clientX === 0 && event.clientY === 0 && rowEl) {
      const r = rowEl.getBoundingClientRect();
      onMenu(row, new MouseEvent('contextmenu', { clientX: r.left + 8, clientY: r.top + 8 }));
      return;
    }
    onMenu(row, event);
  }

  function openMenuAt(el: HTMLElement): void {
    const r = el.getBoundingClientRect();
    onMenu(row, new MouseEvent('contextmenu', { clientX: r.left, clientY: r.bottom + 4 }));
  }
</script>

{#snippet parent()}
  {#if parentText && !hidden.has('parent')}
    <span class="min-w-0 max-w-[12rem] shrink truncate rounded-sm bg-accent/10 px-1.5 text-[0.6875rem] text-accent" title={card.parent?.key ? `${card.parent.key} ${parentText}` : parentText} data-testid="issues-card-parent">{parentText}</span>
  {/if}
{/snippet}

<div
  bind:this={rowEl}
  class="group relative border-b border-border-subtle bg-surface-1 px-3 text-sm {open ? 'bg-accent/10' : 'hover:bg-surface-2'} {carried ? 'opacity-40' : moving ? 'opacity-60' : ''}"
  data-testid="issues-backlog-row"
  data-issue={card.id}
  data-open={open ? '' : undefined}
  data-carried={carried ? '' : undefined}
  data-moving={moving ? '' : undefined}
  role="listitem"
  oncontextmenu={onContextMenu}
>
  <!-- The whole row opens its ticket; its text stays plain under the button. -->
  <button
    type="button"
    class="absolute inset-0 cursor-pointer focus:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40 compact:select-none"
    aria-label="Open {card.key}: {card.title}"
    aria-current={open ? 'true' : undefined}
    data-testid="issues-backlog-row-open"
    onclick={(event) => onOpen(row, opensTicketPane(event))}
  ></button>
  {#if compact}
    <div class="flex flex-col gap-1 py-2">
      <div class="flex min-w-0 items-start gap-2">
        <span class="min-w-0 flex-1 leading-snug text-fg [overflow-wrap:anywhere]" data-testid="issues-card-title">{card.title}</span>
        <button
          type="button"
          class="relative z-[1] -my-1 inline-flex shrink-0 items-center justify-center rounded-[var(--radius-control)] text-fg-muted compact:min-h-9 compact:min-w-9 compact:select-none"
          aria-label="Actions for {card.key}"
          data-card-mark
          data-testid="issues-backlog-row-menu"
          onclick={(event) => openMenuAt(event.currentTarget)}
        ><Icon icon={EllipsisVertical} size={15} strokeWidth={2} /></button>
      </div>
      <div class="flex min-w-0 items-center gap-1.5 text-[0.75rem] text-fg-muted">
        <CardMarks {row} fields={['type']} {hidden} {unit} {typed} editable={false} {writes} bind:els={markEls} onEdit={() => {}} />
        {#if !hidden.has('key')}<span class="shrink-0 tabular-nums" data-testid="issues-card-key">{card.key}</span>{/if}
        {@render parent()}
        <span class="flex-1"></span>
        <CardMarks {row} fields={['priority', 'points', 'assignee']} {hidden} {unit} {typed} editable={false} {writes} bind:els={markEls} onEdit={() => {}} />
      </div>
    </div>
  {:else}
    <div class="flex h-9 min-w-0 items-center gap-2 text-[0.75rem] text-fg-muted">
      <StatusMark group={mark} />
      <CardMarks {row} fields={['type']} {hidden} {unit} {typed} editable {writes} bind:els={markEls} onEdit={(field) => onEdit(row, field)} />
      {#if !hidden.has('key')}<span class="w-16 shrink-0 truncate tabular-nums" data-testid="issues-card-key">{card.key}</span>{/if}
      <span class="min-w-0 flex-1 truncate text-sm text-fg" title={card.title} data-testid="issues-card-title">{card.title}</span>
      {#if card.labels?.length && !hidden.has('labels')}
        <span class="flex shrink-0 gap-1" data-testid="issues-card-labels">
          {#each card.labels.slice(0, 2) as label (label.id)}
            <span class="rounded-sm bg-surface-2 px-1.5 text-[0.6875rem] text-fg-muted">{label.name || label.id}</span>
          {/each}
          {#if card.labels.length > 2}<span class="text-[0.6875rem]" title={card.labels.slice(2).map((l) => l.name || l.id).join(', ')}>+{card.labels.length - 2}</span>{/if}
        </span>
      {/if}
      {@render parent()}
      <CardMarks {row} fields={['priority', 'points', 'assignee']} {hidden} {unit} {typed} editable {writes} bind:els={markEls} onEdit={(field) => onEdit(row, field)} />
    </div>
  {/if}
  {#if notes.length > 0}
    <div class="relative z-[1] flex flex-col gap-0.5 pb-1.5" data-card-mark data-testid="issues-card-notes">
      {#each notes as target (target)}
        <TicketWriteNote {ticketKey} {target} fieldName={cardFieldName} />
      {/each}
    </div>
  {/if}
</div>
{#if editing && anchor}
  <CardEditor ctx={cardWriteContext(boardRef, row)} {row} field={editing} {unit} board={boardRef.board} {typed} {anchor} onClose={() => onEdit(row, null)} />
{/if}
