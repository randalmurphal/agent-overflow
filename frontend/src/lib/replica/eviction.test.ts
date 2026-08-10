import { describe, expect, it } from 'vitest';
import { planRemoval, planWrite } from './eviction';
import { MAX_REPLICA_CHARS, MAX_REPLICA_THREADS } from './envelope';
import type { ReplicaIndexEntry } from './idb';

function entry(threadId: string, savedAt: number, chars = 10): ReplicaIndexEntry {
  return { threadId, savedAt, chars };
}

describe('replica eviction planning', () => {
  it('keeps the incoming row and evicts nothing while under both caps', () => {
    const plan = planWrite([entry('a', 1), entry('b', 2)], entry('c', 3));
    expect(plan.evict).toEqual([]);
    expect(plan.entries.map((e) => e.threadId)).toEqual(['a', 'b', 'c']);
  });

  it('replaces an existing row for the same thread rather than duplicating it', () => {
    const plan = planWrite([entry('a', 1), entry('b', 2)], entry('a', 9, 25));
    expect(plan.entries.map((e) => e.threadId)).toEqual(['b', 'a']);
    expect(plan.entries.find((e) => e.threadId === 'a')?.chars).toBe(25);
    expect(plan.evict).toEqual([]);
  });

  it('evicts oldest-first past the thread cap', () => {
    const current = Array.from({ length: MAX_REPLICA_THREADS }, (_, index) =>
      entry(`t-${index}`, index),
    );
    const plan = planWrite(current, entry('incoming', 9_999));
    expect(plan.evict).toEqual(['t-0']);
    expect(plan.entries).toHaveLength(MAX_REPLICA_THREADS);
    expect(plan.entries.at(-1)?.threadId).toBe('incoming');
  });

  it('evicts by savedAt until the replica-wide char budget fits', () => {
    const half = Math.floor(MAX_REPLICA_CHARS / 2);
    const current = [entry('old', 1, half), entry('newer', 2, half)];
    const plan = planWrite(current, entry('incoming', 3, half));
    expect(plan.evict).toEqual(['old']);
    expect(plan.entries.map((e) => e.threadId)).toEqual(['newer', 'incoming']);
  });

  it('never evicts the row being written, even alone over budget', () => {
    const plan = planWrite([], entry('incoming', 1, MAX_REPLICA_CHARS + 1));
    expect(plan.evict).toEqual([]);
    expect(plan.entries.map((e) => e.threadId)).toEqual(['incoming']);
  });

  it('drops exactly one thread on removal', () => {
    const plan = planRemoval([entry('a', 1), entry('b', 2)], 'a');
    expect(plan.entries.map((e) => e.threadId)).toEqual(['b']);
    expect(plan.evict).toEqual([]);
    expect(planRemoval([entry('a', 1)], 'missing').entries.map((e) => e.threadId)).toEqual(['a']);
  });

  // Removal re-sweeps because the rows it merges against come from the
  // database, not from this page: another page on the origin can have
  // pushed the stored set past the caps between our plans.
  it('re-enforces the caps on removal when the merged set is over budget', () => {
    const over = Array.from({ length: MAX_REPLICA_THREADS + 2 }, (_, index) =>
      entry(`t-${index}`, index),
    );
    const plan = planRemoval(over, 't-5');
    expect(plan.entries).toHaveLength(MAX_REPLICA_THREADS);
    expect(plan.evict).toEqual(['t-0']);
    expect(plan.entries.some((e) => e.threadId === 't-5')).toBe(false);
    expect(plan.entries.some((e) => e.threadId === 't-0')).toBe(false);
  });
});
