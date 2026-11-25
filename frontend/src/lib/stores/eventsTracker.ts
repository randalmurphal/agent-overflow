import type { EventOrigin } from '../transport/handle';
import { backendKeyForOrigin } from '../transport/backends';
import { trackerBoardUpdated, type TrackerBoardEvent } from './trackerBoardEvents';

// Routed by the board's scope on the computer that sent it: the channel
// reaches every client holding tracker:read, and the board store applies a
// frame only to a board this client holds.
export function applyTrackerBoardUpdated(event: TrackerBoardEvent | null | undefined, origin?: EventOrigin): void {
  if (!event) return;
  trackerBoardUpdated(event, backendKeyForOrigin(origin?.backendId ?? ''));
}
