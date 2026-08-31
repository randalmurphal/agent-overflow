import { describe, expect, it } from 'vitest';
import { scopeRefusalMessage } from '../transport/scopeRefusal';
import { TransportError } from '../transport/wsClient';
import { userFacingError } from './userFacingError';

describe('userFacingError', () => {
  it('returns fallback for null/undefined', () => {
    expect(userFacingError(null)).toBe('Something went wrong.');
    expect(userFacingError(undefined)).toBe('Something went wrong.');
  });

  it('returns fallback for empty string', () => {
    expect(userFacingError('')).toBe('Something went wrong.');
  });

  it('honours custom fallback', () => {
    expect(userFacingError(null, 'Could not load.')).toBe('Could not load.');
  });

  it('strips Go wrap chains, keeping the inner message', () => {
    expect(userFacingError(new Error('send message: get thread: thread not found'))).toBe(
      'Thread not found.',
    );
  });

  it('keeps short trailing segments by falling back to the full message', () => {
    // Trailing segment is <= 6 chars; we keep the original to preserve context.
    expect(userFacingError(new Error('database: i/o'))).toBe('Database: i/o.');
  });

  it('drops UUIDs from the message', () => {
    const uuid = '550e8400-e29b-41d4-a716-446655440000';
    expect(userFacingError(new Error(`could not load for thread ${uuid}`))).toBe(
      'Could not load for thread.',
    );
  });

  it('capitalises the first letter and adds a period', () => {
    expect(userFacingError('something failed')).toBe('Something failed.');
  });

  it('preserves an existing trailing period', () => {
    expect(userFacingError('Already done.')).toBe('Already done.');
  });

  it('preserves an existing trailing question or exclamation mark', () => {
    expect(userFacingError('Why?')).toBe('Why?');
    expect(userFacingError('Boom!')).toBe('Boom!');
  });

  it('handles strings without an Error wrapper', () => {
    expect(userFacingError('plain string')).toBe('Plain string.');
  });

  it('handles non-Error objects with a message field', () => {
    expect(userFacingError({ message: 'thrown plain object' } as unknown as Error)).toBe(
      'Thrown plain object.',
    );
  });

  it('returns the fallback when the error stringifies to whitespace', () => {
    expect(userFacingError(new Error('   '))).toBe('Something went wrong.');
  });

  it('phrases an authorization refusal through the one presentation module', () => {
    const refused = new TransportError(
      'scope_required',
      'RenameThread requires the threads:operate scope, which this session was not granted',
      undefined,
      'threads:operate',
    );
    const message = userFacingError(refused);
    // The exact sentence belongs to scopeRefusal.ts; what this pins is
    // that the wrap-trimmer below never touches a refusal (which would
    // surface only the tail segment of the server sentence).
    expect(message).toBe(scopeRefusalMessage(refused));
    expect(message).toContain('granted');
    expect(message).not.toBe('Granted.');
  });

  it('leaves an ordinary transport error to the generic path', () => {
    expect(
      userFacingError(new TransportError('internal_error', 'method failed: database is closed')),
    ).toBe('Database is closed.');
  });
});
