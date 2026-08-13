import { describe, expect, it } from 'vitest';
import type { WorkItem, WorkItemPhase, WorkItemUnit, WorkflowItemDetail } from '../types/workflow';
import {
  failedWorkflowUnitInDetail,
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
  'gate', 'question', 'failed', 'blocked', 'paused', 'checkpoint', 'unit-failed', 'taken-over', 'done', 'running',
  'cancelled',
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
    ['needs-human', 'checkpoint', 'checkpoint'],
    ['needs-human', 'disposition', 'done'],
    ['needs-human', 'stuck', 'blocked'],
    ['needs-human', 'agent-error', 'blocked'],
    ['needs-human', 'wiring-error', 'blocked'],
    ['needs-human', 'setup-failed', 'blocked'],
    ['needs-human', 'budget-exhausted', 'blocked'],
    ['needs-human', 'stalled', 'blocked'],
    // Provider exhaustion continues the dead turn's session. Legacy rows keep
    // that contract, while a workflow loop limit has no provider turn to keep.
    ['needs-human', 'provider-retries-exhausted', 'paused'],
    ['needs-human', 'retries-exhausted', 'paused'],
    ['needs-human', 'loop-limit-exhausted', 'blocked'],
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
    ['checkpoint', ['resume', 'discard']],
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

  // The stop request is the one row entry that is not a function of the run's
  // state: it exists only where a call boundary can honour it, and its label is
  // the only place a human sees that a stop is already pending.
  it('offers no stop when the run has no boundary to stop at', () => {
    expect(ids('running')).toEqual(['pause', 'open-phase-thread', 'cancel']);
  });

  it.each([
    [false, 'soft-stop', 'Stop after this wave'],
    [true, 'clear-soft-stop', 'Stopping after this wave — undo'],
  ] as const)('renders the stop request armed=%s as %s', (isArmed, id, label) => {
    const row = workflowActionRow({ kind: 'running', softStop: { armed: isArmed } });
    expect(row.map((action) => action.id)).toEqual(['pause', id, 'open-phase-thread', 'cancel']);
    expect(row[1]).toMatchObject({ label });
    // Nothing is destroyed either way, so neither direction arms a confirm.
    expect(row[1].arms).toBeUndefined();
  });

  it('gives the stop request no key, leaving §8 bindings untouched', () => {
    const row = workflowActionRow({ kind: 'running', softStop: { armed: false } });
    expect(row.find((action) => action.id === 'soft-stop')?.key).toBeUndefined();
  });

  it('offers a checkpoint park the resume edge in its own words', () => {
    const row = workflowActionRow({ kind: 'checkpoint' });
    expect(row[0]).toMatchObject({ id: 'resume', label: 'Continue the run', key: 'a' });
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

const NOW = 1_000_000;

function phase(phaseId: string, attempt: number, status: string, startedAt: number): WorkItemPhase {
  return { itemId: 'run', phaseId, attempt, status, startedAt } as WorkItemPhase;
}

function unit(unitId: string, unitIndex: number, status: string, extra: Partial<WorkItemUnit> = {}): WorkItemUnit {
  return {
    itemId: 'run', phaseId: 'port', attempt: 1, unitId, unitIndex,
    kind: 'unit', status, unitAttempt: 1, ...extra,
  } as WorkItemUnit;
}

function detail(over: Partial<WorkflowItemDetail>): WorkflowItemDetail {
  return {
    item: { id: 'run' }, checkPhaseIds: [], phases: [], units: [], children: [],
    outputs: {}, artifacts: [], usage: { costUsd: 0 },
    ...over,
  } as unknown as WorkflowItemDetail;
}

describe('failedWorkflowUnitInDetail', () => {
  it('picks the lowest-index failed unit of the newest attempt that has one', () => {
    const found = failedWorkflowUnitInDetail(detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [unit('port-a', 0, 'done'), unit('port-c', 2, 'failed'), unit('port-b', 1, 'failed')],
    }));
    expect(found?.unit.unitId).toBe('port-b');
  });

  it('reads the attempt the run rests on, not the one a retry superseded', () => {
    const found = failedWorkflowUnitInDetail(detail({
      phases: [phase('port', 1, 'failed', 100), phase('port', 2, 'running', 200)],
      units: [
        unit('stale', 0, 'failed'),
        unit('current', 0, 'failed', { attempt: 2 }),
      ],
    }));
    expect(found?.unit.unitId).toBe('current');
  });

  it('falls back to a taken-over unit only when nothing failed outright', () => {
    const takenOver = detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [unit('port-a', 0, 'taken-over')],
    });
    expect(failedWorkflowUnitInDetail(takenOver)?.unit.unitId).toBe('port-a');
  });

  it('carries the label, the retry count, the elapsed span and the thread', () => {
    const found = failedWorkflowUnitInDetail(detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [unit('port-b', 1, 'failed', {
        unitAttempt: 2, startedAt: NOW - 60_000, endedAt: NOW, threadId: 'thread-unit',
      })],
    }));
    expect(found).toMatchObject({ label: 'port-b', meta: '×2 · 1m', threadId: 'thread-unit' });
  });

  it('names the join when the join itself is what failed', () => {
    const found = failedWorkflowUnitInDetail(detail({
      phases: [phase('port', 1, 'running', 100)],
      units: [unit('port-join', 9, 'failed', { kind: 'join' })],
    }));
    expect(found?.label).toBe('port-join (join)');
  });

  it('returns null for a run with no fan-out', () => {
    expect(failedWorkflowUnitInDetail(detail({ phases: [phase('plan', 1, 'completed', 1)] }))).toBeNull();
  });
});
