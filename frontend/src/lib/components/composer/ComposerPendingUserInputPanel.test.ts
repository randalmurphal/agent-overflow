import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import ComposerPendingUserInputPanel from './ComposerPendingUserInputPanel.svelte';
import type { UserInputRequest } from '../../types/events';
import type { UserInputResponse } from '../../stores/bindings';

function request(overrides: Partial<UserInputRequest> = {}): UserInputRequest {
  return {
    requestId: 'req-user-input',
    threadId: 'thread-1',
    toolName: 'request_user_input',
    title: 'User Input Required',
    questions: [
      {
        id: 'framework',
        header: 'Framework',
        question: 'Pick one',
        options: [
          { label: 'React', description: '' },
          { label: 'Svelte', description: '' },
        ],
      },
    ],
    ...overrides,
  };
}

describe('<ComposerPendingUserInputPanel>', () => {
  it('auto-submits the final single-select answer', async () => {
    vi.useFakeTimers();
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const { getByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request(),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve,
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    await fireEvent.click(getByTestId('user-input-option-2'));
    await vi.advanceTimersByTimeAsync(200);

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ framework: 'Svelte' });
    vi.useRealTimers();
  });

  it('clears the selected option while a custom answer is active', async () => {
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request(),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
    };
    const { getByTestId, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-option-1'));
    await rerender({ ...props, customAnswer: 'Other framework' });
    await rerender({ ...props, customAnswer: '' });
    await fireEvent.click(getByTestId('user-input-submit'));

    expect(onResolve).not.toHaveBeenCalled();
  });

  it('uses submitSignal to submit a final custom answer from composer Enter', async () => {
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request({ questions: [{ id: 'name', header: 'Name', question: 'Name?' }] }),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
    };
    const { rerender } = render(ComposerPendingUserInputPanel, { props });

    await rerender({ ...props, customAnswer: 'Randy' });
    await rerender({ ...props, customAnswer: 'Randy', submitSignal: 1 });

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ name: 'Randy' });
  });
});
