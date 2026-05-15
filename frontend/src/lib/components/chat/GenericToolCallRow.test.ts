import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
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
    expect(queryByTestId('editor-link-icon')).toBeNull();
  });

  it('renders an editor-link sibling when the preview leads with a path', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    const link = getByTestId('editor-link-icon');
    expect(link.getAttribute('data-path')).toBe('src/lib/foo.ts');
    expect(getByTestId('tool-call-card-toggle')).toHaveAccessibleName(/src\/lib\/foo\.ts:12/);
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
    await fireEvent.click(getByTestId('editor-link-icon'));

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '']);
  });

  it('clicking the editor-link does NOT toggle the row body', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    // Provide a payloadId so the row renders as expandable.
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
      payloadId: 'p-1',
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });

    expect(queryByTestId('tool-call-card-body')).toBeNull();
    const link = getByTestId('editor-link-icon');
    await fireEvent.click(link);

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '']);

    // Body did NOT expand because stopPropagation prevented the
    // toggle's onclick from firing.
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
    await fireEvent.click(getByTestId('editor-link-icon'));
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '/home/user/repo']);
  });

  it('places the completion badge before the timestamp', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Read',
      summary: 'README.md',
    });

    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    expectBefore(getByTestId('completion-badge'), getByTestId('tool-call-card-time'));
  });

  it('renders the backgrounded "…" badge larger than the inline running label', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Bash',
      summary: 'long-running task',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    const status = getByTestId('tool-call-card-status');
    expect(status.textContent?.trim()).toBe('…');
    expect(status.getAttribute('aria-label')).toBe('Backgrounded');
    // The backgrounded variant uses a much larger font so the affordance
    // is visible without hover. The inline running label uses 10px.
    expect(status.className).toContain('text-[20px]');
    expect(status.className).not.toContain('text-[10px]');
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
    const { getByTestId, getAllByTestId, queryByText } = render(GenericToolCallRow, {
      props: { item },
    });

    await fireEvent.click(getByTestId('tool-call-card-toggle'));

    await waitFor(() => {
      expect(getByTestId('claude-subagent-transcript')).toBeInTheDocument();
    });
    expect(getAllByTestId('claude-subagent-transcript-entry')).toHaveLength(2);
    expect(queryByText(/"isSidechain"/)).toBeNull();
    expect(queryByText('echo done')).toBeInTheDocument();
    expect(queryByText('done')).toBeInTheDocument();
  });
});
