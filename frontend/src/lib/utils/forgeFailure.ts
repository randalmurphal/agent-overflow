// A forge failure's kind, as the PR pump reports it beside the failure's
// text (ErrorKind, Reserve and ResumeAt on pr:updated, pr:ci_updated,
// pr:ci_log and the SubscribePRUpdates result). The kind picks the
// surface; see docs/architecture/forge-transport.md#failure-presentation.

import type { BackendKey } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import { formatTimeOfDay } from './format';
import { forgeLabels } from './forgeLabels';

export type ForgeFailureKind = 'transient' | 'rate_limited' | 'setup' | 'forbidden' | 'forge';

export interface ForgeFailure {
  readonly kind: ForgeFailureKind;
  /** A rate limit the app keeps for the user's own actions, not one the
   * forge enforces. */
  readonly reserve: boolean;
  /** When polling resumes (RFC 3339); set for rate_limited only. */
  readonly resumeAt: string;
}

/** The wire's kind fields, as every PR failure carries them. */
export interface ForgeFailureWire {
  errorKind?: string;
  reserve?: boolean;
  resumeAt?: string;
}

const KINDS: ReadonlySet<string> = new Set<ForgeFailureKind>(['transient', 'rate_limited', 'setup', 'forbidden', 'forge']);

/** The failure a wire payload names; null when it names no known kind. */
export function forgeFailureFrom(wire: ForgeFailureWire): ForgeFailure | null {
  const kind = wire.errorKind ?? '';
  if (!KINDS.has(kind)) return null;
  return {
    kind: kind as ForgeFailureKind,
    reserve: kind === 'rate_limited' && wire.reserve === true,
    resumeAt: kind === 'rate_limited' ? (wire.resumeAt ?? '') : '',
  };
}

/**
 * What a rate-limited pane says, in local time: the forge's limit was
 * reached, or the app is keeping the rest of it for the user's own
 * actions.
 */
export function rateLimitMessage(forge: string | null | undefined, failure: ForgeFailure): string {
  const name = forgeLabels(forge).name;
  const at = formatTimeOfDay(Date.parse(failure.resumeAt));
  if (failure.reserve) {
    return `${name} rate limit nearly used up. Updates pause until ${at} so your own actions still go through.`;
  }
  return `${name} rate limit reached. Updates resume at ${at}.`;
}

/**
 * What a rate-limited read that runs only when asked says (a ticket's
 * Work): what was not read, and when Retry can read it, in local time.
 */
export function rateLimitRetryMessage(forge: string | null | undefined, what: string, failure: ForgeFailure): string {
  const name = forgeLabels(forge).name;
  const at = formatTimeOfDay(Date.parse(failure.resumeAt));
  if (failure.reserve) {
    return `${name} rate limit nearly used up, so ${what} was not searched: the rest is kept for your own actions. Retry after ${at}.`;
  }
  return `${name} rate limit reached, so ${what} was not searched. Retry after ${at}.`;
}

/** A failed forge read as its wire carries it: the caller-safe sentence,
 * its kind fields and the detail behind it (ProjectPRList,
 * TicketForgeProblem). */
export interface ForgeReadFailureWire extends ForgeFailureWire {
  error: string;
  errorDetails?: unknown;
}

/** The failure behind a failed forge read, for its Details and copy: an
 * error carrying the read's detail as a failed call's does; null when the
 * read carries none. */
export function forgeReadFailureCause(wire: ForgeReadFailureWire, backend: BackendKey): TransportError | null {
  return wire.errorDetails ? new TransportError(`forge_${wire.errorKind || 'forge'}`, wire.error, { detail: wire.errorDetails, backend }) : null;
}
