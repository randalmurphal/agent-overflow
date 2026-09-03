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

  it('prioritizes diff-review comment copy over plan copy', () => {
    expect(deriveComposerInputState(input({
      hasDiffReviewSource: true,
      hasDraftDiffReviewComments: true,
      hasPlanSource: true,
      hasDraftPlanComments: true,
    })).placeholder).toBe('Add optional notes, or send the diff comments');
  });

  it('shows plan-comment copy when draft plan comments exist', () => {
    expect(deriveComposerInputState(input({
      hasPlanSource: true,
      hasDraftPlanComments: true,
    })).placeholder).toBe('Add optional notes, or send the plan comments');
  });

  it('shows plan refinement copy when only a plan source is active', () => {
    expect(deriveComposerInputState(input({ hasPlanSource: true })).placeholder)
      .toBe('Add feedback to refine the plan, or leave blank to implement it');
  });
});

describe('deriveComposerInputState — an unreachable machine', () => {
  it('disables the input and names the machine, after the read-only case and before the prompts', () => {
    expect(deriveComposerInputState(input({ unreachableTarget: 'Laptop', hasBlockingPrompt: true }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Laptop is unreachable',
    });
    expect(
      deriveComposerInputState(input({ unreachableTarget: 'Laptop', sendUngranted: true })).placeholder,
    ).toBe('This device has read-only access');
  });
});

describe('deriveComposerInputState, the page own backend being down', () => {
  it('disables the input and says so, ranked under the named machine', () => {
    expect(deriveComposerInputState(input({ offline: true }))).toEqual({
      disabled: true,
      value: 'draft text',
      placeholder: 'Disconnected from the agent backend',
    });
    // Both are true when another machine is what dropped. The named one
    // wins because it says more.
    expect(
      deriveComposerInputState(input({ offline: true, unreachableTarget: 'Laptop' })).placeholder,
    ).toBe('Laptop is unreachable');
    // Read-only outranks both: a session that cannot send says why once.
    expect(
      deriveComposerInputState(input({ offline: true, sendUngranted: true })).placeholder,
    ).toBe('This device has read-only access');
  });

  it('outranks the approval prompt, which cannot be answered while offline', () => {
    expect(
      deriveComposerInputState(input({ offline: true, hasBlockingPrompt: true })).placeholder,
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
    expect(deriveComposerInputState(input({ compact: true, offline: true })).placeholder)
      .toBe('Disconnected from the agent backend');
    expect(deriveComposerInputState(input({ compact: true, hasPlanSource: true })).placeholder)
      .toBe('Add feedback to refine the plan, or leave blank to implement it');
  });
});
