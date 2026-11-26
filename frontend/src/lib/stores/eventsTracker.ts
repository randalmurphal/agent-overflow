import type { EventOrigin } from '../transport/handle';
import { backendKeyForOrigin } from '../transport/backends';
import { trackerBoardUpdated, trackerIssueUpdated, trackerSetupUpdated, type TrackerBoardEvent, type TrackerIssueEvent } from './trackerBoardEvents';

// Routed by the board's scope on the computer that sent it: the channel
// reaches every client holding tracker:read, and the board store applies a
// frame only to a board this client holds.
export function applyTrackerBoardUpdated(event: TrackerBoardEvent | null | undefined, origin?: EventOrigin): void {
  if (!event) return;
  trackerBoardUpdated(event, backendKeyForOrigin(origin?.backendId ?? ''));
}

// Routed by the ticket's connection and issue on the computer that sent it,
// as a board's frame is.
export function applyTrackerIssueUpdated(event: TrackerIssueEvent | null | undefined, origin?: EventOrigin): void {
  if (!event) return;
  trackerIssueUpdated(event, backendKeyForOrigin(origin?.backendId ?? ''));
}

// Payload-less: some project's tracker setup on the computer that sent it
// changed, and the setups held of that computer's projects read again.
export function applyTrackerSetupUpdated(_event: unknown, origin?: EventOrigin): void {
  trackerSetupUpdated(backendKeyForOrigin(origin?.backendId ?? ''));
}
