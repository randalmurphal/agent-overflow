import { describe, expect, it } from 'vitest';
import type { WorkItem } from '../types/workflow';
import {
  workflowActionForKey,
  workflowActionRow,
  workflowDigestFallback,
  workflowResolutionKind,
  type WorkflowResolutionKind,
} from './workflowActionRows';

function item(state: string, reason = ''): Pick<WorkItem, 'state' | 'reason'> {
  return { state, reason };
}

function ids(kind: WorkflowResolutionKind): string[] {
  return workflowActionRow({ kind }).map((action) => action.id);
}

const ALL_KINDS: WorkflowResolutionKind[] = [
  'gate', 'question', 'failed', 'blocked', 'paused', 'unit-failed', 'taken-over', 'done', 'running', 'cancelled',
];

describe('workflowResolutionKind', () => {
  it.each([
    ['running', '', 'running'],
    ['cancelled', '', 'cancelled'],
    ['done', '', 'done'],
    ['failed', 'check-failed-genuine', 'failed'],
    ['failed', 'child-failed', 'failed'],
    ['needs-human', 'gate', 'gate'],
    ['needs-human', 'question', 'question'],
    ['needs-human', 'unit-failed', 'unit-failed'],
    ['needs-human', 'taken-over', 'taken-over'],
    ['needs-human', 'paused', 'paused'],
    ['needs-human', 'interrupted', 'paused'],
    ['needs-human', 'disposition', 'done'],
    ['needs-human', 'stuck', 'blocked'],
    ['needs-human', 'agent-error', 'blocked'],
    ['needs-human', 'wiring-error', 'blocked'],
    ['needs-human', 'setup-failed', 'blocked'],
    ['needs-human', 'budget-exhausted', 'blocked'],
    ['needs-human', 'stalled', 'blocked'],
    ['needs-human', 'retries-exhausted', 'blocked'],
    ['needs-human', '', 'blocked'],
  ])('%s(%s) resolves on the %s row', (state, reason, expected) => {
    expect(workflowResolutionKind(item(state, reason))).toBe(expected);
  });

  it('treats an unknown state as failed rather than rendering no actions', () => {
    expect(workflowResolutionKind(item('teleported'))).toBe('failed');
  });
});

describe('workflowActionRow', () => {
  it('opens a review gate with approve, naming the phase it routes to', () => {
    const row = workflowActionRow({ kind: 'gate', nextPhaseId: 'docs' });
    expect(row.map((action) => action.id)).toEqual(['approve', 'request-changes']);
    expect(row[0]).toMatchObject({ label: 'Approve → docs', key: 'a', variant: 'primary' });
    expect(workflowActionRow({ kind: 'gate' })[0].label).toBe('Approve → next phase');
  });

  it('gives a question no buttons at all — the answer input is the affordance', () => {
    expect(ids('question')).toEqual([]);
  });

  it.each([
    ['failed', ['rerun', 'discard']],
    ['blocked', ['resume', 'discard']],
    ['paused', ['resume', 'discard']],
    ['unit-failed', ['retry-unit', 'retry-failed-units', 'drop-unit', 'take-over-unit', 'discard']],
    ['taken-over', ['complete-takeover', 'discard']],
    ['done', ['merge', 'create-pr', 'discard']],
    ['running', ['pause', 'open-phase-thread', 'cancel']],
    ['cancelled', ['discard', 'back']],
  ] as const)('%s offers exactly its §4.3 row', (kind, expected) => {
    expect(ids(kind)).toEqual([...expected]);
  });

  // D32 removed every affordance that spawned a NEW chat thread from a run:
  // run-level "Take over" / "Continue with agent" and the D17 "Open in thread"
  // seed+bind. Unit take-over (the one that steers an EXISTING unit thread) and
  // "Open phase thread" survive, so the row must keep exactly those two.
  it('offers no run-level thread spawn on any row, and keeps the two thread actions that open an existing thread', () => {
    const spawned = ALL_KINDS.flatMap((kind) => ids(kind))
      .filter((id) => id === 'take-over' || id === 'open-in-thread');
    expect(spawned).toEqual([]);
    expect(ids('unit-failed')).toContain('take-over-unit');
    expect(ids('running')).toContain('open-phase-thread');
  });

  // The reason `failed` and `blocked` are two rows. Each state has exactly one
  // engine edge back and the guards are mutually exclusive: `RerunFailed`
  // refuses anything that is not `failed`, `Resume` refuses anything that is
  // not `needs-human`. Offering the wrong one is a button that always errors,
  // which is what a single shared row used to do to every parked run.
  it.each([
    ['failed', 'rerun', 'resume'],
    ['blocked', 'resume', 'rerun'],
  ] as const)('%s offers the one continuation its state accepts, and never the other', (kind, accepted, refused) => {
    const row = ids(kind);
    expect(row).toContain(accepted);
    expect(row).not.toContain(refused);
  });

  it('arms every destruction that is not already behind the discard preview', () => {
    const armed = (kind: WorkflowResolutionKind) => workflowActionRow({ kind })
      .filter((action) => action.arms).map((action) => action.id);
    expect(armed('unit-failed')).toEqual(['drop-unit']);
    expect(armed('running')).toEqual(['cancel']);
    // Discard never arms inline: §4.5 makes the loss preview the consent.
    expect(armed('done')).toEqual([]);
    expect(armed('failed')).toEqual([]);
    expect(armed('blocked')).toEqual([]);
  });

  it('binds at most one action per key on every row', () => {
    for (const kind of ALL_KINDS) {
      const row = workflowActionRow({ kind });
      const keys = row.map((action) => action.key).filter(Boolean);
      expect(new Set(keys).size).toBe(keys.length);
      for (const key of keys) expect(['a', 'r', 't', 'u']).toContain(key);
    }
  });
});

describe('workflowActionForKey', () => {
  const row = workflowActionRow({ kind: 'gate', nextPhaseId: 'docs' });

  it('maps each §8 key to its action', () => {
    expect(workflowActionForKey(row, 'a')?.id).toBe('approve');
    expect(workflowActionForKey(row, 'r')?.id).toBe('request-changes');
    // §8's `t` now lives only on the unit-failed row.
    expect(workflowActionForKey(row, 't')).toBeNull();
    expect(workflowActionForKey(workflowActionRow({ kind: 'unit-failed' }), 't')?.id).toBe('take-over-unit');
  });

  it('returns null when the row does not bind that key', () => {
    const question = workflowActionRow({ kind: 'question' });
    for (const key of ['a', 'r', 't'] as const) expect(workflowActionForKey(question, key)).toBeNull();
  });
});

describe('workflowDigestFallback', () => {
  it('never renders empty and names the phase when one is known', () => {
    for (const kind of ALL_KINDS) {
      const digest = workflowDigestFallback(kind, 'check');
      expect(digest.whatHappened.length).toBeGreaterThan(0);
      expect(digest.whatItNeeds.length).toBeGreaterThan(0);
    }
    expect(workflowDigestFallback('gate', 'check').whatHappened).toContain('check');
    expect(workflowDigestFallback('gate', '').whatHappened).toContain('the run');
  });
});
