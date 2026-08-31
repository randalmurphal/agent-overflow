import type { SendButtonAction } from './toolbar/sendButtonTypes';

export interface ComposerSendStateInput {
  isDisabled: boolean;
  /**
   * This session was not granted `threads:operate`, so nothing the send
   * button does can reach the backend. Separate from `isDisabled`, which
   * is "there is no thread to send into": the two want different
   * placeholder copy, and only this one is a standing property of the
   * session rather than of the pane.
   */
  sendUngranted?: boolean;
  sending: boolean;
  hasBlockingPrompt: boolean;
  hasUserInputPrompt: boolean;
  hasDraftContent: boolean;
  hasPlanSource: boolean;
  hasDraftPlanComments: boolean;
  hasDiffReviewSource: boolean;
  hasDraftDiffReviewComments: boolean;
  isTurnActive: boolean;
  /**
   * A thread-level operation owns sending right now — the edit-and-resend
   * saga, which reverts and re-sends under one backend lock. Distinct
   * from `sending` (this composer's own send) because nothing here
   * started it and nothing here can stop it; the button just refuses.
   */
  sendSuspended?: boolean;
}

export interface ComposerSendState {
  canSend: boolean;
  action: SendButtonAction;
  label: string | undefined;
  hasPlanImplementAction: boolean;
}

export function deriveComposerSendState(input: ComposerSendStateInput): ComposerSendState {
  const hasPlanCommentAction = input.hasPlanSource && input.hasDraftPlanComments;
  const hasDiffReviewCommentAction = input.hasDiffReviewSource && input.hasDraftDiffReviewComments;
  const hasPlanImplementAction = input.hasPlanSource
    && !input.hasDraftContent
    && !input.hasDraftPlanComments;
  const action = sendAction(input, {
    hasPlanCommentAction,
    hasDiffReviewCommentAction,
  });

  return {
    // Drafts and comment sends are queueable mid-round. Implement-only
    // still requires an idle turn because it runs the plan implementation
    // helper, not just a SendMessage RPC.
    canSend: !input.isDisabled
      && !input.sendUngranted
      && !input.sending
      && !input.sendSuspended
      && !input.hasBlockingPrompt
      && !input.hasUserInputPrompt
      && (
        input.hasDraftContent
        || hasPlanCommentAction
        || hasDiffReviewCommentAction
        || (!input.isTurnActive && hasPlanImplementAction)
      ),
    action,
    label: sendLabel(action),
    hasPlanImplementAction,
  };
}

function sendLabel(action: SendButtonAction): string | undefined {
  if (action === 'send-comments') return 'Send comments';
  if (action === 'implement') return 'Implement';
  if (action === 'refine') return 'Refine';
  return undefined;
}

function sendAction(
  input: ComposerSendStateInput,
  normalized: {
    hasPlanCommentAction: boolean;
    hasDiffReviewCommentAction: boolean;
  },
): SendButtonAction {
  if (normalized.hasDiffReviewCommentAction) return 'send-comments';
  if (!input.hasPlanSource || input.isTurnActive) return 'send';
  if (normalized.hasPlanCommentAction) return 'send-comments';
  if (!input.hasDraftContent) return 'implement';
  return 'refine';
}
