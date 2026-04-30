import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import GenericToolCallRow from './GenericToolCallRow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeItem } from '../../../test/helpers/chat';

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
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0]);

    // Body did NOT expand because stopPropagation prevented the
    // toggle's onclick from firing.
    expect(queryByTestId('tool-call-card-body')).toBeNull();
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

  it('suppresses the dropdown for TaskOutput rows even when a payload exists', () => {
    // TaskOutput retrieves the same stdout already shown on the
    // originating Bash row, so the row stays compact (no toggle button,
    // no expandable body).
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'TaskOutput',
      summary: 'TaskOutput',
      payloadId: 'p-task-output',
    });
    const { queryByTestId, getByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(queryByTestId('tool-call-card-toggle')).toBeNull();
    expect(getByTestId('tool-call-card-row')).not.toBeNull();
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });
});
