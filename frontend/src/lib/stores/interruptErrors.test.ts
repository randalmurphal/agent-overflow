// The benign-race filter on the interrupt path. Its text arm has been there
// since the flow was single-client; the code arm is what makes it work for a
// client that is not on loopback.
import { describe, expect, it, vi } from 'vitest';
import { isBenignInterruptError, reportNonBenignInterruptError } from './interruptErrors';
import { TransportError } from '../transport/wsClient';

describe('isBenignInterruptError', () => {
  // The transport redacts method-error text for every caller that is not on
  // loopback, so a remote client's error reads "method failed (id: ...)" and
  // the text arm cannot see anything in it. Before the code arm, that client
  // got an error banner for the same race the desktop dropped silently.
  it('recognizes a redacted already-handled from a remote client', () => {
    expect(isBenignInterruptError(new TransportError('already_handled', 'method failed (id: abc)')))
      .toBe(true);
  });

  it('still recognizes the loopback messages', () => {
    expect(isBenignInterruptError(new Error('no active turn for thread t1'))).toBe(true);
    expect(isBenignInterruptError(new Error('approval already resolved'))).toBe(true);
    expect(isBenignInterruptError(new Error('provider: stale interactive request'))).toBe(true);
  });

  it('lets a real failure through', () => {
    expect(isBenignInterruptError(new Error('provider session died'))).toBe(false);
    expect(isBenignInterruptError(new TransportError('method_error', 'method failed (id: abc)')))
      .toBe(false);
  });
});

describe('reportNonBenignInterruptError', () => {
  it('leaves the banner alone for a race another client won', () => {
    const setGeneralError = vi.fn();
    reportNonBenignInterruptError(
      { setGeneralError },
      new TransportError('already_handled', 'method failed (id: abc)'),
    );
    expect(setGeneralError).not.toHaveBeenCalled();
  });

  it('surfaces a real failure', () => {
    const setGeneralError = vi.fn();
    reportNonBenignInterruptError({ setGeneralError }, new Error('provider session died'));
    expect(setGeneralError).toHaveBeenCalled();
  });
});
