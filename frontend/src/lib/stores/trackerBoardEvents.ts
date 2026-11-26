// What the always-loaded stores tell the Issues stores, which load with the
// Issues pane (`components/panes/ToolPane.svelte` imports it on demand), and
// the tracker setup store, which loads with a ticket or a Settings page: a
// store there registers here when it loads. Until one has, no board or
// ticket is held and no pane holds the board choices, so a frame is dropped
// and a gap has nothing to read again. This module is all of the Issues pane the startup
// chunk holds (`lib/architecture.test.ts` checks it).

import type { BackendKey } from '../transport/backendKey';
import type { FailureView, SyncView } from '../../../bindings/agent-overflow/internal/trackerapp/models';

/** The `tracker:board_updated` frame (trackerapp.BoardEvent). Wails
 * generates no type for an event payload. */
export interface TrackerBoardEvent {
  scope: { connection: string; board?: string };
  seq: number;
  changed?: string[] | null;
  removed?: string[] | null;
  truncated?: boolean;
  read_again?: boolean;
  sync?: SyncView;
  ended?: boolean;
}

/** The `tracker:issue_updated` frame (trackerapp.IssueEvent). */
export interface TrackerIssueEvent {
  connection: string;
  issue: string;
  seq: number;
  changed?: boolean;
  failure?: FailureView | null;
  poker?: boolean;
  ended?: boolean;
}

/** What a store does with a channel: a frame, and a gap that answers the
 * settling of the reads it starts, or undefined when nothing of the
 * computer is held. */
interface TrackerChannel<E> {
  event(event: E, backend: BackendKey): void;
  gap(backend: BackendKey): Promise<void> | undefined;
}

let boards: TrackerChannel<TrackerBoardEvent> | null = null;
let tickets: TrackerChannel<TrackerIssueEvent> | null = null;
const listsChanged = new Set<(backend: BackendKey) => void>();
const setupUpdated = new Set<(backend: BackendKey) => void>();

/** The board store takes the channel when it loads. */
export function takeTrackerBoardChannel(channel: TrackerChannel<TrackerBoardEvent>): void {
  boards = channel;
}

/** A `tracker:board_updated` frame from `backend`. */
export function trackerBoardUpdated(event: TrackerBoardEvent, backend: BackendKey): void {
  boards?.event(event, backend);
}

/** A gap on `tracker:board_updated` from `backend`; see `TrackerChannel.gap`. */
export function trackerBoardsGap(backend: BackendKey): Promise<void> | undefined {
  return boards?.gap(backend);
}

/** The ticket store takes the channel when it loads. */
export function takeTrackerTicketChannel(channel: TrackerChannel<TrackerIssueEvent>): void {
  tickets = channel;
}

/** A `tracker:issue_updated` frame from `backend`. */
export function trackerIssueUpdated(event: TrackerIssueEvent, backend: BackendKey): void {
  tickets?.event(event, backend);
}

/** A gap on `tracker:issue_updated` from `backend`. */
export function trackerTicketsGap(backend: BackendKey): Promise<void> | undefined {
  return tickets?.gap(backend);
}

/** Runs `listener` whenever the boards some project of a computer lists may
 * have changed; the returned function stops it. */
export function onTrackerBoardListsChanged(listener: (backend: BackendKey) => void): () => void {
  listsChanged.add(listener);
  return () => { listsChanged.delete(listener); };
}

/** The boards some project of `backend` lists may have changed: its
 * connections changed, or this window saved a project's boards. */
export function trackerBoardListsChanged(backend: BackendKey): void {
  for (const listener of listsChanged) listener(backend);
}

/** Runs `listener` on every `tracker:setup_updated` from a computer: some
 * project's tracker setup there changed as it is stored. */
export function onTrackerSetupUpdated(listener: (backend: BackendKey) => void): void {
  setupUpdated.add(listener);
}

/** A `tracker:setup_updated` frame from `backend`. */
export function trackerSetupUpdated(backend: BackendKey): void {
  for (const listener of setupUpdated) listener(backend);
}
