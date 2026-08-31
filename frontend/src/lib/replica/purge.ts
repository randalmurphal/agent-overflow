// Which replica databases this origin is allowed to keep.
//
// The replica stores one database per backend (`ao-replica-<backendId>`),
// so a backend id that moves — a rebuilt host, a different machine behind
// the same `--connect` endpoint, a phone re-paired after a restore —
// leaves the previous database behind with nothing left that can read it.
// It sits outside every per-database cap: eviction is bookkeeping INSIDE a
// database (eviction.ts), so a database nobody opens is never swept. A
// sweep across databases is therefore the only bound on what one origin
// accumulates over a device's lifetime.
//
// The policy is pure and lives apart from the IndexedDB machinery so the
// decision "does any live backend claim this database?" is testable
// without a storage engine, and so it takes a SET of live backend ids from
// the start. Multi-backend attach (docs/specs/remote-access.md §10) adds
// ids to that set and changes nothing here.

/** Prefix every replica database name carries. */
export const REPLICA_DB_PREFIX = 'ao-replica-';

/** The database one backend's replica lives in. */
export function replicaDatabaseName(backendId: string): string {
  return `${REPLICA_DB_PREFIX}${backendId}`;
}

/**
 * The backend id a database name carries, or null when the name is not a
 * replica database this app mints. A bare `ao-replica-` with no id is
 * null on purpose: `initReplica` refuses an empty backend id, so such a
 * name came from something else on this origin and is not ours to delete.
 */
export function replicaDatabaseBackendId(name: string): string | null {
  if (!name.startsWith(REPLICA_DB_PREFIX)) return null;
  const backendId = name.slice(REPLICA_DB_PREFIX.length);
  return backendId === '' ? null : backendId;
}

/**
 * The replica databases in `names` that no live backend claims.
 *
 * `liveBackendIds` is the set of backends still attached — one today,
 * several once a client attaches to more than one backend, and EMPTY when
 * the caller means "keep none of them" (sign-out, device revocation). An
 * empty set is a deliberate instruction, which is why the callers that
 * cannot name their backend refuse to sweep at all rather than passing
 * one: an unknown identity is not a wildcard (transport/backendIdentity.ts).
 */
export function unclaimedReplicaDatabases(
  names: readonly string[],
  liveBackendIds: ReadonlySet<string>,
): string[] {
  const unclaimed: string[] = [];
  for (const name of names) {
    const backendId = replicaDatabaseBackendId(name);
    if (backendId === null) continue;
    if (liveBackendIds.has(backendId)) continue;
    unclaimed.push(name);
  }
  return unclaimed;
}
