import type { SourceDiffReview, SourceProposedPlan } from '../types/models';
import { randomId } from './randomId';

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
  revisionSourceDiffReview?: SourceDiffReview;
  revisionSourceDiffCommentIds?: string[];
  /**
   * Idempotency id for this one send, minted here and nowhere else.
   *
   * A socket that dies after the frame reached the backend looks exactly
   * like one that died before it: the RPC never answers, the composer puts
   * the text back, and pressing Send again starts the turn twice. The id is
   * what lets the backend recognise the second arrival as the same send and
   * answer it from the first one's record.
   *
   * It is minted in `buildSendOptions` because that is the one place BOTH
   * outgoing paths build their options — `SendMessageWithOptions` and
   * `RegisterQueueItem` — so a message that queues and a message that
   * dispatches carry the id on the same terms, and neither call site can
   * ship without one by forgetting.
   */
  sendId: string;
  /** This frontend reconciles provisional rows by send ID, not a predicted turn. */
  reconcileBySendId: true;
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
  revisionSourceProposedPlan?: SourceProposedPlan | null;
  revisionSourceCommentIds?: readonly string[];
  revisionSourceDiffReview?: SourceDiffReview | null;
  revisionSourceDiffCommentIds?: readonly string[];
}

/**
 * Resolve the precedence rule documented on `SendOptions`: a revision
 * always wins over a source-plan ref so a turn cannot simultaneously
 * revise and implement the same plan. Centralised here so the
 * composer's idle dispatch path, the queue's drain path, and any
 * future send vector stay aligned — three hand-rolled copies of the
 * same precedence drifted apart in pre-refactor history.
 *
 * ONE CALL IS ONE SEND. The returned options carry a freshly minted
 * `sendId`, so calling this twice for the same text produces two sends that
 * the backend will rightly treat as two messages. A RETRY must re-send the
 * options it already built, never rebuild them — which is exactly what the
 * transport's retained frame does (`RETRY_ON_TRANSIENT_CLOSE`).
 */
export function buildSendOptions(input: SendOptionsInput): OutgoingSendOptions {
  const out: OutgoingSendOptions = {
    attachmentIds: input.attachmentIds,
    sendId: randomId(),
    reconcileBySendId: true,
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
  if (input.revisionSourceDiffReview) {
    out.revisionSourceDiffReview = input.revisionSourceDiffReview;
  }
  if (input.revisionSourceDiffCommentIds && input.revisionSourceDiffCommentIds.length > 0) {
    out.revisionSourceDiffCommentIds = [...input.revisionSourceDiffCommentIds];
  }
  return out;
}
