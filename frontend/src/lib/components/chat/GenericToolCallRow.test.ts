import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import AgentRow from './AgentRow.svelte';
import GenericToolCallRow from './GenericToolCallRow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem } from '../../../test/helpers/chat';
import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
import type { Item } from '../../types/models';

// Minimal fake pane that satisfies the expansion-registry surface
// GenericToolCallRow reads from. Shared between tests that need a pane
// reference (workspacePath, expansion state).
function makeFakePane(extra: Partial<import('../../stores/thread.svelte').ThreadPane> = {}): import('../../stores/thread.svelte').ThreadPane {
  const cache = new Map<string, ReturnType<typeof createPayloadExpansion>>();
  return {
    expansionStateFor(item: Item) {
      const key = item.id;
      let h = cache.get(key);
      if (!h) {
        h = createPayloadExpansion(() => item.payloadId, () => item.threadId);
        cache.set(key, h);
      }
      return h;
    },
    ...extra,
  } as unknown as import('../../stores/thread.svelte').ThreadPane;
}

function expectBefore(left: Element, right: Element) {
  expect(left.compareDocumentPosition(right) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
}

describe('<GenericToolCallRow> editor-link wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders no editor-link when the input preview does not lead with a path', () => {
    const item = makeItem({ kind: 'tool_call', summary: 'Waiting on agents' });
    const { queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(queryByTestId('editor-link')).toBeNull();
    expect(queryByTestId('editor-link-icon')).toBeNull();
  });

  it('renders the path preview itself as the editor link when the preview leads with a path', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    const link = getByTestId('editor-link');
    // Click target stays workspace-relative so OpenInEditor can resolve
    // it; displayed accessible name collapses to the basename.
    expect(link.getAttribute('data-path')).toBe('src/lib/foo.ts');
    expect(link.textContent).toBe('foo.ts:12');
    expect(link.getAttribute('title')).toBe('Open foo.ts:12 in editor');
    expect(queryByTestId('editor-link-icon')).toBeNull();
  });

  it('shows read file basenames without clipping the visible label or editor tooltip', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'Read: frontend/src/lib/components/chat/WaitGroup.svelte',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    const preview = getByTestId('tool-call-card-preview');
    const link = getByTestId('editor-link');
    expect(preview.textContent).toBe('WaitGroup.svelte');
    expect(preview.className).not.toContain('truncate');
    expect(preview.className).toContain('break-all');
    expect(link.getAttribute('data-path')).toBe('frontend/src/lib/components/chat/WaitGroup.svelte');
    expect(link.getAttribute('title')).toBe('Open WaitGroup.svelte in editor');
    expect(link.getAttribute('aria-label')).toBe('Open WaitGroup.svelte in editor');
  });

  it('keeps the editor-link usable on a non-expandable row', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    expect(getByTestId('tool-call-card-toggle')).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(getByTestId('editor-link'));

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '']);
  });

  it('clicking the editor-link does NOT toggle the row body', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
      payloadId: 'p-1',
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });

    expect(queryByTestId('tool-call-card-body')).toBeNull();
    const link = getByTestId('editor-link');
    await fireEvent.click(link);

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '']);

    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('suppresses the dropdown for Read rows even when a payload exists', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Read',
      summary: 'src/lib/foo.ts',
      payloadId: 'p-read',
    });
    const { queryByTestId, getByTestId } = render(GenericToolCallRow, { props: { item } });
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).toHaveAttribute('tabindex', '-1');
    await fireEvent.click(toggle);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('renders Skill rows with the skill name as preview and a disabled chevron', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Skill',
      summary: 'Skill',
      payloadId: 'p-skill',
      meta: JSON.stringify({ toolName: 'Skill', input: { skill: 'code-review' } }),
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-label').textContent).toBe('skill');
    expect(getByTestId('tool-call-card-preview').textContent).toBe('code-review');
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(toggle);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  // Regression for the original click-to-open bug: tool-result paths
  // are usually agent-emitted relative paths, which the backend used
  // to reject. The row threads `pane.thread.workspacePath` through to
  // EditorLink so the backend can join. Pin that the prop chain
  // forwards the value so a future refactor that drops the prop
  // wiring fails fast.
  it('forwards pane.thread.workspacePath to the OpenInEditor binding', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const pane = makeFakePane({
      thread: { workspacePath: '/home/user/repo' },
    } as Partial<import('../../stores/thread.svelte').ThreadPane>);
    const { getByTestId } = render(GenericToolCallRow, { props: { pane, item } });
    await fireEvent.click(getByTestId('editor-link'));
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '/home/user/repo']);
  });

  it('places the empty indicator slot before the timestamp', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Read',
      summary: 'README.md',
    });

    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    expectBefore(getByTestId('tool-call-card-status-slot'), getByTestId('tool-call-card-time'));
  });

  it('renders the backgrounded indicator state', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Bash',
      summary: 'long-running task',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    const status = getByTestId('tool-call-card-status');
    const indicator = status.querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute('data-state')).toBe('backgrounded');
    expect(indicator?.getAttribute('aria-label')).toBe('Backgrounded');
  });

  it('suppresses the live duration ticker on a backgrounded launch row so chat history shows only a timestamp', () => {
    // Backgrounded launches in chat are stable transcript records.
    // A wall-clock ticker that keeps counting up indefinitely is
    // misleading there — the user can't act on it from a chat row.
    // Live status lives in the tray (which passes its own
    // durationLabel; that path is exercised by BackgroundTaskTrayRow).
    // The eventual completion lands as its own sibling row with its
    // own duration. Use a generous 60s offset so the assertion stays
    // unambiguously past the 2s `RUNNING_ELAPSED_THRESHOLD_MS` gate
    // — a future bump of that threshold won't silently re-mean this
    // test as "ticker is empty in the gate window."
    const sixtySecondsAgo = Date.now() - 60_000;
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Bash',
      summary: 'long-running task',
      createdAt: sixtySecondsAgo,
      updatedAt: sixtySecondsAgo,
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    // Duration slot is reserved (always present in the DOM for layout
    // stability — see ToolHeaderMeta) but the label is empty because
    // shouldTickElapsed is gated off for backgrounded launches.
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).toBe('');
    // Timestamp still renders so the row is anchored in time.
    expect(getByTestId('tool-call-card-time')).toBeInTheDocument();
  });

  it('ticks the live duration on a non-backgrounded running tool (contrast for the suppression above)', () => {
    // Same wall-clock as the backgrounded test, no isBackground flag.
    // This locks in that the timer suppression is gated by
    // isBackgroundedLaunch and not an accidental wider regression.
    const sixtySecondsAgo = Date.now() - 60_000;
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'foreground bash',
      createdAt: sixtySecondsAgo,
      updatedAt: sixtySecondsAgo,
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).not.toBe('');
  });

  it('renders a Claude Agent row with the title-cased subagent_type label and the model affix', () => {
    // Without this header treatment a backgrounded Agent renders the
    // bare "Subagent" classifier label with no model — completely
    // different from the inline `SubagentGroup` card a foreground
    // Agent gets. Matching what `SubagentGroup` does here is what
    // keeps the two surfaces visually aligned.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: Review: security',
      meta: JSON.stringify({ subagent_model: 'claude-opus-4-7' }),
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'Review: security' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    // Preview shows just the description, not the "Agent: …" summary
    // line the generic preview formatter would otherwise emit.
    const preview = getByTestId('agent-row-preview');
    expect(getByTestId('agent-row-label').textContent).toBe('agent');
    expect(preview.textContent).toContain('Explore');
    expect(preview.textContent).toContain('Opus 4.7');
    expect(preview.textContent).toContain('Review: security');
    expect(preview.textContent).not.toContain('Agent:');
  });

  it('falls back to the launch input.model when no subagent_model is stamped on a backgrounded Agent yet', () => {
    // Brief window between launch and the subagent's first assistant
    // envelope — the parser hasn't stamped parent.meta.subagent_model
    // yet, but the user-supplied input.model alias should still drive
    // the affix so the row never renders without a model.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: launching',
      // No meta.subagent_model.
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'Just launched', model: 'opus' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    expect(getByTestId('agent-row-preview').textContent).toContain('Opus');
  });

  it('treats legacy Task rows as Claude Agent rows for label and model derivation', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Task',
      summary: 'Task: launching',
      payloadMeta: JSON.stringify({
        toolName: 'Task',
        input: { subagent_type: 'Explore', description: 'Legacy task launch', model: 'opus' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    const preview = getByTestId('agent-row-preview').textContent ?? '';
    expect(preview).toContain('Explore');
    expect(preview).toContain('Opus');
    expect(preview).toContain('Legacy task launch');
  });

  it('falls back to "Agent" (not "Subagent") when subagent_type is missing on an Agent row', () => {
    // Without the `.not.toContain('Subagent')` clause, this assertion
    // would also pass if the row regressed to the classifier label
    // "Subagent" (which itself contains "Agent"). Pin both directions.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: do something',
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { description: 'do something' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    const preview = getByTestId('agent-row-preview');
    expect(preview.textContent).toContain('Agent');
    expect(preview.textContent).not.toContain('Subagent');
  });

  it('falls through to the generic preview when an Agent row has no description or prompt', () => {
    // The `isClaudeAgent && subagentDescription` guard on inputPreview
    // must NOT swallow the row when description+prompt are both empty
    // — otherwise the preview would render as ''. Without this test a
    // future regression that drops the `&& subagentDescription` guard
    // would silently blank the preview.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: fallback preview',
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    // Label still renders as "Explore" (the subagent_type), and the
    // preview falls through to presentToolCardInputPreview, which
    // strips the redundant `Agent: ` prefix that triage embeds in
    // item.summary (the gutter label already says "agent").
    const previewText = getByTestId('agent-row-preview').textContent ?? '';
    expect(previewText).toContain('Explore');
    expect(previewText).toContain('fallback preview');
    expect(previewText).not.toContain('Agent: fallback preview');
  });

  it('suppresses the dropdown for TaskOutput rows even when a payload exists', async () => {
    // TaskOutput retrieves the same stdout already shown on the
    // originating Bash row, so the row keeps the stable header shell but
    // has no expandable body.
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'TaskOutput',
      summary: 'TaskOutput',
      payloadId: 'p-task-output',
    });
    const { queryByTestId, getByTestId } = render(GenericToolCallRow, { props: { item } });
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).toHaveAttribute('tabindex', '-1');
    await fireEvent.click(toggle);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('renders Claude Agent JSONL payloads as transcript entries', async () => {
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({
      data: [
        JSON.stringify({
          isSidechain: true,
          agentId: 'agent-1',
          type: 'assistant',
          message: {
            role: 'assistant',
            content: [
              {
                type: 'tool_use',
                id: 'tool-1',
                name: 'Bash',
                input: { command: 'echo done' },
              },
            ],
          },
        }),
        JSON.stringify({
          isSidechain: true,
          agentId: 'agent-1',
          type: 'user',
          message: {
            role: 'user',
            content: [
              {
                type: 'tool_result',
                tool_use_id: 'tool-1',
                content: 'done',
                is_error: false,
              },
            ],
          },
        }),
      ].join('\n'),
      totalSize: 400,
      nextOffset: 400,
      isComplete: true,
    })));
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Agent',
      summary: 'Agent: worker -> done',
      payloadId: 'agent-jsonl',
    });
    const { getByTestId, getAllByTestId, queryByText } = render(AgentRow, {
      props: { item },
    });

    await fireEvent.click(getByTestId('agent-row-toggle'));

    await waitFor(() => {
      expect(getByTestId('claude-subagent-transcript')).toBeInTheDocument();
    });
    expect(getAllByTestId('claude-subagent-transcript-entry')).toHaveLength(2);
    expect(queryByText(/"isSidechain"/)).toBeNull();
    expect(queryByText('echo done')).toBeInTheDocument();
    expect(queryByText('done')).toBeInTheDocument();
  });
});
