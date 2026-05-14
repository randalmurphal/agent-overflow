import { describe, expect, it } from 'vitest';
import {
  deriveComposerSendState,
  type ComposerSendStateInput,
} from './composerSendState';

function input(overrides: Partial<ComposerSendStateInput> = {}): ComposerSendStateInput {
  return {
    isDisabled: false,
    sending: false,
    hasBlockingPrompt: false,
    hasUserInputPrompt: false,
    hasDraftContent: false,
    hasPlanSource: false,
    hasDraftPlanComments: false,
    hasDiffReviewSource: false,
    hasDraftDiffReviewComments: false,
    isTurnActive: false,
    ...overrides,
  };
}

describe('deriveComposerSendState', () => {
  it('allows normal draft sends without a custom label', () => {
    expect(deriveComposerSendState(input({ hasDraftContent: true }))).toEqual({
      canSend: true,
      action: 'send',
      label: undefined,
      hasPlanImplementAction: false,
    });
  });

  it('keeps draft sends queueable during active turns', () => {
    expect(deriveComposerSendState(input({
      hasDraftContent: true,
      isTurnActive: true,
    })).canSend).toBe(true);
  });

  it.each([
    ['disabled thread', { isDisabled: true }],
    ['send in flight', { sending: true }],
    ['approval prompt', { hasBlockingPrompt: true }],
    ['user-input prompt', { hasUserInputPrompt: true }],
  ])('blocks sends for %s', (_, overrides) => {
    expect(deriveComposerSendState(input({
      hasDraftContent: true,
      ...overrides,
    })).canSend).toBe(false);
  });

  it('derives idle plan implementation state', () => {
    expect(deriveComposerSendState(input({ hasPlanSource: true }))).toEqual({
      canSend: true,
      action: 'implement',
      label: 'Implement',
      hasPlanImplementAction: true,
    });
  });

  it('blocks implement-only plan actions during active turns', () => {
    expect(deriveComposerSendState(input({
      hasPlanSource: true,
      isTurnActive: true,
    }))).toEqual({
      canSend: false,
      action: 'send',
      label: undefined,
      hasPlanImplementAction: true,
    });
  });

  it('derives plan refine state when draft content is present', () => {
    expect(deriveComposerSendState(input({
      hasDraftContent: true,
      hasPlanSource: true,
    }))).toEqual({
      canSend: true,
      action: 'refine',
      label: 'Refine',
      hasPlanImplementAction: false,
    });
  });

  it('derives plan-comment state even without draft content', () => {
    expect(deriveComposerSendState(input({
      hasPlanSource: true,
      hasDraftPlanComments: true,
    }))).toEqual({
      canSend: true,
      action: 'send-comments',
      label: 'Send comments',
      hasPlanImplementAction: false,
    });
  });

  it('derives diff-review comment state ahead of plan state', () => {
    expect(deriveComposerSendState(input({
      hasPlanSource: true,
      hasDiffReviewSource: true,
      hasDraftDiffReviewComments: true,
    }))).toEqual({
      canSend: true,
      action: 'send-comments',
      label: 'Send comments',
      hasPlanImplementAction: true,
    });
  });
});
