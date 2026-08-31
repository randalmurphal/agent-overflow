import { describe, expect, it } from 'vitest';
import { TransportError } from './wsClient';
import { ignoringAlreadyHandled, isAlreadyHandled } from './alreadyHandled';

const alreadyHandled = () => new TransportError('already_handled', 'approval req-1: already handled');

describe('isAlreadyHandled', () => {
  it('recognizes the code', () => {
    expect(isAlreadyHandled(alreadyHandled())).toBe(true);
  });

  // The whole point of a code is that it survives the transport's redaction of
  // method-error TEXT for anything that is not the loopback caller. A remote
  // client sees "method failed (id: ...)" and nothing else, so a check that
  // reads the message would answer false there and true on the desktop.
  it('does not depend on the message', () => {
    expect(isAlreadyHandled(new TransportError('already_handled', 'method failed (id: abc123)')))
      .toBe(true);
  });

  it('rejects every other failure', () => {
    expect(isAlreadyHandled(new TransportError('method_error', 'already handled'))).toBe(false);
    expect(isAlreadyHandled(new Error('already handled'))).toBe(false);
    expect(isAlreadyHandled(undefined)).toBe(false);
    expect(isAlreadyHandled('already_handled')).toBe(false);
  });
});

describe('ignoringAlreadyHandled', () => {
  it('resolves when another client got there first', async () => {
    await expect(ignoringAlreadyHandled(Promise.reject(alreadyHandled()))).resolves.toBeUndefined();
  });

  it('rethrows a real failure unchanged', async () => {
    const failure = new TransportError('method_error', 'method failed (id: abc123)');
    await expect(ignoringAlreadyHandled(Promise.reject(failure))).rejects.toBe(failure);
  });

  it('passes a success through', async () => {
    await expect(ignoringAlreadyHandled(Promise.resolve('ok'))).resolves.toBeUndefined();
  });
});
