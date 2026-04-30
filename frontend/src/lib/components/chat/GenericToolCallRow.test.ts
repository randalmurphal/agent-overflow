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
});
