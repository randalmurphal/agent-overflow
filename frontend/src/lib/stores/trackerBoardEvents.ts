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
  /** One project's pins that changed (trackerapp.ProjectPins): only that
   * project's subscriptions hold them. */
  pins?: { project: string; pins: string[] } | null;
  sync?: SyncView;
  ended?: boolean;
}

/** The `tracker:dashboards_updated` frame (trackerapp.DashboardsChange):
 * the dashboards of `project`'s `board` changed, of every board of the
 * project when it names none, and `counts` says whether what they count
 * did; a limit's change counts nothing again. */
export interface TrackerDashboardsChange {
  project: string;
  board?: string;
  counts: boolean;
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

/** The `tracker:time_entries_updated` frame (trackerapp.TimeEntriesEvent):
 * the time entries of a connection's author changed on `days`, drafted by
 * `threads` among others. */
export interface TrackerTimeEntriesEvent {
  connection: string;
  author: string;
  days?: string[] | null;
  threads?: string[] | null;
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
// The weeks' store and the draft cards' store, each once it loads.
const timeEntries = new Set<TrackerChannel<TrackerTimeEntriesEvent>>();
const listsChanged = new Set<(backend: BackendKey) => void>();
const setupUpdated = new Set<(backend: BackendKey) => void>();
const dashboardsUpdated = new Set<(backend: BackendKey, change: TrackerDashboardsChange) => void>();

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

/** A store that shows time entries takes the channel when it loads. */
export function takeTrackerTimeEntriesChannel(channel: TrackerChannel<TrackerTimeEntriesEvent>): void {
  timeEntries.add(channel);
}

/** A `tracker:time_entries_updated` frame from `backend`. */
export function trackerTimeEntriesUpdated(event: TrackerTimeEntriesEvent, backend: BackendKey): void {
  for (const channel of timeEntries) channel.event(event, backend);
}

/** A gap on `tracker:time_entries_updated` from `backend`: what each store
 * reads again, settled together. */
export function trackerTimeEntriesGap(backend: BackendKey): Promise<void> | undefined {
  const reads = [...timeEntries].flatMap((channel) => channel.gap(backend) ?? []);
  return reads.length > 0 ? Promise.all(reads).then(() => undefined) : undefined;
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

/** Runs `listener` on every `tracker:dashboards_updated` from a computer:
 * a board's dashboards there, the widgets proposed on it or a limit,
 * changed; the returned function stops it. */
export function onTrackerDashboardsUpdated(listener: (backend: BackendKey, change: TrackerDashboardsChange) => void): () => void {
  dashboardsUpdated.add(listener);
  return () => { dashboardsUpdated.delete(listener); };
}

/** A `tracker:dashboards_updated` frame from `backend`. */
export function trackerDashboardsUpdated(backend: BackendKey, change: TrackerDashboardsChange): void {
  for (const listener of dashboardsUpdated) listener(backend, change);
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
