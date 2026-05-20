import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
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
  it('does not auto-submit when the user clicks an option', async () => {
    // Mouse click should select only — auto-advance/auto-submit is the
    // keyboard fast path. The user has to click the explicit submit
    // button to send. Regression guard for "I clicked an option and
    // the dialog jumped on me before I could change my mind."
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
    await vi.advanceTimersByTimeAsync(500);

    expect(onResolve).not.toHaveBeenCalled();
    vi.useRealTimers();
  });

  it('submits via the explicit Submit button after a mouse click', async () => {
    // The post-click commit path: select with mouse, then click
    // "Submit answer(s)". The resolve carries the selected option.
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
    await fireEvent.click(getByTestId('user-input-submit'));

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ framework: 'Svelte' });
  });

  it('auto-submits when the user picks an option via the keyboard number key', async () => {
    // Keyboard fast path: number keys still auto-advance/auto-submit.
    // This is the existing behavior we explicitly preserved when
    // splitting click and keyboard into separate origins.
    vi.useFakeTimers();
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    render(ComposerPendingUserInputPanel, {
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

    await fireEvent.keyDown(window, { key: '2' });
    await vi.advanceTimersByTimeAsync(200);

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ framework: 'Svelte' });
    vi.useRealTimers();
  });

  it('auto-submits when Enter selects a focused option', async () => {
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

    const option = getByTestId('user-input-option-2') as HTMLButtonElement;
    option.focus();
    await fireEvent.keyDown(option, { key: 'Enter' });
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

  it('renders a side-by-side preview pane when an option carries preview', async () => {
    // Side-by-side layout switches on the moment any option in a
    // single-select question has non-empty `preview`. Multi-select
    // questions never trigger the side-by-side per upstream spec.
    const { getByTestId, queryByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'layout',
              header: 'Layout',
              question: 'Pick one',
              options: [
                { label: 'A', description: '', preview: 'first preview' },
                { label: 'B', description: '', preview: 'second preview' },
              ],
            },
          ],
        }),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve: vi.fn(),
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    expect(getByTestId('user-input-preview')).toBeTruthy();
    expect(queryByTestId('user-input-options')).toBeTruthy();
  });

  it('does not render the preview pane when no option has preview', async () => {
    const { queryByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request(),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve: vi.fn(),
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    expect(queryByTestId('user-input-preview')).toBeNull();
  });

  it('suppresses the preview pane on multi-select even when options carry preview', async () => {
    // Per the upstream tool spec, the side-by-side preview layout is
    // single-select only — multi-select questions ignore the
    // `preview` field. Otherwise checkbox-style toggling against a
    // single shared preview pane confuses "which option am I
    // previewing right now."
    const { queryByTestId, getByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'features',
              header: 'Features',
              question: 'Pick any',
              multiSelect: true,
              options: [
                { label: 'A', description: '', preview: 'first preview' },
                { label: 'B', description: '', preview: 'second preview' },
              ],
            },
          ],
        }),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve: vi.fn(),
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    // Options still render (single-column layout) — preview pane does not.
    expect(getByTestId('user-input-options')).toBeTruthy();
    expect(queryByTestId('user-input-preview')).toBeNull();
  });

  it('moves the focused option with ArrowDown / ArrowUp', async () => {
    // Arrow keys drive which option's preview is shown in the
    // side-by-side pane. Selection is unchanged — only focus moves —
    // so the user can review previews without committing to one.
    const { getByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'layout',
              header: 'Layout',
              question: 'Pick one',
              options: [
                { label: 'A', description: '', preview: 'first preview' },
                { label: 'B', description: '', preview: 'second preview' },
              ],
            },
          ],
        }),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve: vi.fn(),
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    expect(getByTestId('user-input-preview').textContent ?? '').toContain('first preview');
    await fireEvent.keyDown(window, { key: 'ArrowDown' });
    expect(getByTestId('user-input-preview').textContent ?? '').toContain('second preview');
    await fireEvent.keyDown(window, { key: 'ArrowUp' });
    expect(getByTestId('user-input-preview').textContent ?? '').toContain('first preview');
    // Clamps at top — does not wrap.
    await fireEvent.keyDown(window, { key: 'ArrowUp' });
    expect(getByTestId('user-input-preview').textContent ?? '').toContain('first preview');
  });

  it('moves DOM focus through options with j/k and down into the composer textarea', async () => {
    const root = document.createElement('div');
    root.setAttribute('data-testid', 'composer-root');
    const textarea = document.createElement('textarea');
    textarea.setAttribute('aria-label', 'Message Input');
    const target = document.createElement('div');
    root.appendChild(textarea);
    root.appendChild(target);
    document.body.appendChild(root);
    textarea.focus();

    const { getByTestId, unmount } = render(ComposerPendingUserInputPanel, {
      target,
      props: {
        request: request(),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve: vi.fn(),
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    const first = getByTestId('user-input-option-1') as HTMLButtonElement;
    const second = getByTestId('user-input-option-2') as HTMLButtonElement;
    await waitFor(() => expect(document.activeElement).toBe(first));

    await fireEvent.keyDown(first, { key: 'j' });
    expect(document.activeElement).toBe(second);

    await fireEvent.keyDown(second, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(textarea);

    second.focus();
    await fireEvent.keyDown(second, { key: 'k' });
    expect(document.activeElement).toBe(first);

    unmount();
    root.remove();
  });

  it('moves between questions from the header with h/l and arrow keys', async () => {
    const { getByTestId, getByText } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'framework',
              header: 'Framework',
              question: 'Pick one',
              options: [{ label: 'Svelte', description: '' }],
            },
            {
              id: 'scope',
              header: 'Scope',
              question: 'Pick scope',
              options: [{ label: 'This turn', description: '' }],
            },
          ],
        }),
        customAnswer: '',
        submitSignal: 0,
        setCustomAnswerText: vi.fn(),
        onResolve: vi.fn(),
        onResolved: vi.fn(),
        onError: vi.fn(),
      },
    });

    const header = getByTestId('user-input-question-header') as HTMLElement;
    header.focus();
    await fireEvent.keyDown(header, { key: 'l' });
    expect(getByText('Pick scope')).toBeTruthy();
    await fireEvent.keyDown(header, { key: 'ArrowLeft' });
    expect(getByText('Pick one')).toBeTruthy();
  });
});
