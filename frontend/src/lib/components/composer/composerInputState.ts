export interface ComposerInputStateInput {
  isDisabled: boolean;
  hasBlockingPrompt: boolean;
  hasUserInputPrompt: boolean;
  userInputCustomAnswer: string;
  draftContent: string;
  hasDiffReviewSource: boolean;
  hasDraftDiffReviewComments: boolean;
  hasPlanSource: boolean;
  hasDraftPlanComments: boolean;
}

export interface ComposerInputState {
  disabled: boolean;
  value: string;
  placeholder: string;
}

export function deriveComposerInputState(input: ComposerInputStateInput): ComposerInputState {
  return {
    disabled: input.isDisabled || input.hasBlockingPrompt,
    value: input.hasUserInputPrompt ? input.userInputCustomAnswer : input.draftContent,
    placeholder: inputPlaceholder(input),
  };
}

function inputPlaceholder(input: ComposerInputStateInput): string {
  if (input.isDisabled) return 'Select or create a thread to start';
  if (input.hasBlockingPrompt) return 'Respond to the approval request to continue';
  if (input.hasUserInputPrompt) return 'Type a custom answer, or choose an option above';
  if (input.hasDiffReviewSource && input.hasDraftDiffReviewComments) {
    return 'Add optional notes, or send the diff comments';
  }
  if (input.hasPlanSource && input.hasDraftPlanComments) {
    return 'Add optional notes, or send the plan comments';
  }
  if (input.hasPlanSource) return 'Add feedback to refine the plan, or leave blank to implement it';
  return 'Send a message… (Shift+Enter for newline, @ to mention a file)';
}
