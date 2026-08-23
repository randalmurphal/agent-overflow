// Per-turn spinner picks: which verb and which sprite a working turn
// shows. Both are DETERMINISTIC over (threadId, turnKey) — a hash, not
// Math.random — so re-renders, pane remounts, and thread switches all
// re-derive the same pick instead of rerolling mid-turn, and no store
// has to remember what was rolled.
//
// The one piece of memory is the bridge: between a queued send and the
// next turn's registration there is a working chip with no ActiveTurn.
// `stableTurnKey` hands back the last real turn's key for that gap so
// the verb/sprite hold steady instead of flickering through a reroll.

import { fnv1a32 } from '../utils/fnv1a';

/**
 * Pick one entry from a pool, stable for a given (threadId, turnKey,
 * salt). The salt keeps the verb pick and the sprite pick independent —
 * without it the two pools would correlate whenever their sizes share a
 * factor.
 */
export function pickFromPool<T>(pool: readonly T[], threadId: string, turnKey: string, salt: string): T | null {
  if (pool.length === 0) return null;
  return pool[fnv1a32(`${salt}:${threadId}:${turnKey}`) % pool.length] ?? null;
}

/**
 * Assemble the verb pool for a turn: built-ins unless disabled, plus the
 * user's custom verbs. An empty assembled pool means "verbs off in
 * practice" and callers fall back to the plain Working label.
 */
export function assembleVerbPool(
  builtins: readonly string[],
  custom: readonly string[],
  builtinsDisabled: boolean,
): readonly string[] {
  const base = builtinsDisabled ? [] : builtins;
  if (custom.length === 0) return base;
  return [...base, ...custom];
}

// threadId → the current working session's pick entry. `turnId` is null
// while the session is still a bridge (queued send, no registered turn
// yet) and adopts the turn id when the provider registers it. Values are
// short strings and threads are bounded per session; staleness is
// resolved structurally below rather than by a cleanup hook.
interface PickEntry {
  turnId: string | null;
  key: string;
}
const pickEntries = new Map<string, PickEntry>();
let bridgeNonce = 0;

/**
 * The identity picks hash over — one per WORKING SESSION, where a
 * session is the bridge (queued send, no ActiveTurn yet) plus the turn
 * it becomes. The bridge mints the key and the turn ADOPTS it: the
 * activity rail's working chip must not change width when the provider
 * registers the turn (the send-handoff stability contract pinned by
 * activityRailHandoff.browser.test.ts), so the verb and sprite picked
 * during the bridge hold through the whole turn.
 *
 * Staleness is structural: a bridge never coexists with its own
 * registered turn, so an entry carrying a non-null turnId seen during a
 * bridge belongs to a FINISHED turn and is reminted — which is also what
 * makes the next session reroll instead of repeating the last verb.
 * A turn id different from the adopted one is a genuinely new turn
 * (chained sends with no idle gap) and rerolls too.
 */
export function stableTurnKey(threadId: string, activeTurnId: string | null): string {
  const entry = pickEntries.get(threadId);
  if (activeTurnId === null) {
    if (entry !== undefined && entry.turnId === null) return entry.key;
    bridgeNonce += 1;
    const minted: PickEntry = { turnId: null, key: `bridge:${bridgeNonce}` };
    pickEntries.set(threadId, minted);
    return minted.key;
  }
  if (entry !== undefined) {
    if (entry.turnId === null) {
      // The bridge's session becomes this turn; keep its pick.
      entry.turnId = activeTurnId;
      return entry.key;
    }
    if (entry.turnId === activeTurnId) return entry.key;
  }
  const fresh: PickEntry = { turnId: activeTurnId, key: activeTurnId };
  pickEntries.set(threadId, fresh);
  return fresh.key;
}

/** Test seam: forget working-session memory. */
export function __resetSpinnerPickForTest(): void {
  pickEntries.clear();
  bridgeNonce = 0;
}
