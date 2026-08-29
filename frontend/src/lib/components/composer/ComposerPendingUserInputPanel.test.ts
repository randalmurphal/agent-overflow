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

/** Text of the one visible preview layer inside the side-by-side cell. */
function activePreviewText(getByTestId: (id: string) => HTMLElement): string {
  const layer = getByTestId('user-input-preview')
    .querySelector('[data-user-input-preview][data-active="true"]');
  return layer?.textContent ?? '';
}

describe('<ComposerPendingUserInputPanel>', () => {
  // Option labels are model-authored, so nothing upstream promises they are
  // distinct. Keyed on the label, a repeat throws `each_key_duplicate` out of
  // the update flush, aborting the batch and freezing the pane this panel is
  // anchored in (incident 2026-08-29).
  it('renders every option when two of them carry the same label', () => {
    const { getByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'approach',
              header: 'Approach',
              question: 'Which approach?',
              options: [
                { label: 'Rewrite', description: 'From scratch' },
                { label: 'Rewrite', description: 'Keep the tests' },
                { label: 'Patch', description: 'Smallest diff' },
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

    expect(getByTestId('user-input-options').querySelectorAll('[data-user-input-option]'))
      .toHaveLength(3);
  });

  // The side-by-side branch keys the SAME list a second time, into the preview
  // cell — so it needs its own coverage, not the option column's.
  it('renders every preview layer when two options share a label', () => {
    const { getByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'approach',
              header: 'Approach',
              question: 'Which approach?',
              options: [
                { label: 'Rewrite', description: '', preview: 'first' },
                { label: 'Rewrite', description: '', preview: 'second' },
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

    expect(getByTestId('user-input-preview').querySelectorAll('[data-user-input-preview]'))
      .toHaveLength(2);
  });

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

  it('rewrites local file links inside option previews', async () => {
    const { getByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            {
              id: 'layout',
              header: 'Layout',
              question: 'Pick one',
              options: [
                {
                  label: 'A',
                  description: '',
                  preview: '[entity](file:///workspace/models/event.py#L42)',
                },
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
        workspacePath: '/workspace',
      },
    });

    await waitFor(() => {
      const anchor = getByTestId('user-input-preview').querySelector(
        'a[href^="agent-overflow:open"]',
      );
      expect(anchor).not.toBeNull();
      expect(anchor?.getAttribute('href')).toContain('line=42');
    });
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

    // Every option's preview is laid out (that is what keeps the cell's
    // height stable); only the active layer is visible, so assert on it
    // rather than on the cell's full textContent.
    expect(activePreviewText(getByTestId)).toContain('first preview');
    await fireEvent.keyDown(window, { key: 'ArrowDown' });
    expect(activePreviewText(getByTestId)).toContain('second preview');
    await fireEvent.keyDown(window, { key: 'ArrowUp' });
    expect(activePreviewText(getByTestId)).toContain('first preview');
    // Clamps at top — does not wrap.
    await fireEvent.keyDown(window, { key: 'ArrowUp' });
    expect(activePreviewText(getByTestId)).toContain('first preview');
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

  it('preserves the entered answer across collapse and expand', async () => {
    // Collapse is a visual minimize, not a cancel: the component stays
    // mounted while the body is hidden so a selection survives a
    // collapse/expand round-trip and can still be submitted.
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request(),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
      collapsed: false,
    };
    const { getByTestId, queryByTestId, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-option-2'));

    await rerender({ ...props, collapsed: true });
    expect(queryByTestId('composer-pending-user-input')).toBeNull();

    await rerender({ ...props, collapsed: false });
    await fireEvent.click(getByTestId('user-input-submit'));

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ framework: 'Svelte' });
  });

  it('submits selected options together with a typed answer for multi-select', async () => {
    // Multi-select coexistence: chosen options AND a typed entry submit
    // together as a single de-duplicated array.
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request({
        questions: [
          {
            id: 'features',
            header: 'Features',
            question: 'Pick any',
            multiSelect: true,
            options: [
              { label: 'A', description: '' },
              { label: 'B', description: '' },
              { label: 'C', description: '' },
            ],
          },
        ],
      }),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
    };
    const { getByTestId, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-option-1'));
    await fireEvent.click(getByTestId('user-input-option-2'));
    await rerender({ ...props, customAnswer: 'extra' });

    await fireEvent.click(getByTestId('user-input-submit'));

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ features: ['A', 'B', 'extra'] });
  });

  it('shows Next on a non-final question and Submit only on the last', async () => {
    // Navigation lives solely in the top-right toolbar: free-nav Next until
    // the final question, where it becomes the submit. The bottom button row
    // is gone.
    const { getByTestId, queryByTestId } = render(ComposerPendingUserInputPanel, {
      props: {
        request: request({
          questions: [
            { id: 'a', header: 'A', question: 'Q1', options: [{ label: 'x', description: '' }] },
            { id: 'b', header: 'B', question: 'Q2', options: [{ label: 'y', description: '' }] },
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

    expect(getByTestId('user-input-header-next')).toBeTruthy();
    expect(queryByTestId('user-input-submit')).toBeNull();

    // Free navigation to the final question — no answer required.
    await fireEvent.click(getByTestId('user-input-header-next'));

    expect(queryByTestId('user-input-header-next')).toBeNull();
    expect(getByTestId('user-input-submit')).toBeTruthy();
  });

  it('shows a single Submit answer button for a one-question request', async () => {
    const { getByTestId, queryByTestId } = render(ComposerPendingUserInputPanel, {
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

    expect(queryByTestId('user-input-header-previous')).toBeNull();
    expect(queryByTestId('user-input-header-next')).toBeNull();
    expect(getByTestId('user-input-submit').textContent).toContain('Submit answer');
  });

  it('preserves the navigated question index across collapse and expand', async () => {
    // The component stays mounted while collapsed (onMount does not re-run), so
    // the user-navigated index — not just the answers — survives the round-trip.
    const props = {
      request: request({
        questions: [
          { id: 'a', header: 'A', question: 'First question', options: [{ label: 'x', description: '' }] },
          { id: 'b', header: 'B', question: 'Second question', options: [{ label: 'y', description: '' }] },
        ],
      }),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve: vi.fn(),
      onResolved: vi.fn(),
      onError: vi.fn(),
      collapsed: false,
    };
    const { getByTestId, getByText, queryByTestId, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-header-next'));
    expect(getByText('Second question')).toBeTruthy();
    await fireEvent.click(getByTestId('user-input-option-1'));

    await rerender({ ...props, collapsed: true });
    expect(queryByTestId('composer-pending-user-input')).toBeNull();

    await rerender({ ...props, collapsed: false });
    expect(getByText('Second question')).toBeTruthy();
  });

  it('keeps the typed answer when a multi-select option is toggled off', async () => {
    // Exercises selectOption's multi-select branch (does not clear the custom
    // box) plus toggleOptionAnswer preserving the typed entry on toggle-off.
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request({
        questions: [
          {
            id: 'features',
            header: 'Features',
            question: 'Pick any',
            multiSelect: true,
            options: [
              { label: 'A', description: '' },
              { label: 'B', description: '' },
            ],
          },
        ],
      }),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
    };
    const { getByTestId, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-option-1')); // select A
    await rerender({ ...props, customAnswer: 'typed' }); // type a custom entry
    await fireEvent.click(getByTestId('user-input-option-1')); // toggle A back off

    await fireEvent.click(getByTestId('user-input-submit'));

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ features: ['typed'] });
  });

  it('does not advance or submit a hidden multi-question request on collapsed Enter', async () => {
    // While collapsed, the composer-Enter path must not advance to a question
    // the user cannot see, and must not submit an incomplete request.
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request({
        questions: [
          { id: 'a', header: 'A', question: 'Q1', options: [{ label: 'x', description: '' }] },
          { id: 'b', header: 'B', question: 'Q2', options: [{ label: 'y', description: '' }] },
        ],
      }),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
      collapsed: false,
    };
    const { getByTestId, getByText, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-option-1')); // answer Q1 only
    await rerender({ ...props, collapsed: true });
    await rerender({ ...props, collapsed: true, submitSignal: 1 });

    expect(onResolve).not.toHaveBeenCalled();

    await rerender({ ...props, collapsed: false, submitSignal: 1 });
    expect(getByText('Q1')).toBeTruthy(); // still on Q1 — no hidden advance
  });

  it('submits via composer Enter while collapsed once answers are complete', async () => {
    const onResolve = vi.fn<(response: UserInputResponse) => Promise<void>>(async () => {});
    const props = {
      request: request(),
      customAnswer: '',
      submitSignal: 0,
      setCustomAnswerText: vi.fn(),
      onResolve,
      onResolved: vi.fn(),
      onError: vi.fn(),
      collapsed: false,
    };
    const { getByTestId, rerender } = render(ComposerPendingUserInputPanel, { props });

    await fireEvent.click(getByTestId('user-input-option-2')); // complete (single question)
    await rerender({ ...props, collapsed: true });
    await rerender({ ...props, collapsed: true, submitSignal: 1 });

    expect(onResolve).toHaveBeenCalledTimes(1);
    expect(onResolve.mock.calls[0][0].answers).toEqual({ framework: 'Svelte' });
  });
});
