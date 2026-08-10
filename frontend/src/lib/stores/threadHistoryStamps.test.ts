import { beforeEach, describe, expect, it } from 'vitest';
import {
  __resetThreadHistoryStampsForTest,
  adoptEventStamp,
  dropAllThreadHistoryStamps,
  dropThreadHistoryStamp,
  getThreadHistoryStamp,
  recordAttestedStamp,
} from './threadHistoryStamps';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';

describe('thread history stamps', () => {
  beforeEach(() => {
    __resetThreadHistoryStampsForTest();
  });

  it('marks a sync-response stamp attested and an event stamp not', () => {
    recordAttestedStamp('t-1', 2, 9);
    expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 2, rev: 9, attested: true });

    adoptEventStamp('t-2', 1, 3);
    expect(getThreadHistoryStamp('t-2')).toEqual({ epoch: 1, rev: 3, attested: false });
  });

  it('ignores the zero stamp the wire sends for "no stamp"', () => {
    adoptEventStamp('t-1', 0, 0);
    expect(getThreadHistoryStamp('t-1')).toBeNull();
  });

  it('ignores non-numeric or non-finite wire values', () => {
    adoptEventStamp('t-1', undefined, undefined);
    adoptEventStamp('t-1', '3', '4');
    adoptEventStamp('t-1', Number.NaN, 2);
    expect(getThreadHistoryStamp('t-1')).toBeNull();
  });

  it('never moves a stamp backwards when an event races a sync response', () => {
    recordAttestedStamp('t-1', 2, 9);
    adoptEventStamp('t-1', 2, 5);
    expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 2, rev: 9, attested: true });

    adoptEventStamp('t-1', 2, 11);
    expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 2, rev: 11, attested: false });
  });

  // Provenance transitions: the flag is what an L1 snapshot copies and
  // what the fresh-echo guard reads, so the downgrade direction matters
  // as much as the values. A state check passes on a registry that gets
  // the transitions wrong.
  describe('provenance', () => {
    it('downgrades an attested stamp when an equal-or-newer event stamp arrives', () => {
      recordAttestedStamp('t-1', 2, 9);
      adoptEventStamp('t-1', 2, 30);
      // The backend provably moved past the attested rev; only a sync
      // response may claim attestation, so the grade drops with it.
      expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 2, rev: 30, attested: false });
    });

    it('restores attestation only through a sync response', () => {
      adoptEventStamp('t-1', 2, 30);
      expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 2, rev: 30, attested: false });

      recordAttestedStamp('t-1', 3, 31);
      expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 3, rev: 31, attested: true });
    });

    it('ignores a non-finite attestation rather than storing a broken stamp', () => {
      recordAttestedStamp('t-1', 1, 1);
      recordAttestedStamp('t-1', Number.NaN, 5);
      expect(getThreadHistoryStamp('t-1')).toEqual({ epoch: 1, rev: 1, attested: true });
    });
  });

  it('drops one thread, or all of them', () => {
    recordAttestedStamp('t-1', 1, 1);
    recordAttestedStamp('t-2', 1, 1);

    dropThreadHistoryStamp('t-1');
    expect(getThreadHistoryStamp('t-1')).toBeNull();
    expect(getThreadHistoryStamp('t-2')).not.toBeNull();

    dropAllThreadHistoryStamps();
    expect(getThreadHistoryStamp('t-2')).toBeNull();
  });

  it('drops every stamp when the backend identity changes', () => {
    recordAttestedStamp('t-1', 1, 1);
    // A re-minted generation means the counters no longer continue the
    // sequence these stamps were read from.
    setBackendIdentityFromBootstrap('backend-a', 'gen-2');
    expect(getThreadHistoryStamp('t-1')).toBeNull();
  });
});
