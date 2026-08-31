import { TransportError } from './wsClient';

/**
 * The client-side half of the credential refusal vocabulary.
 *
 * The backend answers a refused call with code `auth_failed` and a `reason`
 * from a closed set (internal/identity/reason.go). This module is the ONE
 * place that set is translated into something a person can act on. Reading a
 * reason anywhere else — a component branching on `'expired_session'`, a
 * store composing its own sentence — is what puts two different explanations
 * of the same refusal on two different screens.
 *
 * Codes are stable forever once shipped, and this bundle may be older or
 * newer than the backend it is talking to. Both directions degrade instead of
 * failing: a reason this build does not know falls back to the generic
 * refusal, and a reason this build knows that the backend never sends is
 * simply never shown.
 *
 * Pinned against the Go set by `TestFrontendHintsCoverEveryRefusal` in
 * internal/identity, which reads this file. Add a code here in the same
 * change that adds it there.
 */

/** The refusal codes this build can explain. */
export const AUTH_REASON_CODES = [
  'missing_proof',
  'malformed_proof',
  'key_mismatch',
  'invalid_signature',
  'outside_time_window',
  'unknown_session',
  'revoked_session',
  'expired_session',
  'pending_confirmation',
  'unknown_credential',
] as const;

export type AuthReasonCode = (typeof AUTH_REASON_CODES)[number];

/**
 * What to show for a refusal: one sentence saying what happened, and one
 * saying what to do about it. `retryable` is whether presenting the same
 * credential again could ever succeed — a UI that offers "try again" on a
 * revoked session is offering a button that cannot work.
 */
export interface AuthReasonPresentation {
  /** What happened, in the second person. */
  title: string;
  /** The action that resolves it, or the reason there is none. */
  hint: string;
  /** Whether re-presenting the SAME credential could succeed. */
  retryable: boolean;
}

const PRESENTATIONS: Record<AuthReasonCode, AuthReasonPresentation> = {
  // Nothing was presented. Almost always a client that has not paired yet,
  // or one whose stored credential was cleared with the browser's data.
  missing_proof: {
    title: 'This device is not signed in.',
    hint: 'Pair it again from the app on your computer.',
    retryable: false,
  },
  // Something was presented and could not be read. A truncated copy-paste, a
  // half-written storage entry, or a value from a much older format.
  malformed_proof: {
    title: "This device's sign-in could not be read.",
    hint: 'Sign out and pair this device again.',
    retryable: false,
  },
  // Well-formed, signed under a key this backend does not hold. The usual
  // cause is a restored or re-created backend, which is a re-pair, not a
  // fault on this device.
  key_mismatch: {
    title: 'This device is signed in to a different backend.',
    hint: 'Pair it again with this one.',
    retryable: false,
  },
  // The signature did not verify under a key we do hold. Retrying the same
  // credential cannot change that.
  invalid_signature: {
    title: "This device's sign-in could not be verified.",
    hint: 'Sign out and pair this device again.',
    retryable: false,
  },
  // The dominant real cause of this one is a wrong clock, not a wrong
  // credential — which is why the hint names the clock and why the backend
  // checks the signature BEFORE the time window, so a proof that did not
  // verify can never arrive here.
  outside_time_window: {
    title: "This device's clock is too far from the backend's.",
    hint: 'Check that automatic date & time is on for both devices, then try again.',
    retryable: true,
  },
  // Signature fine, no such session. A backend that lost its history, or a
  // session removed outside the app.
  unknown_session: {
    title: 'This session no longer exists.',
    hint: 'Sign in again to start a new one.',
    retryable: false,
  },
  // Somebody ended this session on purpose. Say so plainly: the honest
  // reading is "this was intended", not "something broke".
  revoked_session: {
    title: 'This session was signed out from another device.',
    hint: 'Sign in again if you still want access from here.',
    retryable: false,
  },
  // Ordinary and expected. A renewal usually happens without anybody
  // reading this, so the wording stays low-alarm.
  expired_session: {
    title: 'This session has expired.',
    hint: 'Sign in again to continue.',
    retryable: false,
  },
  // The only refusal whose remedy is on a different device, and the only one
  // where presenting the same credential again is expected to work shortly.
  // `retryable` says so, so a client can keep waiting instead of offering to
  // start the pairing over.
  pending_confirmation: {
    title: 'This device is waiting to be confirmed.',
    hint: 'Check that the number shown here matches the one on your computer, then confirm it there.',
    retryable: true,
  },
  // A pairing token or a renewal secret that names nothing we still honour.
  // Spent, expired, and never-existed all arrive here together, so the hint
  // names the one action that covers all three.
  unknown_credential: {
    title: 'This pairing could not be completed.',
    hint: 'Start a new pairing from the app on your computer.',
    retryable: false,
  },
};

/**
 * The refusal shown when the backend gave a reason this build does not know,
 * or gave none at all. Says what is true — access was refused — without
 * inventing a cause, and points at the surface that can actually resolve it.
 */
export const UNKNOWN_AUTH_REASON: AuthReasonPresentation = {
  title: 'This device is not allowed to connect.',
  hint: 'Open the app on your computer to check this device, then pair it again.',
  retryable: false,
};

/**
 * Is this a refusal code this build can explain?
 *
 * `Object.hasOwn`, not `in`: `in` walks the prototype chain, so `'toString'`
 * and `'constructor'` would type as valid codes and `presentAuthReason` would
 * hand back a function where a presentation belongs.
 */
export function isAuthReasonCode(code: unknown): code is AuthReasonCode {
  return typeof code === 'string' && Object.hasOwn(PRESENTATIONS, code);
}

/**
 * Translate a refusal code. Always answers: an unknown or missing code gets
 * the generic refusal, because a client that shows nothing when it cannot
 * explain something leaves a person staring at a surface that silently does
 * not work.
 */
export function presentAuthReason(code: string | undefined | null): AuthReasonPresentation {
  return isAuthReasonCode(code) ? PRESENTATIONS[code] : UNKNOWN_AUTH_REASON;
}

/**
 * Was this call refused by the credential check, rather than failing?
 *
 * Branches on the code alone. A refusal whose reason this build cannot read
 * is still a refusal, and treating it as an ordinary error would offer a
 * retry that can never succeed.
 */
export function isAuthFailure(err: unknown): err is TransportError {
  return err instanceof TransportError && err.code === 'auth_failed';
}

/**
 * The presentation for a caught transport error, or null if it was not a
 * credential refusal. The null answer is what keeps callers from reporting
 * an ordinary method failure as a sign-in problem.
 */
export function authFailurePresentation(err: unknown): AuthReasonPresentation | null {
  return isAuthFailure(err) ? presentAuthReason(err.reason) : null;
}
