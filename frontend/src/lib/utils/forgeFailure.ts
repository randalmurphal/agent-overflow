// A forge failure's kind, as the PR pump reports it beside the failure's
// text (ErrorKind, Reserve and ResumeAt on pr:updated, pr:ci_updated,
// pr:ci_log and the SubscribePRUpdates result). The kind picks the
// surface; see docs/architecture/forge-transport.md#failure-presentation.

import { formatTimeOfDay } from './format';
import { forgeLabels } from './forgeLabels';

export type ForgeFailureKind = 'transient' | 'rate_limited' | 'setup' | 'forge';

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

const KINDS: ReadonlySet<string> = new Set<ForgeFailureKind>(['transient', 'rate_limited', 'setup', 'forge']);

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
