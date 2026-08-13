import { describe, expect, it } from 'vitest';
import { workflowAttentionLabel, workflowNodeTone, workflowRunSignal } from './workflowRunSignal';
import type { WorkflowRunReason, WorkflowRunState } from '../types/workflow';

describe('workflowRunSignal', () => {
  it.each([
    ['needs-human', 'gate', 'attention', 'Review gate'],
    ['needs-human', 'question', 'attention', 'Question'],
    ['needs-human', 'stuck', 'attention', 'Stuck'],
    ['needs-human', 'paused', 'attention', 'Paused'],
    ['needs-human', 'interrupted', 'attention', 'Interrupted'],
    ['needs-human', 'unit-failed', 'attention', 'Unit failed'],
    ['needs-human', 'disposition', 'attention', 'Disposition'],
    ['failed', 'check-failed-genuine', 'failed', 'Failed'],
    ['running', '', 'none', 'Running'],
    ['cancelled', '', 'none', 'Cancelled'],
    ['done', '', 'none', 'Done'],
  ])('%s(%s)', (state, reason, signal, label) => {
    expect(workflowRunSignal(state, reason)).toMatchObject({ signal, label });
  });

  it('never leaks an unknown reason token into the state word', () => {
    expect(workflowAttentionLabel('some-future-reason')).toBe('Needs you');
    expect(workflowAttentionLabel(undefined)).toBe('Needs you');
    expect(workflowRunSignal('needs-human', 'some-future-reason').label).toBe('Needs you');
  });

  it('keeps done awaiting disposition neutral', () => {
    expect(workflowRunSignal('done', 'disposition')).toMatchObject({ signal: 'none', tone: 'text-fg-muted' });
  });

  it('gives failed the red hue and no pulse — amber is reserved for needs-human', () => {
    const failed = workflowRunSignal('failed', 'agent-error');
    expect(failed).toMatchObject({ signal: 'failed', tone: 'text-error', dotClass: 'bg-error', pulse: false });
    expect(failed.glowClass).toBeUndefined();
    expect(workflowRunSignal('running', '').glowClass).toBeUndefined();
  });

  it.each([
    'gate', 'question', 'stuck', 'stalled', 'budget-exhausted', 'retries-exhausted',
    'provider-retries-exhausted', 'loop-limit-exhausted',
    'check-failed-genuine', 'agent-error', 'wiring-error', 'disposition', 'setup-failed',
    'interrupted', 'paused', 'unit-failed', 'child-failed', 'taken-over',
  ])('uses the sole amber signal for needs-human(%s)', (reason) => {
    expect(workflowRunSignal('needs-human', reason)).toMatchObject({
      signal: 'attention', dotClass: 'bg-warning', pulse: true, glowClass: 'status-glow-warning',
    });
  });
});

describe('workflowNodeTone', () => {
  it('reserves colour for failure and attention (R1)', () => {
    expect(workflowNodeTone('failed')).toBe('text-error');
    expect(workflowNodeTone('parked')).toBe('text-warning');
    for (const signal of ['done', 'running', 'pending', 'dropped'] as const) {
      expect(workflowNodeTone(signal)).toBe('text-fg-muted');
    }
  });
});

/**
 * Totality of the two label tables. They are typed `Record<Union, string>`, so a
 * reason the engine grows fails `pnpm run check` at the table — but only if the
 * union itself is kept current, and only a runtime sweep catches a member that
 * was added and then answered with the generic fallback anyway.
 */
describe('the label vocabulary is total over its unions', () => {
  // Typed as the union's record, so adding a reason to types/workflow.ts and
  // not to this list is a COMPILE error here as well as in the table.
  const EVERY_REASON: Record<WorkflowRunReason, true> = {
    gate: true, question: true, stuck: true, stalled: true, 'budget-exhausted': true,
    'retries-exhausted': true, 'provider-retries-exhausted': true,
    'loop-limit-exhausted': true, 'check-failed-genuine': true, 'agent-error': true,
    'wiring-error': true, disposition: true, 'setup-failed': true, interrupted: true,
    paused: true, 'unit-failed': true, 'child-failed': true, checkpoint: true,
    'taken-over': true,
  };
  const EVERY_STATE: Record<WorkflowRunState, true> = {
    running: true, 'needs-human': true, done: true, failed: true, cancelled: true,
  };

  it.each(Object.keys(EVERY_REASON) as WorkflowRunReason[])(
    'reason %s has a word of its own, not the fallback',
    (reason) => {
      expect(workflowAttentionLabel(reason)).not.toBe('Needs you');
    },
  );

  it.each(Object.keys(EVERY_STATE) as WorkflowRunState[])('state %s renders a word', (state) => {
    expect(workflowRunSignal(state, 'gate').label).not.toBe('');
  });

  it('answers a state this build has never heard of with no word at all', () => {
    // Better a missing word than a raw wire token on a user-facing surface.
    expect(workflowRunSignal('some-future-state', '')).toMatchObject({
      signal: 'none', label: '', tone: 'text-fg-muted',
    });
  });
});
