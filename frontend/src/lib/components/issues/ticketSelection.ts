// What a ticket list's selection does (docs/specs/issues.md, "Dashboards"):
// the statuses every selected ticket can be moved to, the people every one
// can be assigned, and the batch that sends one write per ticket. Each
// write is the ticket write store's (trackerTicketWrites.svelte.ts), so a
// ticket's outcome shows under its row on a shown list, or in a toast.
//
// Nothing below the frontend bounds how many of a batch's calls run at
// once: the tracker's HTTP client multiplexes them over one HTTP/2
// connection and paces a site only once it has said it is rate limited
// (a 429, or Linear's RATELIMITED answer). So the batches do: at most
// BULK_AT_ONCE of their reads and writes are out at a time, all lists'
// batches together. A read for a picker that closed before its turn is
// not sent (`signal`).

import type { MoveView, UserListView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import type { PickItem, PickList } from './TicketPicker.svelte';
import { pickedEdit } from './ticketEditors';
import { keyPrefix } from '../../stores/ticketWork.svelte';
import { assignableUsers, editTicketFields, fieldTarget, moveTicket, ticketMoves, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
import type { WriteOutcome } from '../../stores/trackerWriteOutcome';
import { userFacingError } from '../../utils/userFacingError';

/** How many calls of one batch are out at a time. */
export const BULK_AT_ONCE = 4;

/** A selected ticket: where it is written, and the status it is in. */
export interface ListTicket {
  readonly ctx: TicketWriteContext;
  /** The status's name: where this list last moved it, else as its row
   * shows it. */
  readonly status: string;
}

// The calls of every batch that are out, and the ones waiting for a slot.
let out = 0;
const waiting: (() => void)[] = [];

/** Takes one of the batches' BULK_AT_ONCE slots: answers its release
 * once taken, which passes it to the next waiting call or frees it. A
 * call still waiting when `signal` aborts leaves the line and rejects. */
function takeSlot(signal?: AbortSignal): Promise<() => void> {
  const release = () => {
    const next = waiting.shift();
    if (next) next();
    else out--;
  };
  signal?.throwIfAborted();
  if (out < BULK_AT_ONCE) {
    out++;
    return Promise.resolve(release);
  }
  return new Promise((resolve, reject) => {
    const take = () => {
      signal?.removeEventListener('abort', leave);
      resolve(release);
    };
    const leave = () => {
      waiting.splice(waiting.indexOf(take), 1);
      reject(signal!.reason);
    };
    signal?.addEventListener('abort', leave, { once: true });
    waiting.push(take);
  });
}

/** Runs `step` on each item, with every batch's calls together at most
 * BULK_AT_ONCE out at a time, and answers each result in the items'
 * order. A step that throws is its result's rejection; the others still
 * run. An item still waiting for its turn when `signal` aborts is not run,
 * and its result is the abort. */
export function eachBounded<T, R>(items: readonly T[], step: (item: T) => Promise<R>, signal?: AbortSignal): Promise<PromiseSettledResult<R>[]> {
  return Promise.allSettled(items.map(async (item) => {
    const release = await takeSlot(signal);
    try {
      return await step(item);
    } finally {
      release();
    }
  }));
}

export function resetTicketSelectionForTest(): void {
  out = 0;
  waiting.length = 0;
}

/** A status the selection may be moved to: the move each ticket takes
 * there (null for a ticket already in it, which is left as it is), or why
 * it cannot be picked. */
export interface ListStatus {
  readonly id: string;
  readonly name: string;
  /** The move of each ticket by its key; null for one already there. */
  readonly moves: ReadonlyMap<string, string | null>;
  /** Why it cannot be picked; '' when it can. */
  readonly off: string;
}

/** The statuses some selected ticket can move to, in the order the
 * tracker first lists them, from each ticket's moves (`moves[i]` is
 * `tickets[i]`'s). A ticket takes the first move to a status that asks for
 * no fields; a status that some ticket reaches only by a move that asks
 * for fields, or does not reach, is off, naming the ticket. A ticket
 * already in a status needs no move there: Jira lists a move into the
 * status an issue is in when its workflow lets every status move there. */
export function commonMoves(tickets: readonly ListTicket[], moves: readonly (readonly MoveView[])[]): ListStatus[] {
  const order: { id: string; name: string }[] = [];
  const seen = new Set<string>();
  for (const list of moves) {
    for (const m of list) {
      if (seen.has(m.to.id)) continue;
      seen.add(m.to.id);
      order.push({ id: m.to.id, name: m.to.name || m.to.id });
    }
  }
  return order.map(({ id, name }) => {
    const plan = new Map<string, string | null>();
    const asks: string[] = [];
    const unreached: string[] = [];
    tickets.forEach((t, i) => {
      if (t.status === name) {
        plan.set(t.ctx.key, null);
        return;
      }
      const to = moves[i].filter((m) => m.to.id === id);
      const plain = to.find((m) => !m.fields?.length);
      if (plain) plan.set(t.ctx.key, plain.id);
      else if (to.length > 0) asks.push(t.ctx.label);
      else unreached.push(t.ctx.label);
    });
    let off = '';
    if (unreached.length > 0) off = `${named(unreached)} cannot move to ${name} from where ${unreached.length === 1 ? 'it is' : 'they are'}`;
    else if (asks.length > 0) off = `Moving ${named(asks)} to ${name} asks for fields; move ${asks.length === 1 ? 'it' : 'them'} from the ticket`;
    else if ([...plan.values()].every((m) => m === null)) off = `${tickets.length === 1 ? 'It is' : 'They are all'} in ${name} already`;
    return { id, name, moves: plan, off };
  });
}

/** "WEB-1", "WEB-1 and WEB-2", or "WEB-1 and 3 more". */
function named(labels: readonly string[]): string {
  if (labels.length === 1) return labels[0];
  if (labels.length === 2) return `${labels[0]} and ${labels[1]}`;
  return `${labels[0]} and ${labels.length - 1} more`;
}

/** Each ticket's moves, or why they could not be read. */
export type MovesRead = { readonly moves: readonly (readonly MoveView[])[] } | { readonly failed: readonly string[] };

/** Reads where each ticket can move (TrackerMoves, one tracker request
 * each), at most BULK_AT_ONCE at a time, until `signal` aborts. */
export async function readMoves(tickets: readonly ListTicket[], signal: AbortSignal): Promise<MovesRead> {
  const read = await eachBounded(tickets, (t) => ticketMoves(t.ctx), signal);
  const failed = read.flatMap((r, i) => r.status === 'rejected' ? [`Could not read where ${tickets[i].ctx.label} can move: ${userFacingError(r.reason)}`] : []);
  if (failed.length > 0) return { failed };
  return { moves: read.map((r) => (r as PromiseFulfilledResult<MoveView[]>).value) };
}

/** What a batch of writes came to: the tickets the tracker made it on, by
 * key, and how many were asked. A ticket whose write of the same target
 * was already out is not sent again and counts as not made. */
export interface BatchOutcome {
  readonly made: ReadonlySet<string>;
  readonly sent: number;
}

// Each write is the store's from the press, so its row says "Saving…"
// while it waits for a slot, and is sent once it has one.
async function sendEach(
  tickets: readonly TicketWriteContext[],
  send: (t: TicketWriteContext, after: Promise<void>) => Promise<WriteOutcome | null>,
): Promise<BatchOutcome> {
  // The write store keeps each failure as its outcome (under the row or in
  // a toast) and never throws one; a throw here is a defect, and is said.
  const results = await Promise.allSettled(tickets.map(async (t) => {
    const slot = takeSlot();
    try {
      return await send(t, slot.then(() => undefined));
    } finally {
      (await slot)();
    }
  }));
  for (const r of results) if (r.status === 'rejected') throw r.reason;
  const made = new Set(tickets.filter((_, i) => {
    const r = results[i];
    return r.status === 'fulfilled' && r.value?.kind === 'applied';
  }).map((t) => t.key));
  return { made, sent: tickets.length };
}

/** Moves each ticket of `status.moves` that is not there yet by its move. */
export function moveTickets(tickets: readonly ListTicket[], status: ListStatus): Promise<BatchOutcome> {
  const moving = tickets.filter((t) => status.moves.get(t.ctx.key));
  return sendEach(moving.map((t) => t.ctx), (t, after) => moveTicket(t, status.moves.get(t.key)!, undefined, after));
}

/** Assigns each ticket to `person`, or unassigns it for null. */
export function assignTickets(tickets: readonly ListTicket[], person: PickItem | null): Promise<BatchOutcome> {
  const edit = pickedEdit('assignee', 'user', person ? [person] : []);
  return sendEach(tickets.map((t) => t.ctx), (t, after) => editTicketFields(t, fieldTarget('assignee'), [edit], after));
}

/** The people whose name starts with `text` who may be assigned every
 * selected ticket: one read per project, by the first ticket of each key
 * prefix, at most BULK_AT_ONCE at a time until `signal` aborts, and the
 * people every answer names, in the first answer's order. A project whose
 * read failed leaves nobody known to be assignable to every ticket, so
 * the read fails naming each such project and why. */
export async function assignablePeople(tickets: readonly ListTicket[], text: string, signal: AbortSignal): Promise<PickList> {
  const byProject = new Map<string, ListTicket>();
  for (const t of tickets) if (!byProject.has(keyPrefix(t.ctx.label))) byProject.set(keyPrefix(t.ctx.label), t);
  const asked = [...byProject.entries()];
  const read = await eachBounded(asked, ([, t]) => assignableUsers(t.ctx, text), signal);
  const failed = read.flatMap((r, i) => r.status === 'rejected' ? [`Could not read who may be assigned ${asked[i][0]} tickets: ${userFacingError(r.reason)}`] : []);
  if (failed.length > 0) throw new Error(failed.join(' '));
  let people: PickItem[] | null = null;
  let more = false;
  for (const r of read as PromiseFulfilledResult<UserListView>[]) {
    more ||= r.value.more ?? false;
    const ids = new Set((r.value.users ?? []).map((u) => u.id));
    people = people === null
      ? (r.value.users ?? []).map((u) => ({ id: u.id, name: u.name || u.id }))
      : people.filter((p) => ids.has(p.id));
  }
  return { items: people ?? [], more };
}
