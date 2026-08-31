import { describe, expect, it } from 'vitest';
import { TransportError } from './wsClient';
import {
  AUTH_REASON_CODES,
  UNKNOWN_AUTH_REASON,
  authFailurePresentation,
  isAuthFailure,
  isAuthReasonCode,
  presentAuthReason,
} from './authReason';

const refusal = (reason?: string) => new TransportError('auth_failed', 'not authorized', reason);

describe('presentAuthReason', () => {
  it('explains every code this build declares', () => {
    for (const code of AUTH_REASON_CODES) {
      const shown = presentAuthReason(code);
      expect(shown.title, code).not.toBe('');
      expect(shown.hint, code).not.toBe('');
      expect(shown, code).not.toBe(UNKNOWN_AUTH_REASON);
    }
  });

  // The bundle and the backend update independently. A reason from a newer
  // backend must still produce a usable refusal, because a client that shows
  // nothing when it cannot explain something leaves a person staring at a
  // surface that silently does not work.
  it('falls back rather than showing nothing', () => {
    expect(presentAuthReason('reason_from_a_later_phase')).toBe(UNKNOWN_AUTH_REASON);
    expect(presentAuthReason(undefined)).toBe(UNKNOWN_AUTH_REASON);
    expect(presentAuthReason(null)).toBe(UNKNOWN_AUTH_REASON);
    expect(presentAuthReason('')).toBe(UNKNOWN_AUTH_REASON);
  });

  // The clock hint is the one this module exists for: a wrong clock is the
  // dominant real cause of a time-window refusal, and it is the only refusal
  // whose remedy is a device setting rather than signing in again.
  it('sends a time-window refusal to the clock, not to a re-pair', () => {
    const shown = presentAuthReason('outside_time_window');
    expect(shown.hint).toContain('automatic date & time');
    expect(shown.retryable).toBe(true);
  });

  // Every other refusal is resolved by presenting a DIFFERENT credential, so
  // offering a retry would offer a button that cannot work.
  it('marks the credential refusals as not retryable', () => {
    for (const code of AUTH_REASON_CODES) {
      if (code === 'outside_time_window') continue;
      expect(presentAuthReason(code).retryable, code).toBe(false);
    }
    expect(UNKNOWN_AUTH_REASON.retryable).toBe(false);
  });

  // A refusal that reads like a fault sends someone to debug a session that
  // was ended on purpose.
  it('says a revoked session was ended deliberately', () => {
    expect(presentAuthReason('revoked_session').title.toLowerCase()).toContain('signed out');
  });
});

describe('isAuthReasonCode', () => {
  it('answers for the declared set only', () => {
    for (const code of AUTH_REASON_CODES) expect(isAuthReasonCode(code), code).toBe(true);
    expect(isAuthReasonCode('method_error')).toBe(false);
    expect(isAuthReasonCode(undefined)).toBe(false);
    expect(isAuthReasonCode(7)).toBe(false);
  });

  // `code in PRESENTATIONS` would answer true for anything Object.prototype
  // carries if the map were a plain object literal reached without care.
  it('is not fooled by inherited property names', () => {
    expect(isAuthReasonCode('toString')).toBe(false);
    expect(isAuthReasonCode('constructor')).toBe(false);
    expect(isAuthReasonCode('__proto__')).toBe(false);
  });
});

describe('isAuthFailure', () => {
  it('branches on the code, not the reason', () => {
    expect(isAuthFailure(refusal('expired_session'))).toBe(true);
    // A refusal whose reason this build cannot read is still a refusal.
    expect(isAuthFailure(refusal('reason_from_a_later_phase'))).toBe(true);
    expect(isAuthFailure(refusal(undefined))).toBe(true);
  });

  it('rejects every other failure', () => {
    expect(isAuthFailure(new TransportError('method_error', 'not authorized'))).toBe(false);
    expect(isAuthFailure(new Error('auth_failed'))).toBe(false);
    expect(isAuthFailure('auth_failed')).toBe(false);
    expect(isAuthFailure(undefined)).toBe(false);
  });
});

describe('authFailurePresentation', () => {
  it('translates a refusal', () => {
    const shown = authFailurePresentation(refusal('outside_time_window'));
    expect(shown?.hint).toContain('automatic date & time');
  });

  // The null answer is what keeps an ordinary method failure from being
  // reported as a sign-in problem.
  it('answers null for anything that is not a refusal', () => {
    expect(authFailurePresentation(new TransportError('method_error', 'boom'))).toBeNull();
    expect(authFailurePresentation(new Error('boom'))).toBeNull();
    expect(authFailurePresentation(null)).toBeNull();
  });

  it('falls back for a refusal it cannot explain', () => {
    expect(authFailurePresentation(refusal('reason_from_a_later_phase'))).toBe(UNKNOWN_AUTH_REASON);
    expect(authFailurePresentation(refusal(undefined))).toBe(UNKNOWN_AUTH_REASON);
  });
});
