<script lang="ts" module>
  // The carry loads on the first press of a row, once per page; it is the
  // Board's (listCarry.ts).
  let carryModule: Promise<typeof import('./listCarry')> | null = null;
  function loadCarry(): Promise<typeof import('./listCarry')> {
    carryModule ??= import('./listCarry');
    return carryModule;
  }
</script>

<script lang="ts">
  // The Backlog screen (docs/specs/issues.md, "Backlog and sprint
  // planning"): the board's open sprints, the active ones first, then the
  // backlog, as the board's computer lists them (`BoardView.plan`), each a
  // header and, unfolded, its rows and a foot where a ticket is made. The
  // folds are remembered per board (`backlogSectionOpen`); an active sprint
  // starts folded. One virtualizer holds every section's items, so the DOM
  // holds the rows near the viewport whatever the sections hold. A sprint is
  // read whole once held; the backlog reads its next page as its foot nears
  // the viewport, and whole while Only mine or Search is on.
  //
  // Changing a row. Its marks and menu are the card's (BacklogRow,
  // BoardCardMenu). A mouse or pen carries it level (listCarry.ts): within
  // its section it ranks it, into another it moves it into that sprint or
  // the backlog at that place, each a write of the ticket's
  // (`stores/trackerBoardCarry.svelte.ts`, `rankCard`, `placeCard`). While
  // it is carried the rows under it slide to open a gap and its own place
  // closes, each a transform on a mounted item that changes only when the
  // gap moves; the section it left and the one it is over say what their
  // numbers would become. A dropped row waits where it was dropped while
  // its write is out, counted there, and is not moved again until the
  // write settles. Under compact there is no carry: the menu moves a row
  // between the sprints and the backlog.
  import { tick, untrack } from 'svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import BacklogCreate from './BacklogCreate.svelte';
  import BacklogRow from './BacklogRow.svelte';
  import BacklogSectionHeader from './BacklogSectionHeader.svelte';
  import BoardCardMenu from './BoardCardMenu.svelte';
  import SprintControls from './SprintControls.svelte';
  import { setTicketWriteSurface } from './ticketWriteSurface';
  import type { Carry, CarryHost, CarryLayout, CarrySpot } from './listCarry';
  import type { CardEditField } from './ticketEditors';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import { backlogSectionOpen, setBacklogSectionOpen, type CardField } from '../../stores/issuesPane.svelte';
  import { loadMoreOfSection, setTrackerBoardFiltered, type TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import { cardBusy, cardWriteContext, placeCard, rankCard, waitingCards } from '../../stores/trackerBoardCarry.svelte';
  import { boardWriteContext, moveTicket, moveTicketToSprint, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import type { MoveView, RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { cardURL, filtering, sectionKey, type CardFilter, type HeldBoard } from '../../stores/trackerBoardModel';
  import {
    backlogItems,
    backlogSections,
    gapItem,
    sectionNumbers,
    shownSections,
    spotAt,
    waitingMoves,
    type BacklogItem,
    type BacklogSection,
    type MovedRow,
  } from '../../stores/trackerBacklogModel';
  import { copyToClipboard } from '../../utils/clipboard';
  import { handleExternalURL } from '../../utils/externalLinks';

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  // Rows from a section's foot at which its next page is asked for.
  const NEAR_END_ROWS = 15;
  // How far a press moves before it picks the row up.
  const PICK_UP_PX = 5;

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
    /** False while the person does not see the screen. */
    shown: boolean;
    /** The issue whose ticket is open, null for none. */
    openIssue: string | null;
    /** The board's tracker: jira or linear; empty until a listing names it. */
    kind: string;
    onOpenCard: (row: RowView, ownPane: boolean) => void;
    /** A move whose screen takes fields: the ticket opens with its form. */
    onMoveWithFields: (row: RowView, move: MoveView) => void;
  }

  let {
    boardKey, boardRef, held, paging, sectionErrors, filter, hidden, compact, shown, openIssue, kind, onOpenCard, onMoveWithFields,
  }: Props = $props();
  setTicketWriteSurface(() => shown);

  // Linear has no issue types, and runs its cycles from the team's
  // settings: it takes no sprint write.
  const typed = $derived(kind !== 'linear');
  const sprintWrites = $derived(kind === 'jira' && held.shape.plansSprints);
  // The board's sprints and the tickets made on it are its own writes.
  const b = $derived(boardWriteContext(boardRef));
  const unit = $derived(held.shape.unit);
  const now = $derived(getMinuteNow());
  const filtered = $derived(filtering(filter));
  const sections = $derived(backlogSections(held));
  const waiting = $derived(waitingCards(boardKey));
  const rowsShown = $derived(shownSections(sections, waiting, filter));
  // An active sprint starts folded.
  const isOpen = (sec: BacklogSection) => backlogSectionOpen(boardRef, sec.list, sec.kind !== 'active');
  const items = $derived(backlogItems(sections, rowsShown, isOpen));

  // A filter reads the rest of every section while it is on.
  $effect(() => {
    const on = filtered;
    untrack(() => setTrackerBoardFiltered(boardKey, on));
  });

  // A carry under way: the row, the list it came from and the spot under
  // it. Changes only when the spot does.
  interface Carrying {
    readonly id: number;
    readonly row: RowView;
    readonly ctx: TicketWriteContext;
    readonly from: string;
    readonly spot: CarrySpot | null;
    /** The carried row's height, the gap it opens. */
    readonly height: number;
  }
  let carrying = $state.raw<Carrying | null>(null);
  let carries = 0;

  // The rows counted in another section than the one holding them: each
  // waiting row, and the carried row over another section.
  const moved = $derived.by((): MovedRow[] => {
    const out = waitingMoves(sections, waiting);
    const c = carrying;
    if (c?.spot && c.spot.list !== c.from) out.push({ row: c.row, from: c.from, to: c.spot.list });
    return out;
  });
  const numbers = $derived(sections.map((sec) => sectionNumbers(held, sec, moved)));

  // The gap a carry opens and the place it closes, as item indices; -1
  // for none. Outside every list neither shows.
  const own = $derived(carrying?.spot ? items.findIndex((it) => it.kind === 'row' && it.row.card.id === carrying!.row.card.id) : -1);
  const gap = $derived(carrying?.spot && carrying.spot.slot !== null ? gapItem(items, sections, carrying.spot.list, carrying.spot.slot) : -1);
  function shiftOf(i: number): number {
    const c = carrying;
    if (!c?.spot) return 0;
    return (gap >= 0 && i >= gap ? c.height : 0) - (own >= 0 && i > own ? c.height : 0);
  }

  const ESTIMATE = { head: 44, row: 37, rowCompact: 62, foot: 34 } as const;
  const estimate = $derived({
    at: (i: number) => {
      const kind = items[i]?.kind ?? 'row';
      return kind === 'head' ? ESTIMATE.head : kind === 'foot' ? ESTIMATE.foot : compact ? ESTIMATE.rowCompact : ESTIMATE.row;
    },
    isExact: () => false,
  });
  const getKey = (item: BacklogItem) => item.key;

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();

  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }

  // A section with more to read asks for its next page as its foot nears
  // the viewport; never while a filter reads it whole.
  function check(): void {
    if (filtered || !shown) return;
    const ref = listRef;
    if (!ref) return;
    const viewport = ref.getViewportSize();
    if (viewport <= 0) return;
    const last = ref.findItemIndex(ref.getScrollOffset() + viewport - 1);
    sections.forEach((sec, i) => {
      if (!sec.more || paging.has(sectionKey(sec.section))) return;
      const foot = items.findIndex((it) => it.section === i && it.kind === 'foot');
      if (foot >= 0 && foot - NEAR_END_ROWS <= last) loadMoreOfSection(boardKey, sec.section);
    });
  }
  $effect(() => {
    const ref = listRef;
    const on = shown;
    if (!ref) return;
    untrack(() => {
      if (on) ref.revalidate();
      check();
    });
  });
  $effect(() => {
    void items;
    void paging;
    untrack(check);
  });
  $effect(() => {
    const ref = listRef;
    if (!ref) return;
    return ref.subscribeContentGeometry(() => untrack(check));
  });

  // The row whose field editor is open, and the field.
  let editing = $state.raw<{ readonly issue: string; readonly field: CardEditField } | null>(null);
  function onEditRow(row: RowView, field: CardEditField | null): void {
    editing = field ? { issue: row.card.id, field } : null;
  }

  // The row's menu, at the pointer or its button.
  let menu = $state.raw<{ x: number; y: number; row: RowView } | null>(null);
  function onRowMenu(row: RowView, event: MouseEvent): void {
    menu = { x: event.clientX, y: event.clientY, row };
  }
  /** The column the row's status is in, for Move to…; -1 for none. */
  function statusColumn(row: RowView): number {
    return held.shape.columns.findIndex((col) => col.statuses.some((st) => st.id === row.card.status.id));
  }
  function moveBy(row: RowView, move: MoveView): void {
    if (move.fields?.length) {
      onMoveWithFields(row, move);
      return;
    }
    void moveTicket(cardWriteContext(boardRef, row), move.id);
  }
  async function copy(text: string, what: string): Promise<void> {
    if (!(await copyToClipboard(text))) addToast('error', `Could not copy the ${what}.`);
  }

  // A press on a row, until it moves far enough to pick the row up or ends
  // as the click that opens the ticket.
  interface Press {
    readonly id: number;
    readonly el: HTMLElement;
    readonly row: RowView;
    readonly from: string;
    readonly sx: number;
    readonly sy: number;
    x: number;
    y: number;
    carry: Carry | null;
    starting: boolean;
  }
  let press: Press | null = null;

  function rowShowing(issue: string): { row: RowView; list: string } | null {
    for (const it of items) if (it.kind === 'row' && it.row.card.id === issue) return { row: it.row, list: sections[it.section].list };
    return null;
  }

  function onPointerDown(event: PointerEvent): void {
    if (compact || press || event.button !== 0 || (event.pointerType !== 'mouse' && event.pointerType !== 'pen')) return;
    // A press with Ctrl or Cmd held is a click with its own meaning.
    if (event.ctrlKey || event.metaKey) return;
    const target = event.target instanceof Element ? event.target : null;
    if (!target || target.closest('[data-card-mark]')) return;
    const el = target.closest<HTMLElement>('[data-testid="issues-backlog-row"]');
    const at = el?.dataset.issue ? rowShowing(el.dataset.issue) : null;
    if (!el || !at || cardBusy(boardKey, cardWriteContext(boardRef, at.row))) return;
    void loadCarry().catch(() => {});
    press = { id: event.pointerId, el, row: at.row, from: at.list, sx: event.clientX, sy: event.clientY, x: event.clientX, y: event.clientY, carry: null, starting: false };
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
        if (press === p) pickUp(mod, p);
      },
      (err: unknown) => {
        if (press === p) endPress();
        addErrorToast('Could not carry the ticket: the carry did not load. Reload the window to try again.', err);
      },
    );
  }

  /** The screen's one list as a carry tests it, read at pick-up: the item
   * under the pointer from the virtualizer's offsets, so a section partly
   * scrolled away still answers. */
  function backlogLayout(mod: typeof import('./listCarry'), scroller: HTMLElement, ref: LongListHandle): CarryLayout {
    const box = scroller.getBoundingClientRect();
    return {
      at(x, y) {
        if (x < box.left || x > box.right || y < box.top || y > box.bottom || items.length === 0) return null;
        const offset = scroller.scrollTop + y - box.top;
        const i = Math.min(ref.findItemIndex(offset), items.length - 1);
        const below = ref.holds(i) && offset > ref.getItemOffset(i) + ref.getItemSize(i) / 2;
        return spotAt(items, rowsShown, sections, i, below);
      },
      edgeScroll(_x, y, dt) {
        scroller.scrollTop += mod.edgeSpeed(y, box.top, box.bottom) * dt;
      },
    };
  }

  function pickUp(mod: typeof import('./listCarry'), p: Press): void {
    const scroller = scrollEl;
    const ref = listRef;
    if (!scroller || !ref) {
      endPress();
      return;
    }
    editing = null;
    menu = null;
    const ctx = cardWriteContext(boardRef, p.row);
    const id = ++carries;
    carrying = { id, row: p.row, ctx, from: p.from, spot: { list: p.from, slot: null }, height: p.el.getBoundingClientRect().height };
    const host: CarryHost = {
      layout: backlogLayout(mod, scroller, ref),
      from: p.from,
      over(spot) {
        const c = carrying;
        if (c?.id === id) carrying = { ...c, spot };
      },
      drop: (spot) => drop(p.row, p.from, spot),
      end() {
        if (carrying?.id === id) carrying = null;
      },
    };
    p.carry = mod.startCarry(p.el, p.sx, p.sy, p.x, p.y, { grab: false, flat: true }, host);
  }

  /** Where the row shows now, for the carried copy to glide to; null when
   * it is not mounted. */
  async function shownAt(issue: string): Promise<DOMRect | null> {
    await tick();
    const el = scrollEl?.querySelector<HTMLElement>(`[data-testid="issues-backlog-row"][data-issue="${CSS.escape(issue)}"]`);
    return el?.getBoundingClientRect() ?? null;
  }

  /** Ranks the row in its section, or moves it into another at the place
   * it was let go: before the row shown after the slot, the carried one
   * left out. */
  function drop(row: RowView, from: string, spot: CarrySpot | null): Promise<DOMRect | null> {
    const issue = row.card.id;
    if (!spot || spot.slot === null) return shownAt(issue);
    const at = sections.findIndex((s) => s.list === spot.list);
    if (at < 0) return shownAt(issue);
    const rows = rowsShown[at];
    const ownAt = rows.findIndex((r) => r.card.id === issue);
    const k = ownAt >= 0 && ownAt < spot.slot ? spot.slot - 1 : spot.slot;
    if (spot.list === from && k === ownAt) return shownAt(issue);
    const before = rows.filter((r) => r.card.id !== issue)[k]?.card.id ?? null;
    const ctx = cardWriteContext(boardRef, row);
    if (spot.list === from) void rankCard(boardKey, ctx, boardRef.board, from, before);
    else void placeCard(boardKey, ctx, boardRef.board, spot.list, before);
    return shownAt(issue);
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
  });

  /** What a section's foot says while it is being read, failed or unread. */
  function footNote(sec: BacklogSection): string | null {
    const k = sectionKey(sec.section);
    if (paging.has(k)) return sec.unread ? 'Loading…' : 'Loading more…';
    if (sectionErrors.has(k)) return sec.unread ? 'Could not read this section.' : 'Could not read more of this section.';
    if (sec.unread) return 'Not read yet';
    return null;
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  bind:this={scrollEl}
  class="relative min-h-0 flex-1 overflow-y-auto pb-6 focus:outline-none"
  style:overflow-anchor="none"
  tabindex="-1"
  data-pane-scroller
  data-testid="issues-backlog"
  data-carrying={carrying ? '' : undefined}
  onpointerdown={onPointerDown}
>
  {#if kind === 'linear' && held.shape.plansSprints}
    <p class="px-3 pt-2 text-xs text-fg-muted" data-testid="issues-backlog-cycles">Linear runs this team's cycles from its settings: they start and end there.</p>
  {/if}
  {#if sections.length === 0}
    <p class="px-3 py-2 text-sm text-fg-muted" data-testid="issues-backlog-none">This board has no backlog.</p>
  {:else}
    <LongListVirtualizer
      bind:this={listRef}
      data={items}
      {getKey}
      scrollRef={scrollEl}
      {estimate}
      renderAll={IS_TEST}
      {applyScrollTarget}
      onscroll={check}
    >
      {#snippet children(item: BacklogItem)}
        {@const sec = sections[item.section]}
        <div
          class="{carrying ? 'transition-transform duration-150 ease-out motion-reduce:transition-none' : ''}"
          style:transform={carrying && shiftOf(item.at) !== 0 ? `translateY(${shiftOf(item.at)}px)` : undefined}
          data-carry-list={sec.list}
        >
          {#if item.kind === 'head'}
            {@const target = !!carrying?.spot && carrying.spot.list === sec.list && carrying.from !== sec.list}
            <BacklogSectionHeader
              {sec}
              numbers={numbers[item.section]}
              {unit}
              open={isOpen(sec)}
              {now}
              {target}
              onFold={(open) => setBacklogSectionOpen(boardRef, sec.list, open)}
              controls={sprintWrites ? sprintControls : undefined}
            />
          {:else if item.kind === 'row'}
            <div class={carrying?.spot && carrying.row.card.id === item.row.card.id ? 'opacity-0' : ''}>
              <BacklogRow
                row={item.row}
                {boardRef}
                {hidden}
                {unit}
                {typed}
                {compact}
                open={item.row.card.id === openIssue}
                carried={carrying?.row.card.id === item.row.card.id}
                editing={editing?.issue === item.row.card.id ? editing.field : null}
                onOpen={onOpenCard}
                onMenu={onRowMenu}
                onEdit={onEditRow}
              />
            </div>
          {:else}
            {@const note = footNote(sec)}
            <div class="flex min-h-[34px] flex-col gap-1 px-3 pt-1" data-testid="issues-backlog-foot" data-list={sec.list}>
              {#if note}
                <p class="text-xs text-fg-muted" data-testid="issues-backlog-foot-note">{note}</p>
              {:else if rowsShown[item.section].length === 0}
                <p class="rounded-[var(--radius-field)] border border-dashed border-border px-3 py-2 text-center text-xs text-fg-muted" data-testid="issues-backlog-empty">
                  {filtered ? (sec.more ? 'No matching tickets among those loaded' : 'No matching tickets') : compact ? 'No tickets' : 'Drop tickets here'}
                </p>
              {/if}
              <BacklogCreate {b} {boardRef} {sec} project={held.shape.project} {typed} />
            </div>
          {/if}
          {#snippet sprintControls()}
            <SprintControls {b} {sec} numbers={numbers[item.section]} {unit} sprints={held.shape.sprints} />
          {/snippet}
        </div>
      {/snippet}
    </LongListVirtualizer>
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
      from={statusColumn(open.row)}
      {typed}
      url={cardURL(held.shape, open.row.card)}
      moving={cardBusy(boardKey, ctx)}
      onOpen={() => onOpenCard(open.row, false)}
      onOwnPane={() => onOpenCard(open.row, true)}
      onOpenURL={() => { void handleExternalURL(cardURL(held.shape, open.row.card)); }}
      onCopy={(text, what) => { void copy(text, what); }}
      onEdit={(field) => onEditRow(open.row, field)}
      onMove={(_column, move) => moveBy(open.row, move)}
      onSprint={(sprint) => { void moveTicketToSprint(ctx, boardRef.board, sprint); }}
      onDismiss={() => { menu = null; }}
    />
  {/key}
{/if}
