import { describe, expect, it, vi } from 'vitest';
import type { WorkItemPhase, WorkflowItemDetail } from '../types/workflow';
import { envelopeText, parsePhaseEnvelope, workflowPartialOutputs, workflowQuestionText } from './workflowEnvelope';

function phase(phaseId: string, attempt: number, envelope: unknown): WorkItemPhase {
  return {
    itemId: 'run', phaseId, attempt, status: 'completed', startedAt: 0,
    outputEnvelope: typeof envelope === 'string' ? envelope : JSON.stringify(envelope),
  } as WorkItemPhase;
}

function detail(phases: WorkItemPhase[]): WorkflowItemDetail {
  return { item: { id: 'run' }, phases } as unknown as WorkflowItemDetail;
}

describe('parsePhaseEnvelope', () => {
  it('accepts an already-decoded object and a JSON string alike', () => {
    const decoded = { itemId: 'run', phaseId: 'p', attempt: 1, outputEnvelope: { status: 'done' } } as unknown as WorkItemPhase;
    expect(parsePhaseEnvelope(decoded)?.status).toBe('done');
    expect(parsePhaseEnvelope(phase('p', 1, { status: 'done' }))?.status).toBe('done');
  });

  it('warns and returns null for malformed JSON rather than throwing', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    expect(parsePhaseEnvelope(phase('p', 1, '{not json'))).toBeNull();
    expect(warn).toHaveBeenCalledTimes(1);
    warn.mockRestore();
  });

  it.each([
    ['absent', undefined],
    ['a JSON array', '[]'],
    ['a JSON scalar', '7'],
  ])('returns null for %s envelope', (_label, value) => {
    expect(parsePhaseEnvelope({ itemId: 'run', phaseId: 'p', attempt: 1, outputEnvelope: value } as unknown as WorkItemPhase)).toBeNull();
  });

  it('trims text and ignores non-strings', () => {
    expect(envelopeText('  hi  ')).toBe('hi');
    expect(envelopeText(7)).toBe('');
    expect(envelopeText(null)).toBe('');
  });
});

describe('workflowQuestionText', () => {
  it('takes the newest asking attempt', () => {
    expect(workflowQuestionText(detail([
      phase('ask', 1, { status: 'question', question: 'older?' }),
      phase('ask', 2, { status: 'question', question: 'newer?' }),
    ]))).toBe('newer?');
  });

  it('ignores envelopes that are not asking, and a question with no text', () => {
    expect(workflowQuestionText(detail([
      phase('done', 1, { status: 'done', question: 'not asked' }),
    ]))).toBe('');
    expect(workflowQuestionText(detail([phase('ask', 1, { status: 'question', question: '   ' })]))).toBe('');
    expect(workflowQuestionText(detail([]))).toBe('');
  });
});

describe('workflowPartialOutputs', () => {
  it('renders named values, never the envelope', () => {
    expect(workflowPartialOutputs(detail([
      phase('port', 1, { status: 'done', outputs: { summary: 'ported 4 files', count: 4 } }),
    ]))).toEqual(['summary: ported 4 files', 'count: 4']);
  });

  it('skips empty outputs and keeps walking back for a phase that captured something', () => {
    expect(workflowPartialOutputs(detail([
      phase('plan', 1, { status: 'done', outputs: { plan: 'do the thing' } }),
      phase('port', 1, { status: 'done', outputs: { empty: '', missing: null } }),
    ]))).toEqual(['plan: do the thing']);
    expect(workflowPartialOutputs(detail([phase('port', 1, { status: 'question', question: 'x' })]))).toEqual([]);
  });
});
