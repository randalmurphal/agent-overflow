// What the always-loaded stores tell the Issues stores, which load with the
// Issues pane (`components/panes/ToolPane.svelte` imports it on demand): a
// store there registers here when it loads. Until one has, no board is held
// and no pane holds the board choices, so a frame is dropped and a gap has
// nothing to read again. This module is all of the Issues pane the startup
// chunk holds (`lib/architecture.test.ts` checks it).

import type { BackendKey } from '../transport/backendKey';
import type { SyncView } from '../../../bindings/agent-overflow/internal/trackerapp/models';

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

/** What the board store does with the channel. */
export interface TrackerBoardChannel {
  /** A frame from `backend`. */
  event(event: TrackerBoardEvent, backend: BackendKey): void;
  /** A gap on the channel from `backend`: answers the settling of the reads
   * it starts, or undefined when no board of the computer is held. */
  gap(backend: BackendKey): Promise<void> | undefined;
}

let boards: TrackerBoardChannel | null = null;
const listsChanged = new Set<(backend: BackendKey) => void>();

/** The board store takes the channel when it loads. */
export function takeTrackerBoardChannel(channel: TrackerBoardChannel): void {
  boards = channel;
}

/** A `tracker:board_updated` frame from `backend`. */
export function trackerBoardUpdated(event: TrackerBoardEvent, backend: BackendKey): void {
  boards?.event(event, backend);
}

/** A gap on `tracker:board_updated` from `backend`; see `TrackerBoardChannel.gap`. */
export function trackerBoardsGap(backend: BackendKey): Promise<void> | undefined {
  return boards?.gap(backend);
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
