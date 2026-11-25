// What moving a Board card leaves while its write is out
// (docs/specs/issues.md, "Board"): the card waits where it was dropped,
// drawn there by the Board screen (`trackerBoardModel.ts#withWaiting`),
// until the write settles. What must hold:
// - a waiting card is a picture of a pending write: it never changes what
//   the board holds, it is drawn from the row the board holds now, and it
//   goes the moment its write settles, whatever the outcome, so a refused
//   or unknown write shows the card where the board holds it with its note
//   saying why;
// - an applied write settles only once the board holds what it was told
//   (`trackerTicketWrites.svelte.ts`, `trackerBoardsSettled`), so the card
//   does not jump back before its new place arrives;
// - a card has one drop at a time: from a release over another column
//   that was accepted (`startDrop`) until its move is sent or the drop
//   ends with none, and then while its status or rank write is out, no
//   other move or rank of it is made, from a carry or from its menu
//   (`cardBusy`);
// - the ranks of one column go one at a time: each is pending from its
//   drop, waits for the rank before it in that column to settle, and then
//   names the card it was dropped before and the card the board holds
//   just before that one, so both stand in that order on the tracker when
//   it is sent.
// The writes are the ticket's own (`moveTicket`, `rankTicket`), so the
// ticket screen shows the same write; the carry (components/issues/
// boardCarry.ts) and the card's Move to… both move through here.

import { SvelteMap } from 'svelte/reactivity';
import type { MoveView, RowView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { heldTrackerBoard, type TrackerBoardRef } from './trackerBoard.svelte';
import { screenColumns, type WaitingCard } from './trackerBoardModel';
import { trackerTicketKey } from './trackerTicket.svelte';
import { moveTicket, RankPlaceError, rankTicket, ticketWrites, type TicketWrite, type TicketWriteContext } from './trackerTicketWrites.svelte';
import type { WriteOutcome } from './trackerWriteOutcome';

const waiting = new SvelteMap<string, { readonly board: string; readonly card: WaitingCard }>();
// The cards with a drop under way, by board and issue.
const drops = new SvelteMap<string, symbol>();
// The last rank of each column of each board, which the next one there
// waits for.
const ranks = new Map<string, Promise<void>>();

function waitKey(board: string, issue: string): string {
  return `${board}\n${issue}`;
}

/** The write context of a card of `board`: the ticket screen's, so one
 * write shows on both. */
export function cardWriteContext(board: TrackerBoardRef, row: RowView): TicketWriteContext {
  const ref = { backend: board.backend, connection: board.connection, issue: row.card.id };
  return { key: trackerTicketKey(ref), ref, label: row.card.key };
}

/** The cards of the board (trackerBoardKey) waiting where they were
 * dropped; reactive. */
export function waitingCards(board: string): WaitingCard[] {
  const out: WaitingCard[] = [];
  for (const w of waiting.values()) if (w.board === board) out.push(w.card);
  return out;
}

/** Whether a move or rank is out among a ticket's writes
 * (`ticketWrites`). */
export function movePending(writes: ReadonlyMap<string, TicketWrite>): boolean {
  return (writes.get('status')?.pending ?? false) || (writes.get('rank')?.pending ?? false);
}

/** Whether the card of `board` has a drop under way or a move or rank
 * out: it is not moved again until that ends; reactive. */
export function cardBusy(board: string, ctx: TicketWriteContext): boolean {
  return drops.has(waitKey(board, ctx.ref.issue)) || movePending(ticketWrites(ctx.key));
}

/** A drop of a card under way: its move, which ends it, or its end with
 * none. */
export interface CardDrop {
  move(column: number, move: MoveView): Promise<WriteOutcome | null>;
  end(): void;
}

/** Starts the one drop of the card, at a release over another column;
 * null when the card is busy (`cardBusy`). */
export function startDrop(board: string, ctx: TicketWriteContext): CardDrop | null {
  if (cardBusy(board, ctx)) return null;
  const k = waitKey(board, ctx.ref.issue);
  const mine = Symbol(k);
  drops.set(k, mine);
  const end = () => {
    if (drops.get(k) === mine) drops.delete(k);
  };
  return {
    move(column, move) {
      // Pending from here on, so the drop hands its hold to the write.
      const sent = sendMove(board, ctx, column, move);
      end();
      return sent;
    },
    end,
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
  return waitOn(board, { issue: ctx.ref.issue, column, status: move.to.id }, () => moveTicket(ctx, move.id));
}

/** Moves the card into `column` by `move`, one that takes no fields; it
 * waits there, where its rank places it, while the move is out. */
export function moveCard(board: string, ctx: TicketWriteContext, column: number, move: MoveView): Promise<WriteOutcome | null> {
  if (cardBusy(board, ctx)) return Promise.resolve(null);
  return sendMove(board, ctx, column, move);
}

/** Ranks the card in its column just before the card `before` (null: at
 * the end) on the tracker board `trackerBoard`; it waits there while the
 * rank is out, and the rank is sent once the column's rank before it
 * settled. */
export function rankCard(board: string, ctx: TicketWriteContext, trackerBoard: string, column: number, before: string | null): Promise<WriteOutcome | null> {
  if (cardBusy(board, ctx)) return Promise.resolve(null);
  const k = `${board}\n${column}`;
  const issue = ctx.ref.issue;
  const sent = waitOn(board, { issue, column, before }, () => rankTicket(ctx, trackerBoard, () => neighbors(board, column, issue, before), ranks.get(k)));
  const settled = sent.then(() => undefined, () => undefined);
  ranks.set(k, settled);
  void settled.then(() => {
    if (ranks.get(k) === settled) ranks.delete(k);
  });
  return sent;
}

/** The neighbors a rank of `issue` before `before` names, from the rows
 * the board holds now: `before`, and the row just before it, or the last
 * row for none. */
function neighbors(board: string, column: number, issue: string, before: string | null): { prev: string; next: string } {
  const held = heldTrackerBoard(board);
  const col = held ? screenColumns(held)[column] : undefined;
  if (!col) throw new RankPlaceError('The board was closed before the rank was sent, so it was not ranked.');
  const rows = col.rows.filter((r) => r.card.id !== issue);
  if (before === null) {
    const last = rows.at(-1);
    if (!last) throw new RankPlaceError(`It is the only card in ${col.name} now, so it was not ranked.`);
    return { prev: last.card.id, next: '' };
  }
  const at = rows.findIndex((r) => r.card.id === before);
  if (at < 0) throw new RankPlaceError(`The card it was dropped before is no longer in ${col.name}, so it was not ranked.`);
  return { prev: rows[at - 1]?.card.id ?? '', next: before };
}

export function resetTrackerBoardCarryForTest(): void {
  waiting.clear();
  drops.clear();
  ranks.clear();
}
