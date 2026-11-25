<script lang="ts" module>
  // The card's field editors load on the first change from a card, once
  // per page.
  let fieldEditor: Promise<typeof import('./CardFieldEditor.svelte')> | null = null;
  function loadFieldEditor(): Promise<typeof import('./CardFieldEditor.svelte')> {
    fieldEditor ??= import('./CardFieldEditor.svelte');
    return fieldEditor;
  }
</script>

<script lang="ts">
  // One card of the Board screen: the epic or parent, the title, labels,
  // then type, priority and key, the estimate and the assignee, and how
  // long the card has sat in its status. Display hides any of them but the
  // title. A click opens its ticket, and its menu (right-click, the Menu
  // key, or a long press under compact) lists what can be done with it;
  // the open ticket's card is marked. The assignee is initials
  // (ReviewAvatar); no avatar image is fetched.
  //
  // Changing it (docs/specs/issues.md, "Board"). On a pointer screen the
  // type, priority, estimate and assignee marks each open that field's
  // editor at the mark (`CardFieldEditor`, the ticket's editors), and the
  // grab bar at the top center carries the card level (BoardScreen owns
  // the carry). Under compact the marks only show, the status mark opens
  // Move to…, and the menu changes the fields. Each write is the ticket's
  // own (`stores/trackerTicketWrites.svelte.ts`), under the ticket's key,
  // so a mark waits while its write is out and its outcome shows under the
  // card, as on the ticket screen, which shows the same write.
  import PriorityIcon from './PriorityIcon.svelte';
  import StatusMark from './StatusMark.svelte';
  import TicketWriteNote from './TicketWriteNote.svelte';
  import TypeIcon from './TypeIcon.svelte';
  import ReviewAvatar from '../review/ReviewAvatar.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import type { CardField } from '../../stores/issuesPane.svelte';
  import type { TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import { cardMovePending, cardWriteContext } from '../../stores/trackerBoardCarry.svelte';
  import { BOARD_CARD_TARGETS, fieldTarget, ticketWrite } from '../../stores/trackerTicketWrites.svelte';
  import { cardFieldName, type CardEditField } from './ticketEditors';
  import { daysInStatus, formatEstimate } from './boardFormat';

  interface Props {
    row: RowView;
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
    onOpen: (row: RowView) => void;
    onMenu: (row: RowView, event: MouseEvent) => void;
    onEdit: (row: RowView, field: CardEditField | null) => void;
    /** Opens Move to… at `anchor` (compact's status mark). */
    onMove: (row: RowView, anchor: HTMLElement) => void;
  }

  let { row, boardRef, hidden, unit, typed, compact, now, open, carried, editing, onOpen, onMenu, onEdit, onMove }: Props = $props();

  const card = $derived(row.card);
  // The ticket's own write context, so a write shows on both surfaces.
  const ctx = $derived(cardWriteContext(boardRef, row));
  const estimate = $derived(formatEstimate(row.estimate, unit));
  const days = $derived(daysInStatus(card.status_since, now));
  const parentText = $derived(card.parent ? card.parent.title || card.parent.key || '' : '');
  const assigneeName = $derived(card.assignee ? card.assignee.name || 'Unknown user' : '');
  const moving = $derived(cardMovePending(ctx));
  const notes = $derived(BOARD_CARD_TARGETS.filter((t) => ticketWrite(ctx.key, t) !== null));

  const TARGETS: Readonly<Record<CardEditField, string>> = {
    type: fieldTarget('type'), priority: fieldTarget('priority'), points: 'points', assignee: fieldTarget('assignee'),
  };
  const pending = (field: CardEditField) => ticketWrite(ctx.key, TARGETS[field])?.pending ?? false;
  // Marks edit on a pointer screen; under compact the menu does.
  const marks = $derived(!compact);

  let cardEl: HTMLElement | undefined = $state();
  const markEls: Partial<Record<CardEditField, HTMLElement>> = $state({});
  // An editor opened from the menu for a field whose mark is hidden opens
  // at the card.
  const anchor = $derived(editing ? (markEls[editing] ?? cardEl) : undefined);

  function edit(field: CardEditField): void {
    if (!pending(field)) onEdit(row, field);
  }

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

  const markClass = 'relative z-[1] inline-flex shrink-0 items-center rounded-sm focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-50';
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
    onclick={() => onOpen(row)}
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
      ><StatusMark group={card.status.group} /></button>
    {/if}
    {#if !hidden.has('type')}
      {#if marks && typed}
        <button bind:this={markEls.type} type="button" class={markClass} aria-label="Type: {card.type?.name ?? 'None'}" disabled={pending('type')} data-card-mark data-testid="issues-card-type-edit" onclick={() => edit('type')}>
          <TypeIcon type={card.type} testId="issues-card-type" />
        </button>
      {:else}
        <TypeIcon type={card.type} testId="issues-card-type" />
      {/if}
    {/if}
    {#if !hidden.has('priority')}
      {#if marks}
        <button bind:this={markEls.priority} type="button" class="{markClass} {card.priority ? '' : 'h-3.5 w-3.5 opacity-0 group-hover:opacity-100 focus-visible:opacity-100'}" aria-label="Priority: {card.priority?.name ?? 'None'}" disabled={pending('priority')} data-card-mark data-testid="issues-card-priority-edit" onclick={() => edit('priority')}>
          {#if card.priority}<PriorityIcon priority={card.priority} testId="issues-card-priority" />{:else}<span class="h-0.5 w-2.5 rounded-full bg-fg-muted"></span>{/if}
        </button>
      {:else}
        <PriorityIcon priority={card.priority} testId="issues-card-priority" />
      {/if}
    {/if}
    {#if !hidden.has('key')}
      <span class="truncate tabular-nums" data-testid="issues-card-key">{card.key}</span>
    {/if}
    <span class="flex-1"></span>
    {#if unit !== 'none' && unit !== '' && !hidden.has('estimate') && (estimate !== null || marks)}
      {#if marks}
        <button bind:this={markEls.points} type="button" class="{markClass} rounded-sm bg-surface-2 px-1.5 tabular-nums text-fg {estimate === null ? 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100' : ''}" aria-label="{unit === 'time' ? 'Estimate' : 'Points'}: {estimate ?? 'None'}" disabled={pending('points')} data-card-mark data-testid="issues-card-estimate-edit" onclick={() => edit('points')}>
          {#if estimate !== null}<span data-testid="issues-card-estimate">{estimate}</span>{:else}–{/if}
        </button>
      {:else}
        <span class="shrink-0 rounded-sm bg-surface-2 px-1.5 tabular-nums text-fg" title={unit === 'time' ? 'Estimate' : 'Points'} data-testid="issues-card-estimate">{estimate}</span>
      {/if}
    {/if}
    {#if !hidden.has('assignee') && (card.assignee || marks)}
      {#if marks}
        <button bind:this={markEls.assignee} type="button" class="{markClass} rounded-full {card.assignee ? '' : 'h-5 w-5 justify-center border border-dashed border-border opacity-0 group-hover:opacity-100 focus-visible:opacity-100'}" aria-label="Assignee: {card.assignee ? assigneeName : 'Unassigned'}" disabled={pending('assignee')} data-card-mark data-testid="issues-card-assignee-edit" onclick={() => edit('assignee')}>
          {#if card.assignee}
            <span class="inline-flex" data-testid="issues-card-assignee"><ReviewAvatar login={card.assignee.id} name={card.assignee.name} title={assigneeName} size={20} /></span>
          {/if}
        </button>
      {:else}
        <span class="inline-flex" data-testid="issues-card-assignee">
          <ReviewAvatar login={card.assignee!.id} name={card.assignee!.name} title={assigneeName} size={20} />
        </span>
      {/if}
    {/if}
  </div>
  {#if days > 0 && !hidden.has('days')}
    <span class="text-[0.6875rem] text-fg-muted" data-testid="issues-card-days">{days} {days === 1 ? 'day' : 'days'} in {card.status.name || 'this status'}</span>
  {/if}
  {#if notes.length > 0}
    <div class="relative z-[1] flex flex-col gap-0.5" data-card-mark data-testid="issues-card-notes">
      {#each notes as target (target)}
        <TicketWriteNote ticketKey={ctx.key} {target} fieldName={cardFieldName} />
      {/each}
    </div>
  {/if}
</article>
{#if editing && anchor}
  {#await loadFieldEditor() then mod}
    <mod.default {ctx} {row} field={editing} {unit} board={boardRef.board} {typed} {anchor} onClose={() => onEdit(row, null)} />
  {:catch err}
    <p class="text-xs text-error" role="alert" data-testid="issues-card-editor-error">The editor could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
  {/await}
{/if}
