// What moving a Board card or a Backlog row leaves while its write is out
// (docs/specs/issues.md, "Board" and "Backlog and sprint planning"): the
// ticket waits where it was dropped, in a list (a Board column or a
// Backlog section, `trackerBoardModel.ts#columnList`, `sectionList`), drawn
// there by its screen (`trackerBoardModel.ts#withWaiting`), until the
// write settles. What must hold:
// - a waiting card is a picture of a pending write: it never changes what
//   the board holds, it is drawn from the row the board holds now, and it
//   goes the moment its write settles, whatever the outcome, so a refused
//   or unknown write shows the card where the board holds it with its note
//   saying why;
// - an applied write settles only once the board holds what it was told
//   (`trackerTicketWrites.svelte.ts`, `trackerBoardsSettled`), so the card
//   does not jump back before its new place arrives;
// - a card has one drop at a time: from a release over another Board
//   column that was accepted (`dropOwner`, its rules written beside it)
//   until its move is sent or the drop ends with none, and then while its
//   status, rank or sprint write is out, no other move, rank or sprint
//   move of it is made, from a carry or from its menu (`cardBusy`). A
//   Backlog drop asks nothing before it writes, so its write is pending
//   from the release and holds the row by itself;
// - the ranks and placed sprint moves into one list go one at a time:
//   each is pending from its drop, waits for the one before it into that
//   list to settle, and then names the card it was dropped before and the
//   card the board holds just before that one, so both stand in that
//   order on the tracker when it is sent.
// The writes are the ticket's own (`moveTicket`, `rankTicket`,
// `moveTicketToSprint`), so the ticket screen shows the same write; the
// carry (components/issues/listCarry.ts) and the menu's Move to… and Move
// to sprint… all move through here.

import { SvelteMap } from 'svelte/reactivity';
import type { MoveView, RowView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { heldTrackerBoard, type TrackerBoardRef } from './trackerBoard.svelte';
import { columnList, listRows, listSprint, type WaitingCard } from './trackerBoardModel';
import { trackerTicketKey } from './trackerTicket.svelte';
import { moveTicket, moveTicketToSprint, RankPlaceError, rankTicket, ticketWrites, type TicketWrite, type TicketWriteContext } from './trackerTicketWrites.svelte';
import type { WriteOutcome } from './trackerWriteOutcome';

const waiting = new SvelteMap<string, { readonly board: string; readonly card: WaitingCard }>();
// The cards with a drop under way, by board and issue.
const drops = new SvelteMap<string, symbol>();
// The last rank or placed sprint move into each list of each board, which
// the next one there waits for.
const ranks = new Map<string, Promise<void>>();

function waitKey(board: string, issue: string): string {
  return `${board}\n${issue}`;
}

/** The write context of a card of `board`: the ticket screen's, so one
 * write shows on both. */
export function cardWriteContext(board: TrackerBoardRef, row: RowView): TicketWriteContext {
  return issueWriteContext(board, row.card.id, row.card.key);
}

/** The write context of the issue `id`, keyed `key`, on `board`'s
 * connection; `key` empty names it as "the ticket". */
export function issueWriteContext(board: TrackerBoardRef, id: string, key: string): TicketWriteContext {
  const ref = { backend: board.backend, connection: board.connection, issue: id };
  return { key: trackerTicketKey(ref), ref, label: key || 'the ticket' };
}

/** Moves the card of `board` (trackerBoardKey) into `sprint` of the
 * tracker board `trackerBoard`, or to its backlog with none, unless it is
 * busy (`cardBusy`): the menu's Move to sprint… and a made ticket's move
 * into its sprint, on every screen. */
export function moveCardToSprint(board: string, ctx: TicketWriteContext, trackerBoard: string, sprint: string | null): Promise<WriteOutcome | null> {
  if (cardBusy(board, ctx)) return Promise.resolve(null);
  return moveTicketToSprint(ctx, trackerBoard, sprint);
}

/** The cards of the board (trackerBoardKey) waiting where they were
 * dropped; reactive. */
export function waitingCards(board: string): WaitingCard[] {
  const out: WaitingCard[] = [];
  for (const w of waiting.values()) if (w.board === board) out.push(w.card);
  return out;
}

/** Whether a move, rank or sprint move is out among a ticket's writes
 * (`ticketWrites`). */
export function movePending(writes: ReadonlyMap<string, TicketWrite>): boolean {
  return (writes.get('status')?.pending ?? false) || (writes.get('rank')?.pending ?? false) || (writes.get('sprint')?.pending ?? false);
}

/** Whether the card of `board` has a drop under way or a move, rank or
 * sprint move out: it is not moved again until that ends; reactive. */
export function cardBusy(board: string, ctx: TicketWriteContext): boolean {
  return drops.has(waitKey(board, ctx.ref.issue)) || movePending(ticketWrites(ctx.key));
}

/** A drop of a card under way, holding the card from its release until
 * it ends. */
export interface CardDrop {
  /** Whether it still holds the card: false once it ended, by any of the
   * ways `dropOwner` lists. */
  readonly holds: boolean;
  /** Sends its move, which ends it; sends nothing once it ended. */
  move(column: number, move: MoveView): Promise<WriteOutcome | null>;
  /** Ends it with no move. */
  end(): void;
}

/** The drops one Board screen accepted on one board. */
export interface DropOwner {
  /** Starts the one drop of the card, at a release over another column;
   * null when the card is busy (`cardBusy`) or the owner closed. */
  start(ctx: TicketWriteContext): CardDrop | null;
  /** Ends every drop it started that has not ended, and starts none
   * after. */
  close(): void;
}

/**
 * The owner of the drops a Board screen accepts on `board`, opened when
 * the screen shows that board and closed when it goes or shows another.
 * What a drop's hold keeps true:
 * - it is taken at an accepted release and held by exactly one owner,
 *   which holds no drop once it closed;
 * - it ends exactly once, at the first of: its move is sent (`move`; the
 *   move's pending write holds the card from then on); the screen decides
 *   no move (the lookup failed, no move reaches the column, the move
 *   takes fields and its form opens, its choice was dismissed or replaced
 *   by another drop's); the owner closes;
 * - a drop that ended sends nothing, so a lookup that answers after its
 *   owner closed moves nothing and asks nothing.
 * The screen ends a drop on each of its decisions; `close` ends the rest,
 * so no hold outlives the screen that can decide it.
 */
export function dropOwner(board: string): DropOwner {
  const open = new Set<() => void>();
  let closed = false;
  return {
    start(ctx) {
      if (closed || cardBusy(board, ctx)) return null;
      const k = waitKey(board, ctx.ref.issue);
      const mine = Symbol(k);
      drops.set(k, mine);
      let holds = true;
      const end = () => {
        if (!holds) return;
        holds = false;
        open.delete(end);
        if (drops.get(k) === mine) drops.delete(k);
      };
      open.add(end);
      return {
        get holds() {
          return holds;
        },
        move(column, move) {
          if (!holds) return Promise.resolve(null);
          // Pending from here on, so the drop hands its hold to the write.
          const sent = sendMove(board, ctx, column, move);
          end();
          return sent;
        },
        end,
      };
    },
    close() {
      closed = true;
      // Each end deletes itself, which a Set's iteration allows.
      for (const end of open) end();
    },
  };
}

async function waitOn(board: string, card: WaitingCard, send: () => Promise<WriteOutcome | null>): Promise<WriteOutcome | null> {
  const k = waitKey(board, card.issue);
  waiting.set(k, { board, card });
  try {
    return await send();
  } finally {
    if (waiting.get(k)?.card === card) waiting.delete(k);
  }
}

function sendMove(board: string, ctx: TicketWriteContext, column: number, move: MoveView): Promise<WriteOutcome | null> {
  return waitOn(board, { issue: ctx.ref.issue, list: columnList(column), status: move.to.id }, () => moveTicket(ctx, move.id));
}

/** Moves the card into `column` by `move`, one that takes no fields; it
 * waits there, where its rank places it, while the move is out. */
export function moveCard(board: string, ctx: TicketWriteContext, column: number, move: MoveView): Promise<WriteOutcome | null> {
  if (cardBusy(board, ctx)) return Promise.resolve(null);
  return sendMove(board, ctx, column, move);
}

/** Ranks the card in its list (`list`, a Board column or a Backlog
 * section) just before the card `before` (null: at the end) on the
 * tracker board `trackerBoard`; it waits there while the rank is out, and
 * the rank is sent once the list's write before it settled. */
export function rankCard(board: string, ctx: TicketWriteContext, trackerBoard: string, list: string, before: string | null): Promise<WriteOutcome | null> {
  if (cardBusy(board, ctx)) return Promise.resolve(null);
  const issue = ctx.ref.issue;
  return inTurn(board, list, { issue, list, before }, (after) => rankTicket(ctx, trackerBoard, () => neighbors(board, list, issue, before, 'ranked'), after));
}

/** Moves the row into the Backlog section `list` (a sprint of the tracker
 * board `trackerBoard`, or its backlog) just before the row `before`
 * (null: at the end): one placed sprint move, which waits there while it
 * is out and is sent once the list's write before it settled. Into a list
 * that holds no other row it is placed nowhere. */
export function placeCard(board: string, ctx: TicketWriteContext, trackerBoard: string, list: string, before: string | null): Promise<WriteOutcome | null> {
  const sprint = listSprint(list);
  if (sprint === undefined) throw new Error(`placeCard: ${list} is no sprint and no backlog`);
  if (cardBusy(board, ctx)) return Promise.resolve(null);
  const issue = ctx.ref.issue;
  const place = () => {
    const held = heldTrackerBoard(board);
    const at = held ? listRows(held, list) : null;
    // A list holding no other row has no place to name.
    if (at && at.rows.every((r) => r.card.id === issue)) return { prev: '', next: '' };
    return neighbors(board, list, issue, before, 'moved');
  };
  return inTurn(board, list, { issue, list, before }, (after) => moveTicketToSprint(ctx, trackerBoard, sprint, { place, after }));
}

/** Sends `write` once the list's write before it settled, the card
 * waiting at `card` meanwhile, and makes it the one the list's next write
 * waits for. */
function inTurn(board: string, list: string, card: WaitingCard, write: (after: Promise<void> | undefined) => Promise<WriteOutcome | null>): Promise<WriteOutcome | null> {
  const k = `${board}\n${list}`;
  const sent = waitOn(board, card, () => write(ranks.get(k)));
  const settled = sent.then(() => undefined, () => undefined);
  ranks.set(k, settled);
  void settled.then(() => {
    if (ranks.get(k) === settled) ranks.delete(k);
  });
  return sent;
}

/** The neighbors a write of `issue` before `before` in `list` names, from
 * the rows the board holds now: `before`, and the row just before it, or
 * the last row for none. `done` is what the write would have done, for
 * the sentence that says it did not. */
function neighbors(board: string, list: string, issue: string, before: string | null, done: 'ranked' | 'moved'): { prev: string; next: string } {
  const held = heldTrackerBoard(board);
  const at = held ? listRows(held, list) : null;
  if (!at) throw new RankPlaceError(`The board was closed before the ${done === 'ranked' ? 'rank' : 'move'} was sent, so it was not ${done}.`);
  const rows = at.rows.filter((r) => r.card.id !== issue);
  if (before === null) {
    const last = rows.at(-1);
    if (!last) throw new RankPlaceError(`It is the only card in ${at.name} now, so it was not ${done}.`);
    return { prev: last.card.id, next: '' };
  }
  const i = rows.findIndex((r) => r.card.id === before);
  if (i < 0) throw new RankPlaceError(`The card it was dropped before is no longer in ${at.name}, so it was not ${done}.`);
  return { prev: rows[i - 1]?.card.id ?? '', next: before };
}

export function resetTrackerBoardCarryForTest(): void {
  waiting.clear();
  drops.clear();
  ranks.clear();
}
