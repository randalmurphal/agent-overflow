import { describe, expect, it } from 'vitest';
import { fireEvent, render, within } from '@testing-library/svelte';
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

  it('renders the selected option from persisted meta.answers keyed by header (Claude)', async () => {
    // The fix: triage merges the resolved answers onto the AskUserQuestion
    // launch row as meta.answers. Claude questions carry no id, so the answer
    // is keyed by the question header. Without the fix the card shows
    // "No answer recorded." even though the agent received the choice.
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            header: 'Retention fix',
            question: 'Which approach?',
            options: [
              { label: 'SECURITY DEFINER fn', description: 'recommended' },
              { label: 'Minimal degrade-only', description: 'defer' },
            ],
          },
        ],
        directAnswers: { 'Retention fix': 'SECURITY DEFINER fn' },
      }),
    });

    const { getByTestId, getAllByTestId, queryByText } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    expect(queryByText('No answer recorded.')).toBeNull();
    const options = getAllByTestId('ask-user-question-option');
    expect(options[0].getAttribute('data-selected')).toBe('true'); // SECURITY DEFINER fn
    expect(options[1].getAttribute('data-selected')).toBe('false');
  });

  it('renders multiple selected options from persisted meta.answers (Claude multi-select)', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
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
        directAnswers: { Features: ['Markdown', 'Attachments'] },
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
  });

  it('renders matched options AND a custom value from one coexistence array (multi-select)', async () => {
    // Multi-select coexistence: the user picks options AND types a custom
    // answer, so the composer sends one combined array [opt, opt, custom].
    // mergeUserInputAnswersIntoLaunch persists it verbatim onto meta.answers,
    // and the card must split it back into checked options PLUS a Custom row —
    // not drop the custom value and not render it as a phantom option.
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
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
        directAnswers: { Features: ['Markdown', 'Attachments', 'Voice notes'] },
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

    const custom = getByTestId('ask-user-question-custom');
    expect(custom.textContent).toContain('Custom:');
    expect(custom.textContent).toContain('Voice notes');
  });

  it('prefers persisted meta.answers over a conflicting tool_result echo', async () => {
    // meta.answers is exactly what was sent to the agent; the tool_result
    // echo is free-form text that may parse to something stale or wrong.
    // The card must let the authoritative meta.answers win.
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            header: 'Retention fix',
            question: 'Which approach?',
            options: [
              { label: 'SECURITY DEFINER fn', description: 'recommended' },
              { label: 'Minimal degrade-only', description: 'defer' },
            ],
          },
        ],
        directAnswers: { 'Retention fix': 'SECURITY DEFINER fn' },
        toolResultContent: JSON.stringify({
          answers: { 'Retention fix': 'Minimal degrade-only' },
        }),
      }),
    });

    const { getByTestId, getAllByTestId } = render(AskUserQuestionCard, {
      props: { item },
    });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    const options = getAllByTestId('ask-user-question-option');
    expect(options[0].getAttribute('data-selected')).toBe('true'); // meta.answers wins
    expect(options[1].getAttribute('data-selected')).toBe('false'); // not the echo's choice
  });

  it('disambiguates duplicate headers by question id (Claude normalized ids)', async () => {
    // Two questions share the header "Scope". Triage persists the normalized
    // question list with deduped ids (Scope / Scope-2) and keys the answers by
    // those ids, so each question must resolve to its OWN answer. Without the
    // ids the card would fall back to header matching and render the first
    // answer ("turn") for both questions.
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'AskUserQuestion',
      meta: buildMeta({
        questions: [
          {
            id: 'Scope',
            header: 'Scope',
            question: 'Scope for fix A?',
            options: [
              { label: 'turn', description: '' },
              { label: 'session', description: '' },
            ],
          },
          {
            id: 'Scope-2',
            header: 'Scope',
            question: 'Scope for fix B?',
            options: [
              { label: 'turn', description: '' },
              { label: 'session', description: '' },
            ],
          },
        ],
        directAnswers: { Scope: 'turn', 'Scope-2': 'session' },
      }),
    });

    const { getByTestId } = render(AskUserQuestionCard, { props: { item } });

    await fireEvent.click(getByTestId('ask-user-question-toggle'));

    const first = within(getByTestId('ask-user-question-question-0')).getAllByTestId(
      'ask-user-question-option',
    );
    const second = within(getByTestId('ask-user-question-question-1')).getAllByTestId(
      'ask-user-question-option',
    );
    expect(first[0].getAttribute('data-selected')).toBe('true'); // Scope -> turn
    expect(first[1].getAttribute('data-selected')).toBe('false');
    expect(second[0].getAttribute('data-selected')).toBe('false'); // Scope-2 -> session, not turn
    expect(second[1].getAttribute('data-selected')).toBe('true');
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
