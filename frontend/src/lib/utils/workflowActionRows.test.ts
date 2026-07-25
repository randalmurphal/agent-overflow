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

function ids(kind: WorkflowResolutionKind, over: { bound?: boolean; isChild?: boolean } = {}): string[] {
  return workflowActionRow({ kind, bound: false, isChild: false, ...over }).map((action) => action.id);
}

describe('workflowResolutionKind', () => {
  it.each([
    ['running', '', 'running'],
    ['cancelled', '', 'cancelled'],
    ['done', '', 'done'],
    ['failed', 'agent-error', 'stuck'],
    ['needs-human', 'gate', 'gate'],
    ['needs-human', 'question', 'question'],
    ['needs-human', 'unit-failed', 'unit-failed'],
    ['needs-human', 'taken-over', 'taken-over'],
    ['needs-human', 'paused', 'paused'],
    ['needs-human', 'interrupted', 'paused'],
    ['needs-human', 'disposition', 'done'],
    ['needs-human', 'stuck', 'stuck'],
    ['needs-human', 'child-failed', 'stuck'],
    ['needs-human', '', 'stuck'],
  ])('%s(%s) resolves on the %s row', (state, reason, expected) => {
    expect(workflowResolutionKind(item(state, reason))).toBe(expected);
  });

  it('treats an unknown state as stuck rather than rendering no actions', () => {
    expect(workflowResolutionKind(item('teleported'))).toBe('stuck');
  });
});

describe('workflowActionRow', () => {
  it('opens a review gate with approve, naming the phase it routes to', () => {
    const row = workflowActionRow({ kind: 'gate', nextPhaseId: 'docs', bound: false, isChild: false });
    expect(row.map((action) => action.id)).toEqual(['approve', 'request-changes', 'take-over']);
    expect(row[0]).toMatchObject({ label: 'Approve → docs', key: 'a', variant: 'primary' });
    expect(workflowActionRow({ kind: 'gate', bound: false, isChild: false })[0].label).toBe('Approve → next phase');
  });

  it('gives a question only the thread escape — the answer is not a button', () => {
    expect(ids('question')).toEqual(['take-over']);
  });

  it.each([
    ['stuck', ['take-over', 'rerun', 'discard']],
    ['paused', ['resume', 'take-over', 'discard']],
    ['unit-failed', ['retry-unit', 'drop-unit', 'take-over-unit', 'discard']],
    ['taken-over', ['complete-takeover', 'take-over', 'discard']],
    ['running', ['pause', 'open-phase-thread', 'cancel']],
    ['cancelled', ['discard', 'back']],
  ] as const)('%s offers exactly its §4.3 row', (kind, expected) => {
    expect(ids(kind)).toEqual([...expected]);
  });

  it('offers an unbound done run a thread to bind, and never offers one to a child (D18)', () => {
    expect(ids('done')).toEqual(['merge', 'create-pr', 'take-over', 'open-in-thread', 'discard']);
    expect(ids('done', { bound: true })).toEqual(['merge', 'create-pr', 'take-over', 'discard']);
    expect(ids('done', { isChild: true })).toEqual(['merge', 'create-pr', 'take-over', 'discard']);
  });

  it('arms every destruction that is not already behind the discard preview', () => {
    const armed = (kind: WorkflowResolutionKind) => workflowActionRow({ kind, bound: false, isChild: false })
      .filter((action) => action.arms).map((action) => action.id);
    expect(armed('unit-failed')).toEqual(['drop-unit']);
    expect(armed('running')).toEqual(['cancel']);
    // Discard never arms inline: §4.5 makes the loss preview the consent.
    expect(armed('done')).toEqual([]);
    expect(armed('stuck')).toEqual([]);
  });

  it('binds at most one action per key on every row', () => {
    const kinds: WorkflowResolutionKind[] = [
      'gate', 'question', 'stuck', 'paused', 'unit-failed', 'taken-over', 'done', 'running', 'cancelled',
    ];
    for (const kind of kinds) {
      const row = workflowActionRow({ kind, bound: false, isChild: false });
      const keys = row.map((action) => action.key).filter(Boolean);
      expect(new Set(keys).size).toBe(keys.length);
      for (const key of keys) expect(['a', 'r', 't']).toContain(key);
    }
  });
});

describe('workflowActionForKey', () => {
  const row = workflowActionRow({ kind: 'gate', nextPhaseId: 'docs', bound: false, isChild: false });

  it('maps each §8 key to its action', () => {
    expect(workflowActionForKey(row, 'a')?.id).toBe('approve');
    expect(workflowActionForKey(row, 'r')?.id).toBe('request-changes');
    expect(workflowActionForKey(row, 't')?.id).toBe('take-over');
  });

  it('returns null when the row does not bind that key', () => {
    const question = workflowActionRow({ kind: 'question', bound: false, isChild: false });
    expect(workflowActionForKey(question, 'a')).toBeNull();
    expect(workflowActionForKey(question, 'r')).toBeNull();
    expect(workflowActionForKey(question, 't')?.id).toBe('take-over');
  });
});

describe('workflowDigestFallback', () => {
  it('never renders empty and names the phase when one is known', () => {
    const kinds: WorkflowResolutionKind[] = [
      'gate', 'question', 'stuck', 'paused', 'unit-failed', 'taken-over', 'done', 'running', 'cancelled',
    ];
    for (const kind of kinds) {
      const digest = workflowDigestFallback(kind, 'check');
      expect(digest.whatHappened.length).toBeGreaterThan(0);
      expect(digest.whatItNeeds.length).toBeGreaterThan(0);
    }
    expect(workflowDigestFallback('gate', 'check').whatHappened).toContain('check');
    expect(workflowDigestFallback('gate', '').whatHappened).toContain('the run');
  });
});
