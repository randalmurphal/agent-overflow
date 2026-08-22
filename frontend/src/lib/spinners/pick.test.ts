import { beforeEach, describe, expect, it } from 'vitest';
import { __resetSpinnerPickForTest, assembleVerbPool, fnv1a, pickFromPool, stableTurnKey } from './pick';

describe('fnv1a', () => {
  it('is deterministic and spreads distinct inputs', () => {
    expect(fnv1a('a:b:c')).toBe(fnv1a('a:b:c'));
    expect(fnv1a('a:b:c')).not.toBe(fnv1a('a:b:d'));
  });
});

describe('pickFromPool', () => {
  const pool = ['a', 'b', 'c', 'd', 'e'];

  it('answers null for an empty pool', () => {
    expect(pickFromPool([], 't1', 'turn1', 'verb')).toBeNull();
  });

  it('is stable for the same thread + turn + salt', () => {
    const first = pickFromPool(pool, 't1', 'turn1', 'verb');
    expect(pickFromPool(pool, 't1', 'turn1', 'verb')).toBe(first);
  });

  it('varies across turns', () => {
    const picks = new Set(
      Array.from({ length: 20 }, (_, i) => pickFromPool(pool, 't1', `turn${i}`, 'verb')),
    );
    expect(picks.size).toBeGreaterThan(1);
  });

  it('decorrelates salts', () => {
    const verbPicks = Array.from({ length: 30 }, (_, i) =>
      pickFromPool(pool, 't1', `turn${i}`, 'verb'),
    );
    const spritePicks = Array.from({ length: 30 }, (_, i) =>
      pickFromPool(pool, 't1', `turn${i}`, 'sprite'),
    );
    expect(verbPicks).not.toEqual(spritePicks);
  });
});

describe('assembleVerbPool', () => {
  it('is builtins alone with no customs', () => {
    expect(assembleVerbPool(['A', 'B'], [], false)).toEqual(['A', 'B']);
  });

  it('appends customs', () => {
    expect(assembleVerbPool(['A'], ['X'], false)).toEqual(['A', 'X']);
  });

  it('drops builtins when disabled', () => {
    expect(assembleVerbPool(['A'], ['X'], true)).toEqual(['X']);
  });

  it('is empty when disabled with no customs', () => {
    expect(assembleVerbPool(['A'], [], true)).toEqual([]);
  });
});

describe('stableTurnKey', () => {
  beforeEach(() => __resetSpinnerPickForTest());

  it('a turn with no bridge keys on its own id and holds it', () => {
    expect(stableTurnKey('t1', 'turn-9')).toBe('turn-9');
    expect(stableTurnKey('t1', 'turn-9')).toBe('turn-9');
  });

  it('a bridge mints one key and holds it across re-derivations', () => {
    const key = stableTurnKey('t1', null);
    expect(stableTurnKey('t1', null)).toBe(key);
  });

  it('the turn following a bridge ADOPTS the bridge key (send-handoff stability)', () => {
    const bridgeKey = stableTurnKey('t1', null);
    expect(stableTurnKey('t1', 'turn-9')).toBe(bridgeKey);
    // And keeps holding it for the rest of the turn.
    expect(stableTurnKey('t1', 'turn-9')).toBe(bridgeKey);
  });

  it('a bridge after a finished turn reminits (next session rerolls)', () => {
    stableTurnKey('t1', 'turn-9');
    const next = stableTurnKey('t1', null);
    expect(next).not.toBe('turn-9');
    // The turn that follows adopts the fresh bridge key.
    expect(stableTurnKey('t1', 'turn-10')).toBe(next);
  });

  it('a new turn id with no bridge between rerolls (chained sends)', () => {
    stableTurnKey('t1', 'turn-9');
    expect(stableTurnKey('t1', 'turn-10')).toBe('turn-10');
  });

  it('keeps threads independent', () => {
    const t1 = stableTurnKey('t1', null);
    const t2 = stableTurnKey('t2', null);
    expect(t1).not.toBe(t2);
    expect(stableTurnKey('t1', null)).toBe(t1);
  });
});
