import { describe, expect, it } from 'vitest';
import { TransportError, DisconnectedError } from './wsClient';
import {
  SESSION_REFUSAL,
  STEP_UP_REFUSAL,
  UNKNOWN_SCOPE_REFUSAL,
  isScopeRefusal,
  isStepUpRefusal,
  presentScope,
  scopeRefusalMessage,
  scopeRefusalPresentation,
} from './scopeRefusal';
import { SCOPES } from './scopes';

function refusal(scope?: string): TransportError {
  return new TransportError('scope_required', 'not authorized', undefined, scope);
}

describe('scopeRefusal', () => {
  it('names the capability the backend named', () => {
    const presentation = scopeRefusalPresentation(refusal('threads:autonomy'));
    expect(presentation?.title).toContain('workflows');
    expect(presentation?.hint).toContain('Devices');
  });

  it('has a sentence for every capability this build knows', () => {
    // A capability with no sentence would fall through to the generic
    // refusal, which is the degradation reserved for names this build has
    // never heard of — not for ones it gates on.
    for (const scope of SCOPES) {
      const presentation = presentScope(scope);
      expect(presentation.title).not.toBe('');
      expect(presentation.hint).not.toBe('');
    }
  });

  it('degrades an unknown capability name to the generic sentence', () => {
    // A tab stays loaded across a backend update, so a newer backend can
    // refuse with a name this bundle has no word for. Saying so vaguely
    // beats a surface that silently does not work.
    expect(scopeRefusalPresentation(refusal('quantum:entangle'))).toEqual(UNKNOWN_SCOPE_REFUSAL);
    expect(scopeRefusalPresentation(refusal(undefined))).toEqual(UNKNOWN_SCOPE_REFUSAL);
    expect(scopeRefusalPresentation(refusal(''))).toEqual(UNKNOWN_SCOPE_REFUSAL);
  });

  it('does not read a prototype property as a capability name', () => {
    // The lookup is a plain object, so `constructor` and `toString` would
    // type as capability names under an `in` check and hand back a
    // function where a sentence belongs.
    expect(scopeRefusalPresentation(refusal('constructor'))).toEqual(UNKNOWN_SCOPE_REFUSAL);
    expect(scopeRefusalPresentation(refusal('toString'))).toEqual(UNKNOWN_SCOPE_REFUSAL);
  });

  it('answers `host` with the presence remedy, not a grant one', () => {
    // Nobody can be granted `host`, so offering to widen this device's
    // access would be a false instruction.
    expect(presentScope('host')).toEqual(STEP_UP_REFUSAL);
  });

  it('answers the `session` floor with the pairing remedy, not a grant one', () => {
    // Unreachable for a session caller — the floor admits on session
    // presence alone — so the sentence is for the only way it could
    // arrive: a backend that did not accept this device's session.
    expect(presentScope('session')).toEqual(SESSION_REFUSAL);
    expect(SESSION_REFUSAL.hint).not.toBe(STEP_UP_REFUSAL.hint);
  });

  it('treats a step-up refusal as its own kind', () => {
    const err = new TransportError('step_up_required', 'not authorized');
    expect(isStepUpRefusal(err)).toBe(true);
    expect(isScopeRefusal(err)).toBe(false);
    expect(scopeRefusalPresentation(err)).toEqual(STEP_UP_REFUSAL);
  });

  it('answers null for anything that is not an authorization refusal', () => {
    // The null is what keeps an ordinary method failure from being
    // reported to somebody as a permissions problem.
    expect(scopeRefusalPresentation(new TransportError('method_error', 'boom'))).toBeNull();
    expect(scopeRefusalPresentation(new TransportError('auth_failed', 'nope', 'expired_session'))).toBeNull();
    expect(scopeRefusalPresentation(new DisconnectedError('socket closed'))).toBeNull();
    expect(scopeRefusalPresentation(new Error('plain'))).toBeNull();
    expect(scopeRefusalPresentation(undefined)).toBeNull();
    expect(scopeRefusalMessage(new Error('plain'))).toBeNull();
  });

  it('folds a refusal into one line for a toast', () => {
    const line = scopeRefusalMessage(refusal('git:operate'));
    expect(line).toContain('git actions');
    expect(line).toContain('Devices');
  });
});
