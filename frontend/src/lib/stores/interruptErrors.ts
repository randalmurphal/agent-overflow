// Shared error-filter helpers for the Stop / interrupt path. Used by
// both the `thread.interrupt` builtin command (Esc key) and the
// `runInterruptOrRevert` flow (Composer Stop button). The two paths
// share the same fire-and-forget contract, so they share the same
// benign-error policy: race-window failures are dropped, real provider
// failures surface as a banner.
//
// Kept in its own module so `revertOnInterrupt.svelte.ts` can import
// without taking a dependency on `builtinCommands.svelte.ts` (which
// imports `revertOnInterrupt` itself).

import type { ErrorSurface } from './threadPaneRoles';
import { errString } from '../utils/errors';
import { userFacingError } from '../utils/userFacingError';
import { isAlreadyHandled } from '../transport/alreadyHandled';

/**
 * Errors we treat as no-ops on the interrupt path. Two cases:
 *
 *  1. "no active turn" — fired during the optimistic dispatch window
 *     (sendInFlight=true, but backend hasn't seen `turn/started` yet)
 *     when the user presses Esc.
 *  2. The request is no longer open — a race between our explicit cancel
 *     and the CLI's own `control_cancel_request`, or another client that
 *     answered the same prompt first. The backend answers both with the
 *     `already_handled` code.
 *
 * Case 2 is checked by CODE first and by text second, and the order is
 * load-bearing rather than stylistic. The transport redacts method-error
 * text for every caller that is not on loopback, so the text match has
 * never worked over the network: a remote client hitting this race saw a
 * banner where the desktop saw nothing. The code survives redaction. The
 * text arm stays for the loopback paths that still reach here with a raw
 * message and for errors raised client-side, which carry no code at all.
 *
 * Anything else surfaces as a banner so a real failure doesn't get
 * swallowed.
 */
export function isBenignInterruptError(err: unknown): boolean {
  if (isAlreadyHandled(err)) return true;
  const message = errString(err).toLowerCase();
  return (
    message.includes('no active turn') ||
    message.includes('already resolved') ||
    message.includes('stale interactive request')
  );
}

/**
 * Standard `.catch` for the fire-and-forget interrupt RPCs. Benign
 * races (no-active-turn, already-resolved) are dropped silently;
 * everything else surfaces on the pane banner so a real provider
 * crash doesn't get swallowed by the optimistic UI path.
 */
export function reportNonBenignInterruptError(
  pane: Pick<ErrorSurface, 'setGeneralError'>,
  err: unknown,
): void {
  if (isBenignInterruptError(err)) return;
  console.error('Failed to interrupt turn:', err);
  pane.setGeneralError(userFacingError(err));
}
