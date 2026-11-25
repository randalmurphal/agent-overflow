<script lang="ts" module>
  // The carry loads on the first press of a card, once per page.
  let carryModule: Promise<typeof import('./boardCarry')> | null = null;
  function loadCarry(): Promise<typeof import('./boardCarry')> {
    carryModule ??= import('./boardCarry');
    return carryModule;
  }
</script>

<script lang="ts">
  // The Board screen: the board's columns side by side, the row scrolling
  // sideways when the pane is narrower than the columns. Under compact the
  // page never scrolls sideways: a strip of the columns, each with its count
  // and estimate sum, picks the one column shown (kept per board while the
  // app runs; the first in-progress column until one is picked). Only mine
  // and Search filter the held cards; while either is on, each column not
  // held whole is read whole from this computer's copy.
  //
  // Changing a card (docs/specs/issues.md, "Board"). A card's marks open
  // its field editors (BoardCard); its menu is BoardCardMenu. A mouse or
  // pen carries a card (boardCarry.ts): to another column it moves the
  // ticket by the move that reaches that column, read when the card first
  // leaves its own; within its own column it ranks it between the cards
  // shown either side of the drop. A dropped card waits where it was
  // dropped while its write is out (`stores/trackerBoardCarry.svelte.ts`),
  // and the headers count it there. While carried, the column under it
  // says whether it can take it and the two headers show what they would
  // count; nothing else renders per pointer move.
  import { tick, untrack } from 'svelte';
  import BoardCardMenu from './BoardCardMenu.svelte';
  import BoardColumn from './BoardColumn.svelte';
  import StatusMark from './StatusMark.svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import { columnCount } from './boardFormat';
  import { setTicketWriteSurface } from './ticketWriteSurface';
  import type { Carry, CarryColumn, CarryHost } from './boardCarry';
  import type { CardEditField } from './ticketEditors';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import { compactColumn, setCompactColumn, type CardField } from '../../stores/issuesPane.svelte';
  import { loadMoreOfSection, setTrackerBoardFiltered, type TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import { cardBusy, cardWriteContext, moveCard, rankCard, startDrop, waitingCards, type CardDrop } from '../../stores/trackerBoardCarry.svelte';
  import { moveTicketToSprint, ticketMoves, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import type { MoveView, RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import {
    cardMatches,
    cardURL,
    columnsReached,
    columnWith,
    filtering,
    rowOf,
    screenColumns,
    sectionKey,
    shownColumn,
    withWaiting,
    type CardFilter,
    type HeldBoard,
    type ScreenColumn,
    type ShownColumn,
  } from '../../stores/trackerBoardModel';
  import { copyToClipboard } from '../../utils/clipboard';
  import { errString } from '../../utils/errors';
  import { handleExternalURL } from '../../utils/externalLinks';

  interface Props {
    boardKey: string;
    boardRef: TrackerBoardRef;
    held: HeldBoard;
    /** The section being read (`TrackerBoardState.paging`). */
    paging: ReadonlySet<string>;
    /** The sections whose page read failed (`TrackerBoardState.sectionErrors`). */
    sectionErrors: ReadonlyMap<string, unknown>;
    filter: CardFilter;
    hidden: ReadonlySet<CardField>;
    compact: boolean;
    /** False while the person does not see the board. */
    shown: boolean;
    /** The issue whose ticket is open, null for none. */
    openIssue: string | null;
    /** The tracker has issue types (not Linear). */
    typed: boolean;
    onOpenCard: (row: RowView) => void;
    /** A move whose screen takes fields: the ticket opens with its form. */
    onMoveWithFields: (row: RowView, move: MoveView) => void;
  }

  let { boardKey, boardRef, held, paging, sectionErrors, filter, hidden, compact, shown, openIssue, typed, onOpenCard, onMoveWithFields }: Props = $props();
  setTicketWriteSurface(() => shown);

  // The last columns, so a rebuild keeps the rows arrays nothing changed.
  let previous: ScreenColumn[] = [];
  const columns = $derived.by(() => {
    const next = screenColumns(held, previous);
    previous = next;
    return next;
  });
  const filtered = $derived(filtering(filter));
  const visible = $derived(columns.map((col) => shownColumn(held, col, filter)));
  const now = $derived(getMinuteNow());
  const unit = $derived(held.shape.unit);

  // The cards waiting where they were dropped, drawn there.
  const waiting = $derived(waitingCards(boardKey));
  const drawn = $derived(withWaiting(columns, visible, waiting, filter));

  // A carry under way: the card, the column it came from, the column under
  // it and the drop slot in its own column, and what the moves read said.
  type MovesRead = { readonly state: 'reading' } | { readonly state: 'read'; readonly reached: Map<number, MoveView[]> } | { readonly state: 'failed'; readonly error: unknown };
  interface Carrying {
    /** Which carry: a copy still gliding home ends its own carry only. */
    readonly id: number;
    readonly row: RowView;
    readonly ctx: TicketWriteContext;
    readonly from: number;
    readonly over: number | null;
    readonly slot: number | null;
    readonly moves: MovesRead | null;
  }
  let carrying = $state.raw<Carrying | null>(null);
  let carries = 0;
  const carriedIssue = $derived(carrying?.row.card.id ?? null);

  /** Whether a carried card over `column`, another than its own, may be
   * dropped there: unknown (null) until the moves are read. */
  function reaches(c: Carrying, column: number): boolean | null {
    if (!c.moves || c.moves.state === 'reading') return null;
    return c.moves.state === 'read' && c.moves.reached.has(column);
  }

  // The cards counted in a column other than the one holding them: each
  // waiting card, and a carried card over a column that may take it, with
  // the statuses it may have there (its move's, else any of the column's).
  const moved = $derived.by(() => {
    const out: { row: RowView; from: number; to: number; statuses: readonly string[] }[] = [];
    for (const w of waiting) {
      const at = rowOf(columns, w.issue);
      if (at && at.column !== w.column) out.push({ row: at.row, from: at.column, to: w.column, statuses: w.status ? [w.status] : statusesOf(w.column) });
    }
    const c = carrying;
    if (c && c.over !== null && c.over !== c.from && reaches(c, c.over) !== false) {
      const into = c.moves?.state === 'read' ? c.moves.reached.get(c.over) : undefined;
      out.push({ row: c.row, from: c.from, to: c.over, statuses: into ? into.map((m) => m.to.id) : statusesOf(c.over) });
    }
    return out;
  });
  function statusesOf(column: number): string[] {
    return held.shape.columns[column]?.statuses.map((st) => st.id) ?? [];
  }
  const heads = $derived(columns.map((col, i) => {
    let head: { col: ScreenColumn; shown: ShownColumn } = { col, shown: drawn[i] };
    for (const m of moved) {
      const inShown = !filtered || cardMatches(m.row.card, filter);
      if (m.from === i) head = columnWith(held, head.col, head.shown, m.row, -1, [m.row.card.status.id], inShown);
      if (m.to === i) head = columnWith(held, head.col, head.shown, m.row, 1, m.statuses, inShown);
    }
    return head;
  }));
  // One header count per column, for the strip and the column's header.
  const counts = $derived(heads.map((h) => columnCount(h.col, h.shown, filtered, unit)));

  /** What column `i` says while the carried card is over it. */
  function dropNote(i: number): string | null {
    const c = carrying;
    if (!c || c.over !== i || i === c.from) return null;
    const key = c.row.card.key;
    if (!c.moves || c.moves.state === 'reading') return `Checking whether ${key} can move here…`;
    if (c.moves.state === 'failed') return `Could not read where ${key} can move: ${errString(c.moves.error)}`;
    return c.moves.reached.has(i) ? null : `No move of ${key} reaches ${columns[i].name}`;
  }
  const noSprint = $derived(held.shape.plansSprints && !held.shape.sprints.some((sp) => sp.state === 'active'));

  // A filter reads the rest of every column while it is on.
  $effect(() => {
    const on = filtered;
    untrack(() => setTrackerBoardFiltered(boardKey, on));
  });

  function moreOf(col: ScreenColumn): () => void {
    return () => {
      if (col.moreFrom !== null) loadMoreOfSection(boardKey, held.sections[col.moreFrom].section);
    };
  }

  // The sections a column still reads from: its own on a kanban board,
  // every sprint's on a sprint board.
  function readingFrom(col: ScreenColumn): string[] {
    if (col.moreFrom === null) return [];
    if (held.shape.plansSprints) return held.sections.filter((s) => s.section.kind === 'sprint').map((s) => sectionKey(s.section));
    return [sectionKey(held.sections[col.moreFrom].section)];
  }

  function pagingOf(col: ScreenColumn): boolean {
    return readingFrom(col).some((k) => paging.has(k));
  }

  function failedOf(col: ScreenColumn): boolean {
    return readingFrom(col).some((k) => sectionErrors.has(k));
  }

  // The card whose field editor is open, and the field.
  let editing = $state.raw<{ readonly issue: string; readonly field: CardEditField } | null>(null);
  function onEditCard(row: RowView, field: CardEditField | null): void {
    editing = field ? { issue: row.card.id, field } : null;
  }

  // The card menu, at the pointer; under compact the status mark opens it
  // at Move to….
  let menu = $state.raw<{ x: number; y: number; row: RowView; from: number; atMove: boolean } | null>(null);
  function columnShowing(issue: string): number {
    return drawn.findIndex((col) => col.rows.some((r) => r.card.id === issue));
  }
  function onCardMenu(row: RowView, event: MouseEvent): void {
    menu = { x: event.clientX, y: event.clientY, row, from: columnShowing(row.card.id), atMove: false };
  }
  function onMoveCard(row: RowView, anchor: HTMLElement): void {
    const r = anchor.getBoundingClientRect();
    menu = { x: r.left, y: r.bottom + 4, row, from: columnShowing(row.card.id), atMove: true };
  }

  async function copy(text: string, what: string): Promise<void> {
    if (!(await copyToClipboard(text))) addToast('error', `Could not copy the ${what}.`);
  }

  /** Moves the card by `move` into `column`: with its form when the move
   * takes fields, else at once, the card waiting there; as the drop
   * `drop` when one decided it. */
  function moveTo(row: RowView, column: number, move: MoveView, drop?: CardDrop): void {
    if (move.fields?.length) {
      onMoveWithFields(row, move);
      return;
    }
    if (drop) void drop.move(column, move);
    else void moveCard(boardKey, cardWriteContext(boardRef, row), column, move);
  }

  // Several moves reach the column a card was dropped on: which. The drop
  // holds the card until a move is chosen or the choice is dismissed.
  let dropChoice = $state.raw<{ x: number; y: number; row: RowView; column: number; moves: readonly MoveView[]; drop: CardDrop } | null>(null);
  function endChoice(): void {
    dropChoice?.drop.end();
    dropChoice = null;
  }

  // A press on a card, until it moves far enough to pick the card up or
  // ends as the click that opens the ticket.
  interface Press {
    readonly id: number;
    readonly el: HTMLElement;
    readonly row: RowView;
    readonly from: number;
    readonly level: boolean;
    readonly sx: number;
    readonly sy: number;
    x: number;
    y: number;
    carry: Carry | null;
    starting: boolean;
  }
  let press: Press | null = null;
  let columnsEl: HTMLElement | undefined = $state();
  const columnRefs: ({ carryColumn(): CarryColumn } | undefined)[] = $state([]);

  function onPointerDown(event: PointerEvent): void {
    if (compact || press || event.button !== 0 || (event.pointerType !== 'mouse' && event.pointerType !== 'pen')) return;
    // A press with Ctrl or Cmd held is a click with its own meaning.
    if (event.ctrlKey || event.metaKey) return;
    const target = event.target instanceof Element ? event.target : null;
    if (!target || target.closest('[data-card-mark]')) return;
    const el = target.closest<HTMLElement>('[data-testid="issues-card"]');
    const issue = el?.dataset.issue;
    if (!el || !issue) return;
    const from = columnShowing(issue);
    const row = from >= 0 ? drawn[from].rows.find((r) => r.card.id === issue) : undefined;
    if (!row || cardBusy(boardKey, cardWriteContext(boardRef, row))) return;
    void loadCarry().catch(() => {});
    press = { id: event.pointerId, el, row, from, level: target.closest('[data-grab]') !== null, sx: event.clientX, sy: event.clientY, x: event.clientX, y: event.clientY, carry: null, starting: false };
    window.addEventListener('pointermove', onPressMove);
    window.addEventListener('pointerup', onPressUp);
    window.addEventListener('pointercancel', onPressCancel);
  }

  function endPress(): void {
    window.removeEventListener('pointermove', onPressMove);
    window.removeEventListener('pointerup', onPressUp);
    window.removeEventListener('pointercancel', onPressCancel);
    press = null;
  }

  function onPressMove(event: PointerEvent): void {
    const p = press;
    if (!p || event.pointerId !== p.id) return;
    p.x = event.clientX;
    p.y = event.clientY;
    if (p.carry) {
      p.carry.move(p.x, p.y);
      return;
    }
    if (p.starting || Math.hypot(p.x - p.sx, p.y - p.sy) < PICK_UP_PX) return;
    p.starting = true;
    loadCarry().then(
      (mod) => {
        if (press !== p) return;
        pickUp(mod.startCarry, p);
      },
      (err: unknown) => {
        if (press === p) endPress();
        addErrorToast('Could not carry the card: the board\'s carry did not load. Reload the window to try again.', err);
      },
    );
  }

  // How far a press moves before it picks the card up.
  const PICK_UP_PX = 5;

  function pickUp(start: typeof import('./boardCarry').startCarry, p: Press): void {
    const scroller = columnsEl;
    const geometry = columnRefs.map((ref) => ref?.carryColumn());
    if (!scroller || geometry.some((g) => !g)) {
      endPress();
      return;
    }
    editing = null;
    menu = null;
    movesRead = null;
    const ctx = cardWriteContext(boardRef, p.row);
    const id = ++carries;
    carrying = { id, row: p.row, ctx, from: p.from, over: p.from, slot: null, moves: null };
    const host: CarryHost = {
      columns: geometry as CarryColumn[],
      scroller,
      from: p.from,
      over(column) {
        const c = carrying;
        if (c?.id !== id) return;
        carrying = { ...c, over: column };
        if (column !== null && column !== c.from) readMoves();
      },
      slot(index) {
        const c = carrying;
        if (c?.id === id && c.slot !== index) carrying = { ...c, slot: index };
      },
      drop: (column, slot, at) => drop(p.row, p.from, column, slot, at),
      end() {
        if (carrying?.id === id) carrying = null;
      },
    };
    p.carry = start(p.el, p.sx, p.sy, p.x, p.y, p.level, host);
  }

  // The moves of the carried ticket, read once per carry when the card
  // first leaves its own column.
  let movesRead: Promise<MoveView[]> | null = null;
  function readMoves(): Promise<MoveView[]> {
    const c = carrying!;
    if (movesRead) return movesRead;
    const read = ticketMoves(c.ctx);
    movesRead = read;
    carrying = { ...c, moves: { state: 'reading' } };
    read.then(
      (moves) => {
        if (movesRead === read && carrying) carrying = { ...carrying, moves: { state: 'read', reached: columnsReached(held.shape.columns, moves) } };
      },
      (error: unknown) => {
        if (movesRead === read && carrying) carrying = { ...carrying, moves: { state: 'failed', error } };
      },
    );
    return read;
  }

  /** Where the card shows now, for the carried copy to glide to; null
   * when it is not mounted. */
  async function shownAt(issue: string): Promise<DOMRect | null> {
    await tick();
    const el = columnsEl?.querySelector<HTMLElement>(`[data-testid="issues-card"][data-issue="${CSS.escape(issue)}"]`);
    return el?.getBoundingClientRect() ?? null;
  }

  async function drop(row: RowView, from: number, column: number | null, slot: number | null, at: { x: number; y: number }): Promise<DOMRect | null> {
    const ctx = cardWriteContext(boardRef, row);
    if (column === null) return shownAt(row.card.id);
    if (column === from) {
      if (slot === null) return shownAt(row.card.id);
      // The card shown after the slot, the carried one left out; the rank
      // names it and the one the board holds before it when it is sent.
      const rows = drawn[from].rows;
      const own = rows.findIndex((r) => r.card.id === row.card.id);
      const k = own >= 0 && own < slot ? slot - 1 : slot;
      if (k === own) return shownAt(row.card.id);
      const rest = rows.filter((r) => r.card.id !== row.card.id);
      void rankCard(boardKey, ctx, boardRef.board, from, rest[k]?.card.id ?? null);
      return shownAt(row.card.id);
    }
    const hold = startDrop(boardKey, ctx);
    if (!hold) return shownAt(row.card.id);
    let handed = false;
    try {
      const known = carrying?.moves?.state === 'read' || carrying?.moves?.state === 'failed';
      let moves: MoveView[];
      try {
        moves = await (movesRead ?? readMoves());
      } catch (err) {
        if (!known) addErrorToast(`Could not read where ${row.card.key} can move.`, err);
        return shownAt(row.card.id);
      }
      const into = columnsReached(held.shape.columns, moves).get(column) ?? [];
      if (into.length === 0) {
        if (!known) addToast('info', `No move of ${row.card.key} reaches ${columns[column].name}.`);
        return shownAt(row.card.id);
      }
      if (into.length > 1) {
        dropChoice = { x: at.x, y: at.y, row, column, moves: into, drop: hold };
        handed = true;
        return shownAt(row.card.id);
      }
      moveTo(row, column, into[0], hold);
      return shownAt(row.card.id);
    } finally {
      if (!handed) hold.end();
    }
  }

  function onPressUp(event: PointerEvent): void {
    const p = press;
    if (!p || event.pointerId !== p.id) return;
    endPress();
    if (!p.carry) return;
    // The click a carry's release raises opens nothing.
    const swallow = (e: MouseEvent) => {
      e.preventDefault();
      e.stopPropagation();
    };
    window.addEventListener('click', swallow, { capture: true, once: true });
    setTimeout(() => window.removeEventListener('click', swallow, { capture: true }), 0);
    p.carry.up(event.clientX, event.clientY);
  }

  function onPressCancel(event: PointerEvent): void {
    const p = press;
    if (!p || event.pointerId !== p.id) return;
    endPress();
    p.carry?.cancel();
  }

  $effect(() => () => {
    const p = press;
    endPress();
    p?.carry?.cancel();
    untrack(endChoice);
  });

  const picked = $derived.by(() => {
    const chosen = compactColumn(boardRef);
    if (chosen !== null && chosen < columns.length) return chosen;
    const working = columns.findIndex((col) => col.statusGroups.includes('in_progress'));
    return working >= 0 ? working : 0;
  });

</script>

<div class="flex min-h-0 flex-1 flex-col" data-testid="issues-board">
  {#if noSprint}
    <p class="px-3 py-2 text-sm text-fg-muted" data-testid="issues-board-no-sprint">No sprint is active on this board.</p>
  {:else if columns.length === 0}
    <p class="px-3 py-2 text-sm text-fg-muted" data-testid="issues-board-no-columns">This board has no columns.</p>
  {:else if compact}
    <div class="flex flex-wrap gap-1 border-b border-border-subtle px-2 py-1.5" role="tablist" aria-label="Columns" data-testid="issues-column-strip">
      {#each columns as col, i (i)}
        <button
          type="button"
          role="tab"
          aria-selected={i === picked}
          class="inline-flex h-9 min-w-0 items-center gap-1.5 rounded-[var(--radius-control)] px-2.5 text-sm compact:select-none {i === picked ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
          data-testid="issues-column-tab"
          onclick={() => setCompactColumn(boardRef, i)}
        >
          <StatusMark group={col.statusGroups[0]} />
          <span class="truncate">{col.name}</span>
          <span class="shrink-0 tabular-nums text-fg-muted">{counts[i]}</span>
        </button>
      {/each}
    </div>
    {#key picked}
      <div class="flex min-h-0 flex-1 px-2 pt-2">
        <BoardColumn
          column={columns[picked]}
          rows={drawn[picked].rows}
          count={counts[picked]}
          {filtered}
          {unit}
          {hidden}
          {now}
          paging={pagingOf(columns[picked])}
          failed={failedOf(columns[picked])}
          onNearEnd={moreOf(columns[picked])}
          {shown}
          wide
          {openIssue}
          {boardRef}
          {typed}
          {compact}
          carriedIssue={null}
          {editing}
          dropNote={null}
          slot={null}
          {onOpenCard}
          {onCardMenu}
          {onEditCard}
          {onMoveCard}
        />
      </div>
    {/key}
  {:else}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div bind:this={columnsEl} class="flex min-h-0 flex-1 gap-3 overflow-x-auto px-3 pt-3" data-testid="issues-columns" onpointerdown={onPointerDown}>
      {#each columns as col, i (i)}
        <BoardColumn
          bind:this={columnRefs[i]}
          column={col}
          rows={drawn[i].rows}
          count={counts[i]}
          {filtered}
          {unit}
          {hidden}
          {now}
          paging={pagingOf(col)}
          failed={failedOf(col)}
          onNearEnd={moreOf(col)}
          {shown}
          {openIssue}
          {boardRef}
          {typed}
          {compact}
          {carriedIssue}
          {editing}
          dropNote={dropNote(i)}
          slot={carrying?.from === i ? carrying.slot : null}
          {onOpenCard}
          {onCardMenu}
          {onEditCard}
          {onMoveCard}
        />
      {/each}
    </div>
  {/if}
</div>

{#if menu}
  {@const open = menu}
  {@const ctx = cardWriteContext(boardRef, open.row)}
  {#key open}
    <BoardCardMenu
      x={open.x}
      y={open.y}
      row={open.row}
      {ctx}
      shape={held.shape}
      from={open.from}
      {typed}
      url={cardURL(held.shape, open.row.card)}
      moving={cardBusy(boardKey, ctx)}
      atMove={open.atMove}
      onOpen={() => onOpenCard(open.row)}
      onOpenURL={() => { void handleExternalURL(cardURL(held.shape, open.row.card)); }}
      onCopy={(text, what) => { void copy(text, what); }}
      onEdit={(field) => onEditCard(open.row, field)}
      onMove={(column, move) => moveTo(open.row, column, move)}
      onSprint={(sprint) => { void moveTicketToSprint(ctx, boardRef.board, sprint); }}
      onDismiss={() => { menu = null; }}
    />
  {/key}
{/if}

{#if dropChoice}
  {@const choice = dropChoice}
  <ContextMenu x={choice.x} y={choice.y} ariaLabel="Move {choice.row.card.key} by" onDismiss={endChoice}>
    {#each choice.moves as move (move.id)}
      <MenuItem
        label={move.name}
        description={move.fields?.length ? 'Asks for fields' : undefined}
        testId="issues-card-drop-move"
        onSelect={() => { moveTo(choice.row, choice.column, move, choice.drop); endChoice(); }}
      />
    {/each}
  </ContextMenu>
{/if}
