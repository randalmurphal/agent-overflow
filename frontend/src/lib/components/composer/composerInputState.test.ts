import { describe, expect, it } from 'vitest';
import {
  deriveComposerInputState,
  type ComposerInputStateInput,
} from './composerInputState';

function input(overrides: Partial<ComposerInputStateInput> = {}): ComposerInputStateInput {
  return {
    isDisabled: false,
    hasBlockingPrompt: false,
    hasUserInputPrompt: false,
    userInputCustomAnswer: 'custom answer',
    draftContent: 'draft text',
    hasDiffReviewSource: false,
    hasDraftDiffReviewComments: false,
    hasPlanSource: false,
    hasDraftPlanComments: false,
    ...overrides,
  };
}

describe('deriveComposerInputState', () => {
  it('uses the draft content for normal composer input', () => {
    expect(deriveComposerInputState(input())).toEqual({
      disabled: false,
      value: 'draft text',
      placeholder: 'Send a message… (Shift+Enter for newline, @ to mention a file)',
    });
  });

  it('disables input and shows the empty-thread placeholder when compose is unavailable', () => {
    expect(deriveComposerInputState(input({ isDisabled: true }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Select or create a thread to start',
    });
  });

  it('disables input and prioritizes approval prompt copy over other states', () => {
    expect(deriveComposerInputState(input({
      hasBlockingPrompt: true,
      hasUserInputPrompt: true,
      hasPlanSource: true,
      hasDraftPlanComments: true,
    }))).toEqual({
      disabled: true,
      value: 'custom answer',
      placeholder: 'Respond to the approval request to continue',
    });
  });

  it('uses the custom answer buffer for user-input prompts', () => {
    expect(deriveComposerInputState(input({ hasUserInputPrompt: true }))).toEqual({
      disabled: false,
      value: 'custom answer',
      placeholder: 'Type a custom answer, or choose an option above',
    });
  });

  it.each([
    {
      case: 'prioritizes diff-review comment copy over plan copy',
      overrides: { hasDiffReviewSource: true, hasDraftDiffReviewComments: true, hasPlanSource: true, hasDraftPlanComments: true },
      placeholder: 'Add optional notes, or send the diff comments',
    },
    {
      case: 'shows plan-comment copy when draft plan comments exist',
      overrides: { hasPlanSource: true, hasDraftPlanComments: true },
      placeholder: 'Add optional notes, or send the plan comments',
    },
    {
      case: 'shows plan refinement copy when only a plan source is active',
      overrides: { hasPlanSource: true },
      placeholder: 'Add feedback to refine the plan, or leave blank to implement it',
    },
  ])('$case', ({ overrides, placeholder }) => {
    expect(deriveComposerInputState(input(overrides)).placeholder).toBe(placeholder);
  });
});

describe('deriveComposerInputState — an unreachable machine', () => {
  it('disables the input and names the machine, after the read-only case and before the prompts', () => {
    expect(deriveComposerInputState(input({ sendAccess: 'offline', unreachableTarget: 'Laptop', hasBlockingPrompt: true }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Laptop is offline',
    });
    // composerAccess keeps a known refusal over the outage.
    expect(
      deriveComposerInputState(input({ unreachableTarget: 'Laptop', sendAccess: 'refused' })).placeholder,
    ).toBe('This device has read-only access');
  });

  // Reachable, but the computer has not said what this device may do, or
  // no computer is known to hold the thread: never "read-only".
  it('waits, and takes no message, while the grant has not arrived or the computer is not known', () => {
    expect(deriveComposerInputState(input({ sendAccess: 'pending' }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Waiting for this thread’s computer to answer',
    });
    expect(deriveComposerInputState(input({ sendAccess: 'unrouted' }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Waiting to learn which computer holds this thread',
    });
  });
});

describe('deriveComposerInputState, the page own backend being down', () => {
  it('disables the input and says the backend is disconnected', () => {
    expect(deriveComposerInputState(input({ sendAccess: 'offline' }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Disconnected from the agent backend',
    });
  });

  it('outranks the approval prompt, which cannot be answered while offline', () => {
    expect(
      deriveComposerInputState(input({ sendAccess: 'offline', hasBlockingPrompt: true })).placeholder,
    ).toBe('Disconnected from the agent backend');
  });
});

describe('deriveComposerInputState, compact layout', () => {
  it('drops the chord hint for a key the on-screen keyboard does not have', () => {
    expect(deriveComposerInputState(input({ compact: true })).placeholder)
      .toBe('Send a message… (@ to mention a file)');
  });

  it('changes nothing else: compact alone never disables the composer', () => {
    expect(deriveComposerInputState(input({ compact: true })).disabled).toBe(false);
  });

  it('yields to every placeholder that reports a state', () => {
    expect(deriveComposerInputState(input({ compact: true, sendAccess: 'offline' })).placeholder)
      .toBe('Disconnected from the agent backend');
    expect(deriveComposerInputState(input({ compact: true, hasPlanSource: true })).placeholder)
      .toBe('Add feedback to refine the plan, or leave blank to implement it');
  });
});

describe('deriveComposerInputState, an ask copy', () => {
  it('disables the input and says an agent question runs there, ranked under read-only access', () => {
    expect(deriveComposerInputState(input({ askCopy: true }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: "An agent's question runs in this copy. It takes no messages.",
    });
    expect(deriveComposerInputState(input({ askCopy: true, hasBlockingPrompt: true })).placeholder)
      .toBe("An agent's question runs in this copy. It takes no messages.");
    expect(deriveComposerInputState(input({ askCopy: true, sendAccess: 'refused' })).placeholder)
      .toBe('This device has read-only access');
  });
});
