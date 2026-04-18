import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import UserInputPanel from './UserInputPanel.svelte';
import { ApprovalResponse } from '../../../stores/bindings';
import type { ApprovalRequest } from '../../../types/events';

// Typed resolver so .mock.calls[0][0] is ApprovalResponse, not undefined.
function makeResolver(
  impl: (r: ApprovalResponse) => Promise<void> = async () => {},
) {
  return vi.fn<(response: ApprovalResponse) => Promise<void>>(impl);
}

function baseApproval(overrides: Partial<ApprovalRequest> = {}): ApprovalRequest {
  return {
    requestId: 'req-ui',
    threadId: 'thread-1',
    turnId: 'turn-1',
    toolName: 'ask_user',
    description: 'Please answer',
    input: {},
    title: 'User input',
    kind: 'user-input',
    questions: [
      { id: 'q1', header: 'Question 1', question: 'What is your name?' },
    ],
    ...overrides,
  };
}

describe('<UserInputPanel>', () => {
  it('renders each question with a text input when no options are provided', () => {
    const { getByTestId } = render(UserInputPanel, {
      props: {
        approval: baseApproval(),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    expect(getByTestId('user-input-text-q1')).toBeInTheDocument();
  });

  it('renders a <select> when a question has options', () => {
    const { getByTestId } = render(UserInputPanel, {
      props: {
        approval: baseApproval({
          questions: [
            {
              id: 'q1',
              header: '',
              question: 'Pick one',
              options: [
                { label: 'Yes', description: '' },
                { label: 'No', description: 'not this' },
              ],
            },
          ],
        }),
        onResolve: makeResolver(),
        onError: vi.fn(),
      },
    });
    const select = getByTestId('user-input-select-q1') as HTMLSelectElement;
    expect(select.options.length).toBe(3); // placeholder + two options
    // The placeholder comes first, then the user-provided labels.
    expect(Array.from(select.options).slice(1).map((o) => o.value)).toEqual(['Yes', 'No']);
  });

  it('submits collected answers as { decision: allow, answers }', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(UserInputPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.input(getByTestId('user-input-text-q1'), {
      target: { value: 'Randy' },
    });
    await fireEvent.click(getByTestId('user-input-submit'));
    expect(onResolve).toHaveBeenCalledTimes(1);
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.requestId).toBe('req-ui');
    expect(response.decision).toBe('allow');
    expect(response.answers).toEqual({ q1: 'Randy' });
  });

  it('Submit emits an empty answers record when no question has been touched', async () => {
    // This is the caller-side contract: the backend decides whether a missing
    // required answer is an error. The panel just ships what the user filled in.
    const onResolve = makeResolver();
    const { getByTestId } = render(UserInputPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('user-input-submit'));
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.answers).toEqual({});
  });

  it('Cancel emits a deny response', async () => {
    const onResolve = makeResolver();
    const { getByTestId } = render(UserInputPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError: vi.fn(),
      },
    });
    await fireEvent.click(getByTestId('user-input-cancel'));
    const response = onResolve.mock.calls[0][0] as ApprovalResponse;
    expect(response.decision).toBe('deny');
    expect(response.answers).toBeUndefined();
  });

  it('surfaces a resolver rejection via onError', async () => {
    const onResolve = makeResolver(async () => {
      throw new Error('boom');
    });
    const onError = vi.fn();
    const { getByTestId } = render(UserInputPanel, {
      props: {
        approval: baseApproval(),
        onResolve,
        onError,
      },
    });
    await fireEvent.click(getByTestId('user-input-submit'));
    // Microtask drain — onResolve awaits then throws.
    await Promise.resolve();
    await Promise.resolve();
    expect(onError).toHaveBeenCalled();
    expect(onError.mock.calls[0][0]).toMatch(/Failed to submit input/i);
  });
});
