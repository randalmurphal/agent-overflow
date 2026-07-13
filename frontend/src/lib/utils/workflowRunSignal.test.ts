import { describe, expect, it } from 'vitest';
import { workflowRunSignal } from './workflowRunSignal';

describe('workflowRunSignal', () => {
  it.each([
    ['needs-human', 'gate', 'attention', 'Review gate'],
    ['needs-human', 'question', 'attention', 'Question'],
    ['needs-human', 'stuck', 'attention', 'Needs you'],
    ['failed', 'check-failed-genuine', 'failed', 'Failed'],
    ['running', '', 'none', 'Running'],
    ['queued', '', 'none', 'Queued'],
    ['cancelled', '', 'none', 'Cancelled'],
    ['done', '', 'none', 'Done'],
  ])('%s(%s)', (state, reason, signal, label) => {
    expect(workflowRunSignal(state, reason)).toMatchObject({ signal, label });
  });

  it('keeps done awaiting disposition neutral', () => {
    expect(workflowRunSignal('done', 'disposition')).toMatchObject({ signal: 'none', tone: 'text-fg-muted' });
  });

  it.each([
    'gate', 'question', 'stuck', 'stalled', 'budget-exhausted', 'retries-exhausted',
    'check-failed-genuine', 'agent-error', 'wiring-error', 'disposition', 'setup-failed',
    'interrupted', 'taken-over',
  ])('uses the sole amber signal for needs-human(%s)', (reason) => {
    expect(workflowRunSignal('needs-human', reason)).toMatchObject({
      signal: 'attention', dotClass: 'bg-warning', pulse: true, glowClass: 'status-glow-warning',
    });
  });
});
