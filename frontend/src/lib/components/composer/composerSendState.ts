import type { SendButtonAction } from './toolbar/sendButtonTypes';

export interface ComposerSendStateInput {
  isDisabled: boolean;
  sending: boolean;
  hasBlockingPrompt: boolean;
  hasUserInputPrompt: boolean;
  hasDraftContent: boolean;
  hasPlanSource: boolean;
  hasDraftPlanComments: boolean;
  hasDiffReviewSource: boolean;
  hasDraftDiffReviewComments: boolean;
  isTurnActive: boolean;
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

  return {
    canSend: !input.isDisabled
      && !input.sending
      && !input.hasBlockingPrompt
      && !input.hasUserInputPrompt
      && (
        input.hasDraftContent
        || hasPlanCommentAction
        || hasDiffReviewCommentAction
        || (!input.isTurnActive && hasPlanImplementAction)
      ),
    action: sendAction(input),
    label: sendLabel(input),
    hasPlanImplementAction,
  };
}

function sendLabel(input: ComposerSendStateInput): string | undefined {
  if (input.hasDraftDiffReviewComments) return 'Send comments';
  if (!input.hasPlanSource || input.isTurnActive) return undefined;
  if (input.hasDraftPlanComments) return 'Send comments';
  if (!input.hasDraftContent) return 'Implement';
  return 'Refine';
}

function sendAction(input: ComposerSendStateInput): SendButtonAction {
  if (input.hasDraftDiffReviewComments) return 'send-comments';
  if (!input.hasPlanSource || input.isTurnActive) return 'send';
  if (input.hasDraftPlanComments) return 'send-comments';
  if (!input.hasDraftContent) return 'implement';
  return 'refine';
}
