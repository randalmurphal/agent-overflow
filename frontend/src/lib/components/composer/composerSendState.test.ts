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

  it('does not render diff-review comment state without an active diff-review source', () => {
    expect(deriveComposerSendState(input({
      hasDraftDiffReviewComments: true,
    }))).toEqual({
      canSend: false,
      action: 'send',
      label: undefined,
      hasPlanImplementAction: false,
    });
  });

  // A draft that is a shell command: Send means Run, whatever else the
  // composer holds, and only a command that can run enables it.
  it('turns Send into Run for a shell command and enables it only when ready', () => {
    const run = { action: 'run', label: 'Run', hasPlanImplementAction: false };
    expect(deriveComposerSendState(input({ shell: 'ready', hasDraftContent: true }))).toEqual({ ...run, canSend: true });
    expect(deriveComposerSendState(input({ shell: 'empty', hasDraftContent: true }))).toEqual({ ...run, canSend: false });
    expect(deriveComposerSendState(input({ shell: 'blocked', hasDraftContent: true }))).toEqual({ ...run, canSend: false });
    // Plan and review actions do not take the button over while the draft is a command.
    expect(deriveComposerSendState(input({
      shell: 'ready', hasDraftContent: true, hasPlanSource: true, hasDraftPlanComments: true,
      hasDiffReviewSource: true, hasDraftDiffReviewComments: true,
    }))).toEqual({ ...run, canSend: true });
    // A turn in flight does not block a command: its output queues.
    expect(deriveComposerSendState(input({ shell: 'ready', hasDraftContent: true, isTurnActive: true })).canSend).toBe(true);
  });

  it('keeps every refusal that applies to a send in force for a command', () => {
    for (const refusal of [
      { isDisabled: true }, { sendUngranted: true }, { sending: true }, { sendSuspended: true },
      { hasBlockingPrompt: true }, { hasUserInputPrompt: true },
    ]) {
      expect(deriveComposerSendState(input({ shell: 'ready', hasDraftContent: true, ...refusal })).canSend).toBe(false);
    }
  });
});
