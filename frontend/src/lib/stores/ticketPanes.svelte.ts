// Ticket panes: a tracker ticket in a pane of its own (docs/specs/issues.md,
// "A ticket in its own pane"). A ticket pane is a layout item of kind
// `ticket` under a generated id, and a record here: the ticket it shows,
// the tickets Back steps through, and the board it was opened from (its
// points are that board's row; none after a reload, when they are the first
// board's the ticket is on). The layout saves what names the ticket (its
// computer, connection and issue) and nothing a tracker wrote: a restored
// pane learns the ticket's key and title from its subscription, and its
// connection's tracker and the person's account there are read from the
// connection as the app knows it now (components/issues/TicketHost.svelte),
// never kept here.
//
// Who owns what:
// - Running: the layout item owns that the pane exists, its place and its
//   width; the record owns which ticket it shows. Nothing else names the
//   ticket.
// - Saving: the layout's save reads the record of each ticket item
//   (`ticketPaneCarrier`: computer, connection, issue); an item with no
//   record is saved with no ticket.
// - Restoring: the saved entry owns the ticket until the restore writes
//   the record (`restoreTicketPane`), before the layout takes the item. An
//   entry whose ticket does not read back comes back as an item with no
//   record, which says so until the person closes it
//   (components/panes/TicketPane.svelte).
//
// Must keep true:
// - A record exists only while its layout item does: every way an item
//   leaves the layout (its close, a thread or tool taking its slot, a
//   layout reset or restore) drops the record, here, through the layout's
//   removal listener. An item has no record only when its saved ticket did
//   not read back.
// - A move's bridge (`handoff`) is the new hold taken before the Issues
//   pane lets go of the ticket, since the pane's body loads after the
//   move. It ends when the pane's body holds the ticket itself, when the
//   ticket's computer is gone, or with the pane (its record dropped).
//   Nothing else holds it, and nothing outlives a reload: the page and its
//   subscriptions go together.
// - One pane per ticket for a person's open: opening a ticket a pane shows
//   focuses and reveals that pane. A pane that steps to a ticket another
//   pane shows is not merged with it.
// - Only a person's open asks for the focus (Ctrl/Cmd+click or "Open in
//   its own pane", which opens a pane or focuses the one showing the
//   ticket, and the pane's own open of a linked ticket, Back and Escape);
//   a restore does not (`utils/openFocusRequest.svelte.ts`).
// - Not offered under compact: a ticket pane has no row anywhere to reach
//   it again there (`ticketPanesOffered`); opening one there throws.
//
// This module is in the startup graph and imports no Issues store; the
// Issues chunk reads and writes a pane's ticket through it
// (`stores/issuesPane.svelte#createTicketTrail`) and keeps the drafts of
// every ticket a record names (`ticketPaneRefs`, `onTicketPanesChanged`).

import { SvelteMap } from 'svelte/reactivity';
import type { OpenTicket } from './issuesPane.svelte';
import { isCompactLayout } from './layoutMode.svelte';
import {
  addPaneLayoutItem,
  averagePaneWidthPx,
  notePaneLayoutMutation,
  onPaneLayoutItemsRemoved,
  paneLayoutKindOf,
  requestPaneLayoutPersistence,
} from './paneLayout.svelte';
import { focusPane, removeSurfacePaneFromLayout, resolveNewPaneInsertIndex, revealPane } from './panes.svelte';
import type { TrackerTicketRef } from './trackerTicket.svelte';
import { activationIntent } from '../utils/modClick';
import { createOpenFocusRequest, type OpenFocusRequest } from '../utils/openFocusRequest.svelte';
import { randomId } from '../utils/randomId';
import type { PaneLayoutPersistedTicket } from '../types/settings';

/** What a ticket pane is saved as. */
export type TicketPaneCarrier = Readonly<PaneLayoutPersistedTicket>;

/** A ticket pane's state. Replaced whole on every change. */
export interface TicketPaneRecord {
  readonly ticket: OpenTicket;
  /** The tickets Back steps through, oldest first. */
  readonly trail: readonly OpenTicket[];
  /** The board it was opened from; null after a reload. */
  readonly board: string | null;
  /** The focus the pane's opens ask for. */
  readonly focus: OpenFocusRequest<OpenTicket>;
}

const records = new SvelteMap<string, TicketPaneRecord>();
// What holds the ticket until the pane's body subscribes it (a moved
// ticket's subscription), released once it has or with the pane. Not
// state: nothing draws it.
const handoffs = new Map<string, () => void>();
const changeListeners = new Set<() => void>();

function changed(): void {
  for (const listener of changeListeners) listener();
}

/** The one rule for whether two refs name the same ticket. */
export function sameTicketRef(a: TrackerTicketRef, b: TrackerTicketRef): boolean {
  return a.backend === b.backend && a.connection === b.connection && a.issue === b.issue;
}

function drop(paneId: string): void {
  const record = records.get(paneId);
  if (!record) return;
  record.focus.end();
  takeTicketPaneHandoff(paneId)?.();
  records.delete(paneId);
  changed();
}

onPaneLayoutItemsRemoved((paneIds) => {
  for (const paneId of paneIds) drop(paneId);
});

/** Reactive: whether a ticket may open in a pane of its own here. */
export function ticketPanesOffered(): boolean {
  return !isCompactLayout();
}

/** The one rule for whether a press of a ticket opens it in a pane of its
 * own: Ctrl or Cmd held (`utils/modClick.ts`), where such panes are
 * offered. */
export function opensTicketPane(event: MouseEvent | KeyboardEvent): boolean {
  return activationIntent(event).openInNewPane && ticketPanesOffered();
}

export interface OpenTicketPaneOptions {
  /** The board it is opened from; null where none is known (a restored
   * pane's link). */
  readonly board: string | null;
  /** Releases what holds the ticket until the pane's body subscribes it. */
  readonly handoff?: () => void;
}

/**
 * A person opens `open` in a pane of its own: the pane showing it already
 * is focused and revealed, else a new pane opens right of the focused one
 * at the average pane width. Either takes the focus. Returns the pane id.
 * Throws under compact (`ticketPanesOffered`).
 */
export function openTicketPane(open: OpenTicket, options: OpenTicketPaneOptions): string {
  if (!ticketPanesOffered()) throw new Error('openTicketPane: a ticket does not open in a pane of its own under the compact layout');
  for (const [paneId, record] of records) {
    if (!sameTicketRef(record.ticket.ref, open.ref)) continue;
    options.handoff?.();
    focusPane(paneId);
    revealPane(paneId);
    record.focus.ask(record.ticket);
    return paneId;
  }
  const paneId = `ticket-${randomId()}`;
  const focus = createOpenFocusRequest<OpenTicket>();
  records.set(paneId, { ticket: open, trail: [], board: options.board, focus });
  if (options.handoff) handoffs.set(paneId, options.handoff);
  notePaneLayoutMutation();
  addPaneLayoutItem({ id: paneId, paneId, kind: 'ticket', widthPx: averagePaneWidthPx() }, resolveNewPaneInsertIndex());
  focusPane(paneId);
  revealPane(paneId);
  focus.ask(open);
  changed();
  return paneId;
}

/** Reactive: the pane's state, null for an id that is not a ticket pane. */
export function ticketPaneRecord(paneId: string): TicketPaneRecord | null {
  return records.get(paneId) ?? null;
}

/** The pane now shows `ticket` with `trail` behind it. A change of the
 * ticket is saved with the layout. */
export function setTicketPaneTicket(paneId: string, ticket: OpenTicket, trail: readonly OpenTicket[]): void {
  const record = records.get(paneId);
  if (!record) throw new Error(`setTicketPaneTicket: ${paneId} is not a ticket pane`);
  if (record.ticket === ticket && record.trail === trail) return;
  records.set(paneId, { ...record, ticket, trail });
  if (record.ticket !== ticket) requestPaneLayoutPersistence();
  changed();
}

/** What holds the pane's ticket until its body subscribes it, taken: the
 * body releases it once it has. */
export function takeTicketPaneHandoff(paneId: string): (() => void) | null {
  const handoff = handoffs.get(paneId) ?? null;
  handoffs.delete(paneId);
  return handoff;
}

/** Closes the ticket pane `paneId`; throws for any other pane. Focus
 * moves to the adjacent pane. */
export function closeTicketPane(paneId: string): void {
  if (paneLayoutKindOf(paneId) !== 'ticket') throw new Error(`closeTicketPane: ${paneId} is not a ticket pane`);
  removeSurfacePaneFromLayout(paneId);
}

/** The ticket of every pane and of every pane's Back trail. */
export function ticketPaneRefs(): TrackerTicketRef[] {
  const refs: TrackerTicketRef[] = [];
  for (const record of records.values()) {
    refs.push(record.ticket.ref);
    for (const back of record.trail) refs.push(back.ref);
  }
  return refs;
}

/** Calls `listener` after any pane's tickets change, a pane opens or one
 * goes. */
export function onTicketPanesChanged(listener: () => void): () => void {
  changeListeners.add(listener);
  return () => { changeListeners.delete(listener); };
}

/** The pane as the layout saves it; null for an id that is not a ticket
 * pane. */
export function ticketPaneCarrier(paneId: string): TicketPaneCarrier | null {
  const ref = records.get(paneId)?.ticket.ref;
  return ref ? { backend: ref.backend, connection: ref.connection, issue: ref.issue } : null;
}

/** A saved pane comes back on the ticket it showed, with no trail and no
 * text of it until its subscription answers; it asks for no focus. Call
 * before its layout item is placed. */
export function restoreTicketPane(paneId: string, carrier: TicketPaneCarrier): void {
  const ticket: OpenTicket = {
    ref: { backend: carrier.backend, connection: carrier.connection, issue: carrier.issue },
    seed: { key: '', title: '', card: null },
  };
  records.get(paneId)?.focus.end();
  records.set(paneId, { ticket, trail: [], board: null, focus: createOpenFocusRequest<OpenTicket>() });
  changed();
}

export function resetTicketPanesForTest(): void {
  for (const paneId of records.keys()) drop(paneId);
  handoffs.clear();
}
