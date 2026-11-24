import { TransportError } from '../../transport/wsClient';

// The rule both pairing dialogs (PairDeviceModal, ComputerPairingWindow)
// apply to their asynchronous completions. A dialog begins a turn when it
// opens and again for each invitation or window it starts, and ends its turns
// on dismissal. Every completion (a mint, an open, a poll, an approval, a
// cancellation, and their error and finally branches) captures the turn it
// started in and changes the dialog only while that turn is the latest: a
// reply for an earlier opening or an earlier invitation never paints over,
// closes, or restarts the current one.

/** One turn of a pairing dialog. */
export interface PairingTurn {
  /** Whether no later turn has begun and the dialog has not ended. */
  readonly current: boolean;
}

export interface PairingTurns {
  /** Starts a turn that supersedes every earlier one. */
  begin(): PairingTurn;
  /** Supersedes every turn, for a dismissal or teardown. */
  end(): void;
}

export function pairingTurns(): PairingTurns {
  let latest = 0;
  return {
    begin() {
      const id = ++latest;
      return { get current() { return id === latest; } };
    },
    end() {
      ++latest;
    },
  };
}

/**
 * Whether an approval was refused because an earlier approval already
 * allowed the device (ConfirmDevicePairing's `pairing_already_allowed`): the
 * device has access, so the surface shows it as allowed, not as a failure.
 */
export function pairingAlreadyAllowed(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'pairing_already_allowed';
}

// The approval each computer's pairing window has in flight. Opening a new
// window retires the previous one on the host, so an open waits for the
// previous window's approval to settle, including one sent from a dialog that
// has since been dismissed. Keyed by computer; an entry lives only while its
// approval is in flight.
const decisions = new Map<string, Promise<void>>();

/** Records approval as the computer's outstanding decision until it settles. */
export function recordPairingDecision(computer: string, approval: Promise<void>): void {
  const settled = approval.then(() => {}, () => {});
  decisions.set(computer, settled);
  void settled.then(() => {
    if (decisions.get(computer) === settled) decisions.delete(computer);
  });
}

/** Settles once the computer's outstanding approval, if any, has. */
export function pairingDecisionSettled(computer: string): Promise<void> {
  return decisions.get(computer) ?? Promise.resolve();
}
