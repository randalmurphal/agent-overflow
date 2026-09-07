// Removing a connection is local intent, not a membership revocation. Keep it
// across launches so a sponsor cannot immediately add that computer back.
const KEY = 'agent-overflow:own-device-exclusions';
const LIMIT = 128;
const MEMBERSHIP_KEY = 'agent-overflow:own-device-membership';
type Membership = { backendId: string; keyThumbprint: string; generation: number; removed: boolean };

function validMembership(row: unknown): row is Membership {
  if (!row || typeof row !== 'object') return false;
  const member = row as Membership;
  return typeof member.backendId === 'string' && !!member.backendId && member.backendId.length <= 128
    && typeof member.keyThumbprint === 'string' && !!member.keyThumbprint && member.keyThumbprint.length <= 128
    && Number.isSafeInteger(member.generation) && member.generation >= 1 && member.generation <= 2 ** 52
    && typeof member.removed === 'boolean';
}

function validateActiveIdentities(known: Map<string, Membership>): void {
  const active = new Set<string>();
  for (const member of known.values()) {
    if (member.removed) continue;
    if (active.has(member.backendId)) throw new Error('A computer has conflicting device identities.');
    active.add(member.backendId);
  }
}

function memberships(): Map<string, Membership> {
  const raw = localStorage.getItem(MEMBERSHIP_KEY);
  const value: unknown = raw ? JSON.parse(raw) : [];
  if (!Array.isArray(value) || value.length > LIMIT || !value.every(validMembership)) {
    throw new Error('Saved device membership could not be read.');
  }
  const known = new Map(value.map((row: Membership) => [row.keyThumbprint, row]));
  if (known.size !== value.length) throw new Error('Saved device membership contains duplicate identities.');
  validateActiveIdentities(known);
  return known;
}

/** Keep removal knowledge across app restarts. An older sponsor cannot undo it;
 * only a later explicit enrollment generation can admit that device again.
 * These are public connection hints; destination sessions still authorize access. */
export function rememberOwnDeviceMemberships(rows: readonly { backendId?: string; keyThumbprint: string; generation: number; removed: boolean }[]): void {
  const known = memberships();
  let changed = false;
  for (const row of rows) {
    if (!row.backendId) continue;
    if (!validMembership(row)) throw new Error('A computer returned invalid device membership.');
    const old = known.get(row.keyThumbprint);
    if (old && old.backendId !== row.backendId) throw new Error('A device identity names conflicting computers.');
    if (old && (old.generation > row.generation || (old.generation === row.generation && (old.removed || !row.removed)))) continue;
    known.set(row.keyThumbprint, { backendId: row.backendId, keyThumbprint: row.keyThumbprint, generation: row.generation, removed: row.removed });
    changed = true;
  }
  if (known.size > LIMIT) throw new Error('This device group has reached its 128-device membership limit, including removed devices.');
  validateActiveIdentities(known);
  if (changed) localStorage.setItem(MEMBERSHIP_KEY, JSON.stringify([...known.values()]));
}

function removedComputers(): Set<string> {
  const known = [...memberships().values()];
  const removed = new Set(known.filter((member) => member.removed).map((member) => member.backendId));
  // An explicitly enrolled replacement key can own the same backend. Keep the
  // old key's tombstone without retiring the replacement's live connection.
  for (const member of known) if (!member.removed) removed.delete(member.backendId);
  return removed;
}

export function ownDeviceMembershipRemoved(id: string): boolean { return removedComputers().has(id); }

/** Read once for a reconciliation pass; re-read only across async admission. */
export function ownDeviceConnectionPolicy() {
  const removed = removedComputers();
  const blocked = excluded();
  return { removed: (id: string) => removed.has(id), excluded: (id: string) => blocked.has(id) || removed.has(id) };
}

function excluded(): Set<string> {
  const raw = localStorage.getItem(KEY);
  if (!raw) return new Set();
  const value: unknown = JSON.parse(raw);
  if (!Array.isArray(value) || value.length > LIMIT || value.some((id) => typeof id !== 'string' || !id || id.length > 128)) {
    throw new Error('Saved device connections could not be read. Automatic connections are paused.');
  }
  return new Set(value);
}

export function ownDeviceConnectionExcluded(id: string): boolean { return excluded().has(id) || ownDeviceMembershipRemoved(id); }

export function setOwnDeviceConnectionExcluded(id: string, blocked: boolean): void {
  if (!id) return;
  if (!blocked) {
    const known = memberships();
    const kept = [...known.values()].filter((member) => member.backendId !== id);
    if (kept.length !== known.size) localStorage.setItem(MEMBERSHIP_KEY, JSON.stringify(kept));
  }
  const next = excluded();
  if (next.has(id) === blocked) return;
  if (blocked) next.add(id); else next.delete(id);
  if (next.size > LIMIT) throw new Error('Too many removed device connections.');
  // Fail before forgetting credentials when persistence is unavailable.
  localStorage.setItem(KEY, JSON.stringify([...next]));
}
