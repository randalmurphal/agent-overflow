// What moving a Board card leaves while its write is out
// (docs/specs/issues.md, "Board"): the card waits where it was dropped,
// drawn there by the Board screen (`trackerBoardModel.ts#withWaiting`),
// until the write settles. What must hold:
// - a waiting card is a picture of a pending write: it never changes what
//   the board holds, and it goes the moment its write settles, whatever
//   the outcome, so a refused or unknown write shows the card where the
//   board holds it with its note saying why;
// - an applied write settles only once the board holds what it was told
//   (`trackerTicketWrites.svelte.ts`, `trackerBoardsSettled`), so the card
//   does not jump back before its new place arrives;
// - a card waits for at most one write: none is sent while the card's
//   status or rank write is out.
// The writes are the ticket's own (`moveTicket`, `rankTicket`), so the
// ticket screen shows the same write; the carry (components/issues/
// boardCarry.ts) and the card's Move to… both move through `moveCard`.

import { SvelteMap } from 'svelte/reactivity';
import type { MoveView, RowView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import type { WaitingCard } from './trackerBoardModel';
import { trackerTicketKey } from './trackerTicket.svelte';
import { moveTicket, rankTicket, ticketWrite, type TicketWriteContext } from './trackerTicketWrites.svelte';
import type { TrackerBoardRef } from './trackerBoard.svelte';
import type { WriteOutcome } from './trackerWriteOutcome';

const waiting = new SvelteMap<string, { readonly board: string; readonly card: WaitingCard }>();

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

/** Whether a move or rank of the card is out: it cannot be moved again
 * until it settles. */
export function cardMovePending(ctx: TicketWriteContext): boolean {
  return (ticketWrite(ctx.key, 'status')?.pending ?? false) || (ticketWrite(ctx.key, 'rank')?.pending ?? false);
}

async function waitOn(board: string, card: WaitingCard, send: () => Promise<WriteOutcome | null>): Promise<WriteOutcome | null> {
  const k = waitKey(board, card.row.card.id);
  waiting.set(k, { board, card });
  try {
    return await send();
  } finally {
    if (waiting.get(k)?.card === card) waiting.delete(k);
  }
}

/** Moves the card into `column` by `move`, one that takes no fields; it
 * waits there, where its rank places it, while the move is out. */
export function moveCard(board: string, ctx: TicketWriteContext, row: RowView, column: number, move: MoveView): Promise<WriteOutcome | null> {
  if (cardMovePending(ctx)) return Promise.resolve(null);
  return waitOn(board, { row, column }, () => moveTicket(ctx, move.id));
}

/** Ranks the card in its column between `prev` and `next` (issue IDs,
 * empty for an end) on the tracker board `trackerBoard`; it waits before
 * `next` while the rank is out. */
export function rankCard(board: string, ctx: TicketWriteContext, trackerBoard: string, row: RowView, column: number, prev: string, next: string): Promise<WriteOutcome | null> {
  if (cardMovePending(ctx) || (!prev && !next)) return Promise.resolve(null);
  return waitOn(board, { row, column, before: next || null }, () => rankTicket(ctx, trackerBoard, prev, next));
}

export function resetTrackerBoardCarryForTest(): void {
  waiting.clear();
}
