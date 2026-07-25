// Reading a phase's output envelope. The envelope itself is an internal wire
// shape and MUST NOT render (R2) — these helpers exist so the surface can pull
// the two human-readable strings out of it (the question a phase asked, the
// partial result a stopped phase managed to record) and nothing else.
//
// Pure and total: a malformed or absent envelope is "no text", never a throw
// and never a rendered JSON blob.

import type { WorkItemPhase, WorkflowItemDetail } from '../types/workflow';

export interface WorkflowPhaseEnvelope {
  status?: string;
  outputs?: Record<string, unknown> | null;
  question?: string;
  reason?: string;
}

export function parsePhaseEnvelope(phase: WorkItemPhase): WorkflowPhaseEnvelope | null {
  if (!phase.outputEnvelope) return null;
  try {
    const parsed = typeof phase.outputEnvelope === 'string'
      ? JSON.parse(phase.outputEnvelope) as unknown
      : phase.outputEnvelope as unknown;
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed)
      ? parsed as WorkflowPhaseEnvelope
      : null;
  } catch (error) {
    console.warn(`workflows: could not parse the envelope for ${phase.phaseId} attempt ${phase.attempt}`, error);
    return null;
  }
}

export function envelopeText(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

/** The question a parked phase asked — newest attempt wins (§4.3 question). */
export function workflowQuestionText(detail: WorkflowItemDetail): string {
  for (const phase of [...(detail.phases ?? [])].reverse()) {
    const envelope = parsePhaseEnvelope(phase);
    if (envelope?.status === 'question') {
      const question = envelopeText(envelope.question);
      if (question) return question;
    }
  }
  return '';
}

/**
 * What a stopped phase managed to record before it stopped (§4.3
 * paused/interrupted: "partial-envelope digest if one was captured"). Named
 * outputs only, rendered as `name: value` — never the envelope itself.
 */
export function workflowPartialOutputs(detail: WorkflowItemDetail): string[] {
  for (const phase of [...(detail.phases ?? [])].reverse()) {
    const outputs = parsePhaseEnvelope(phase)?.outputs;
    if (!outputs) continue;
    const lines = Object.entries(outputs)
      .filter(([, value]) => value !== null && value !== undefined && value !== '')
      .map(([name, value]) => `${name}: ${typeof value === 'string' ? value : JSON.stringify(value)}`);
    if (lines.length > 0) return lines;
  }
  return [];
}
