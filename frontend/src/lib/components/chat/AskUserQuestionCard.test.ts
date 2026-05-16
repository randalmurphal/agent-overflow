import { describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import AskUserQuestionCard from './AskUserQuestionCard.svelte';
import { makeItem } from '../../../test/helpers/chat';

/**
 * `meta` payloads in this file mirror what the parser writes:
 * - launch: `{toolName, input}` (parse_assistant.go marshalToolMeta)
 * - completion: `{is_error, tool_result, ...}` merged onto the launch
 *   meta by tool_lifecycle.go mergeItemMetaJSON.
 *
 * We construct them inline rather than re-export a helper because the
 * card has only a handful of test variants and inlining keeps each
 * assertion self-evident.
 */
function buildMeta(parts: {
  questions: unknown[];
  answers?: Record<string, unknown>;
  directAnswers?: Record<string, unknown>;
  toolResultContent?: unknown;
  toolName?: string;
}): string {
  const meta: Record<string, unknown> = {
    toolName: parts.toolName ?? 'AskUserQuestion',
    input: { questions: parts.questions },
  };
  if (parts.directAnswers) {
    meta.answers = parts.directAnswers;
  }
  if (parts.toolResultContent) {
    meta.tool_result = {
      content: parts.toolResultContent,
    };
    return JSON.stringify(meta);
  }
  if (parts.answers) {
    meta.tool_result = {
      content: JSON.stringify({ answers: parts.answers }),
    };
  }
  return JSON.stringify(meta);
}

describe('<AskUserQuestionCard>', () => {
  it('renders a single-question collapsed title with the question text', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'framework',
            header: 'Framework',
            question: 'Which framework do you want?',
            options: [
              { label: 'React', description: '' },
              { label: 'Svelte', description: '' },
            ],
          },
        ],
      }),
    });

    const { getByRole, getByTestId } = render(AskUserQuestionCard, { props: { item } });

    expect(getByTestId('ask-user-question-title').textContent).toContain(
      'Question: Which framework do you want?',
    );
    expect(getByRole('button', { name: /Which framework do you want\?/ })).toBeInTheDocument();
  });

  it('summarises multi-question prompts as "Question: N questions"', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          { id: 'a', header: 'A', question: 'First?', options: [] },
          { id: 'b', header: 'B', question: 'Second?', options: [] },
          { id: 'c', header: 'C', question: 'Third?', options: [] },
        ],
      }),
    });

    const { getByTestId } = render(AskUserQuestionCard, { props: { item } });

    expect(getByTestId('ask-user-question-title').textContent).toContain(
      'Question: 3 questions',
    );
  });

  it('shows the running indicator while the question is unanswered', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          { id: 'a', header: 'A', question: 'Pick?', options: [] },
        ],
      }),
    });

    const { getByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    expect(getByTestId('ask-user-question-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('running');
  });

  it('shows no success indicator once the user has answered', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'framework',
            header: 'Framework',
            question: 'Which framework?',
            options: [
              { label: 'React', description: '' },
              { label: 'Svelte', description: '' },
            ],
          },
        ],
        answers: { framework: 'Svelte' },
      }),
    });

    const { getByTestId, queryByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    expect(queryByTestId('ask-user-question-status')).toBeNull();
  });

  it('shows the failure indicator when the row was force-closed by interrupt', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'errored',
      toolName: 'AskUserQuestion',
      summary: 'Question: Pick? — turn ended with tool unresolved',
      meta: buildMeta({
        questions: [{ id: 'a', header: 'A', question: 'Pick?', options: [] }],
      }),
    });

    const { getByTestId } = render(AskUserQuestionCard, { props: { item } });

    expect(getByTestId('ask-user-question-status').querySelector('[data-testid="indicator"]')?.getAttribute('data-state')).toBe('error');
    expect(getByTestId('row-error')).toBeInTheDocument();
  });

  it('expands to show check/X marks per option', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'framework',
            header: 'Framework',
            question: 'Which framework?',
            options: [
              { label: 'React', description: 'A React app' },
              { label: 'Svelte', description: 'A Svelte app' },
              { label: 'Vue', description: 'A Vue app' },
            ],
          },
        ],
        answers: { framework: 'Svelte' },
      }),
    });

    const { getByTestId, getAllByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    const options = getAllByTestId('ask-user-question-option');
    expect(options).toHaveLength(3);
    // React, Svelte, Vue — only Svelte should be marked selected.
    expect(options[0].getAttribute('data-selected')).toBe('false');
    expect(options[1].getAttribute('data-selected')).toBe('true');
    expect(options[2].getAttribute('data-selected')).toBe('false');
  });

  it('renders a "Custom: <text>" row for free-typed answers', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'framework',
            header: 'Framework',
            question: 'Which framework?',
            options: [
              { label: 'React', description: '' },
              { label: 'Svelte', description: '' },
            ],
          },
        ],
        // User typed "Solid" instead of picking one of the predefined
        // options — the card surfaces it as a custom answer row.
        answers: { framework: 'Solid' },
      }),
    });

    const { getByTestId, getAllByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    // All predefined options stay marked unselected.
    for (const option of getAllByTestId('ask-user-question-option')) {
      expect(option.getAttribute('data-selected')).toBe('false');
    }

    const custom = getByTestId('ask-user-question-custom');
    expect(custom.textContent).toContain('Custom:');
    expect(custom.textContent).toContain('Solid');
  });

  it('renders multiple selected options for multi-select answers', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'features',
            header: 'Features',
            question: 'Which features?',
            multiSelect: true,
            options: [
              { label: 'Markdown', description: '' },
              { label: 'Code highlighting', description: '' },
              { label: 'Attachments', description: '' },
              { label: 'Slash commands', description: '' },
            ],
          },
        ],
        answers: { features: ['Markdown', 'Attachments'] },
      }),
    });

    const { getByTestId, getAllByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    const options = getAllByTestId('ask-user-question-option');
    expect(options[0].getAttribute('data-selected')).toBe('true'); // Markdown
    expect(options[1].getAttribute('data-selected')).toBe('false'); // Code highlighting
    expect(options[2].getAttribute('data-selected')).toBe('true'); // Attachments
    expect(options[3].getAttribute('data-selected')).toBe('false'); // Slash commands
  });

  it('parses Claude AskUserQuestion tool-result sentences keyed by question text', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'features',
            header: 'Features',
            question: 'Which features?',
            multiSelect: true,
            options: [
              { label: 'Markdown', description: '' },
              { label: 'Code highlighting', description: '' },
              { label: 'Attachments', description: '' },
            ],
          },
        ],
        toolResultContent:
          'User has answered your questions: "Which features?"="Markdown, Attachments". You can now continue with the task.',
      }),
    });

    const { getByTestId, getAllByTestId, queryByText } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    expect(queryByText('No answer recorded.')).toBeNull();
    const options = getAllByTestId('ask-user-question-option');
    expect(options[0].getAttribute('data-selected')).toBe('true');
    expect(options[1].getAttribute('data-selected')).toBe('false');
    expect(options[2].getAttribute('data-selected')).toBe('true');
  });

  it('keeps comma-containing option labels intact when parsing Claude multi-select answers', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'platforms',
            header: 'Platforms',
            question: 'Which platforms?',
            multiSelect: true,
            options: [
              { label: 'Web, mobile', description: '' },
              { label: 'API', description: '' },
              { label: 'Desktop', description: '' },
            ],
          },
        ],
        toolResultContent:
          'User has answered your questions: "Which platforms?"="Web, mobile, API". You can now continue with the task.',
      }),
    });

    const { getByTestId, getAllByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    const options = getAllByTestId('ask-user-question-option');
    expect(options[0].getAttribute('data-selected')).toBe('true');
    expect(options[1].getAttribute('data-selected')).toBe('true');
    expect(options[2].getAttribute('data-selected')).toBe('false');
  });

  it('reads Codex request_user_input answers from item metadata', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'request_user_input',
      meta: buildMeta({
        toolName: 'request_user_input',
        questions: [
          {
            id: 'scope',
            header: 'Scope',
            question: 'Choose a scope',
            options: [
              { label: 'turn', description: '' },
              { label: 'session', description: '' },
            ],
          },
        ],
        directAnswers: { scope: 'session' },
      }),
    });

    const { getByTestId, getAllByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    const options = getAllByTestId('ask-user-question-option');
    expect(options[0].getAttribute('data-selected')).toBe('false');
    expect(options[1].getAttribute('data-selected')).toBe('true');
  });

  it('renders one block per question for multi-question prompts', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'theme',
            header: 'Theme',
            question: 'Which theme?',
            options: [
              { label: 'Dark', description: '' },
              { label: 'Light', description: '' },
            ],
          },
          {
            id: 'lang',
            header: 'Language',
            question: 'Which language?',
            options: [
              { label: 'TypeScript', description: '' },
              { label: 'Python', description: '' },
            ],
          },
        ],
        answers: { theme: 'Dark', lang: 'TypeScript' },
      }),
    });

    const { getByTestId } = render(AskUserQuestionCard, { props: { item } });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    expect(getByTestId('ask-user-question-question-0')).toBeTruthy();
    expect(getByTestId('ask-user-question-question-1')).toBeTruthy();
    // Title summarises as "N questions".
    expect(getByTestId('ask-user-question-title').textContent).toContain(
      'Question: 2 questions',
    );
  });

  it('falls back gracefully when meta lacks the expected shape', () => {
    // Defensive coverage: a legacy persisted row, or a tool_use that
    // failed mid-parse, should still render a header even if no
    // question metadata is recoverable.
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: '{}',
    });

    const { getByTestId } = render(AskUserQuestionCard, { props: { item } });

    expect(getByTestId('ask-user-question-title').textContent).toContain('Question');
  });
});
