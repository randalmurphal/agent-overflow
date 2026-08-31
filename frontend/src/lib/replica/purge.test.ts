// The sweep policy on its own: which of an origin's databases are ours,
// and which of ours no live backend claims. No storage engine involved —
// the decision is the part that must not drift.
import { describe, expect, it } from 'vitest';
import {
  REPLICA_DB_PREFIX,
  replicaDatabaseBackendId,
  replicaDatabaseName,
  unclaimedReplicaDatabases,
} from './purge';

describe('replica database names', () => {
  it('round-trips a backend id through the database name', () => {
    const name = replicaDatabaseName('7f1c3a2e');
    expect(name).toBe(`${REPLICA_DB_PREFIX}7f1c3a2e`);
    expect(replicaDatabaseBackendId(name)).toBe('7f1c3a2e');
  });

  it('claims no database it did not mint', () => {
    expect(replicaDatabaseBackendId('keyval-store')).toBeNull();
    expect(replicaDatabaseBackendId('ao-attachments-7f1c')).toBeNull();
    // The bare prefix cannot come from initReplica, which refuses an
    // empty backend id — so it is somebody else's database.
    expect(replicaDatabaseBackendId(REPLICA_DB_PREFIX)).toBeNull();
  });
});

describe('unclaimedReplicaDatabases', () => {
  const origin = [
    replicaDatabaseName('live'),
    replicaDatabaseName('moved-on'),
    'some-other-app',
    REPLICA_DB_PREFIX,
  ];

  it('keeps every live backend and reaps the rest', () => {
    expect(unclaimedReplicaDatabases(origin, new Set(['live']))).toEqual([
      replicaDatabaseName('moved-on'),
    ]);
  });

  it('never touches a database this app did not mint', () => {
    const targets = unclaimedReplicaDatabases(origin, new Set());
    expect(targets).not.toContain('some-other-app');
    expect(targets).not.toContain(REPLICA_DB_PREFIX);
  });

  it('takes several live backends, which is the whole point of a set', () => {
    expect(
      unclaimedReplicaDatabases(origin, new Set(['live', 'moved-on'])),
    ).toEqual([]);
  });

  it('reads an empty live set as "keep none" — the sign-out instruction', () => {
    expect(unclaimedReplicaDatabases(origin, new Set())).toEqual([
      replicaDatabaseName('live'),
      replicaDatabaseName('moved-on'),
    ]);
  });
});
