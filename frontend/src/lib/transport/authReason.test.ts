import { describe, expect, it } from 'vitest';
import { TransportError } from './wsClient';
import {
  AUTH_REASON_CODES,
  UNKNOWN_AUTH_REASON,
  isAuthFailure,
  isAuthReasonCode,
  presentAuthReason,
} from './authReason';

const refusal = (reason?: string) => new TransportError('auth_failed', 'not authorized', { reason });

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

  // Every refusal outside this set is resolved by presenting a DIFFERENT
  // credential, so offering a retry would offer a button that cannot work.
  const RETRYABLE_REFUSALS = new Set([
    'outside_time_window',
    // The device holds the real credential already and is waiting for the
    // owner to match the verification number on the minting surface.
    'pending_confirmation',
    // A proof is spent on first use and minted fresh per request, so a
    // replayed one is resolved by the NEXT attempt by itself.
    'proof_replayed',
    'temporarily_unavailable',
  ]);

  it('offers a retry only for the refusals the next attempt can resolve', () => {
    for (const code of AUTH_REASON_CODES) {
      expect(presentAuthReason(code).retryable, code).toBe(RETRYABLE_REFUSALS.has(code));
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
