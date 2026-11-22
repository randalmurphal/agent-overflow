import type { TransportStatus } from '../../transport/wsClient';

/** What a computer's transport strip is showing, when it shows. */
export type TransportBannerReason = 'outage' | 'checking' | 'syncing' | 'notice';

export interface TransportBannerInput {
  /**
   * A paired computer that is still attached. Removal is a fact to act on,
   * not a drop, so a removed computer is judged as this page's own is.
   */
  paired: boolean;
  status: TransportStatus;
  /** The connection is open and a resume check is asking it to answer. */
  checking: boolean;
  /** A reconnect's recovery is replaying what this page missed. */
  resyncing: boolean;
  /** `backendUnreachableSettled`: in an outage for the settle time without a break. */
  unreachableSettled: boolean;
  /** This page has been connected to the computer at least once. */
  hasEverConnected: boolean;
  /** The first connect has had its second of grace. */
  bootGraceExpired: boolean;
  /** A healthy-connection notice (a failed boot phase, a bundle notice). */
  notice: boolean;
}

/**
 * Whether a computer's strip shows now, and why: the one rule for it.
 *
 * A paired computer's strip shows a connection problem only once it has
 * been in an outage for the settle time. Every reason it could show
 * earlier (a drop, a refusal, a resume check, a resync) is held to that,
 * so a connection that comes and goes never shows, hides or resizes
 * anything; the composer of an open thread there says at once that a
 * message cannot be sent. While connected, its notice still shows.
 *
 * This page's own computer shows a problem at once, except a start or a
 * first connect: a start only for a computer this page already used (a
 * restart), and a first connect only after its second of grace.
 */
export function transportBannerReason(input: TransportBannerInput): TransportBannerReason | null {
  const connected = input.status === 'connected';
  if (input.paired) {
    if (!connected) return input.unreachableSettled ? 'outage' : null;
    return input.notice ? 'notice' : null;
  }
  if (connected) {
    if (input.checking) return 'checking';
    if (input.resyncing) return 'syncing';
    return input.notice ? 'notice' : null;
  }
  const shown = input.status === 'starting'
    ? input.hasEverConnected
    : input.hasEverConnected || input.bootGraceExpired;
  return shown || input.notice ? 'outage' : null;
}
