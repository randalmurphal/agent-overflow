import type { SourceProposedPlan } from '../types/models';

/**
 * Wire shape of the third argument to `SendMessageWithOptions`. Every
 * outgoing send (idle dispatch from the composer, drained queue item)
 * funnels through the same shape so the backend never sees provenance
 * differences between "user just clicked Send" and "queued message
 * just drained."
 */
export interface OutgoingSendOptions {
  attachmentIds: string[];
  runtimeMode?: string;
  sourceProposedPlan?: SourceProposedPlan;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: string[];
}

/**
 * Input the composer assembles before knowing whether it will dispatch
 * directly or queue. Carries the full revision-vs-source-plan ambiguity;
 * `buildSendOptions` resolves the precedence in one place so both call
 * sites produce identical wire payloads.
 */
export interface SendOptionsInput {
  attachmentIds: string[];
  runtimeMode?: string;
  sourceProposedPlan?: SourceProposedPlan | null;
  revisionSourceProposedPlan?: SourceProposedPlan;
  revisionSourceCommentIds?: readonly string[];
}

/**
 * Resolve the precedence rule documented on `SendOptions`: a revision
 * always wins over a source-plan ref so a turn cannot simultaneously
 * revise and implement the same plan. Centralised here so the
 * composer's idle dispatch path, the queue's drain path, and any
 * future send vector stay aligned — three hand-rolled copies of the
 * same precedence drifted apart in pre-refactor history.
 */
export function buildSendOptions(input: SendOptionsInput): OutgoingSendOptions {
  const out: OutgoingSendOptions = {
    attachmentIds: input.attachmentIds,
  };
  if (input.runtimeMode) out.runtimeMode = input.runtimeMode;
  if (input.revisionSourceProposedPlan) {
    out.revisionSourceProposedPlan = input.revisionSourceProposedPlan;
  } else if (input.sourceProposedPlan) {
    out.sourceProposedPlan = input.sourceProposedPlan;
  }
  if (input.revisionSourceCommentIds && input.revisionSourceCommentIds.length > 0) {
    out.revisionSourceCommentIds = [...input.revisionSourceCommentIds];
  }
  return out;
}
