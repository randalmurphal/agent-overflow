import { describe, expect, it } from 'vitest';
import { workflowAttentionLabel, workflowRunSignal } from './workflowRunSignal';

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
    'check-failed-genuine', 'agent-error', 'wiring-error', 'disposition', 'setup-failed',
    'interrupted', 'paused', 'unit-failed', 'child-failed', 'taken-over',
  ])('uses the sole amber signal for needs-human(%s)', (reason) => {
    expect(workflowRunSignal('needs-human', reason)).toMatchObject({
      signal: 'attention', dotClass: 'bg-warning', pulse: true, glowClass: 'status-glow-warning',
    });
  });
});
