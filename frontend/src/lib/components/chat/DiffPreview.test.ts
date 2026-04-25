import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DiffPreview from './DiffPreview.svelte';
import { setBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import type { DiffMeta, Item } from '../../types/models';

const META: DiffMeta = {
  filePath: 'src/lib/foo.ts',
  changeKind: 'modified',
  insertions: 3,
  deletions: 1,
  preview: '',
};

const ITEM: Item = {
  id: 'item-1',
  threadId: 't1',
  turnIndex: 0,
  itemIndex: 0,
  kind: 'tool_call',
  status: 'completed',
  summary: '',
  createdAt: 0,
  updatedAt: 0,
} as unknown as Item;

describe('<DiffPreview> editor-link wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    // Avoid the payload-expansion path firing real RPCs in this test —
    // we only exercise the header.
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
    setBindingMock('GetPayloadChunk', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders the toggle button + the editor-link icon as siblings', () => {
    const { getByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    const link = getByTestId('editor-link-icon');
    expect(toggle.tagName).toBe('BUTTON');
    expect(link.tagName).toBe('BUTTON');
    expect(toggle.contains(link)).toBe(false);
  });

  it('clicking the toggle toggles the diff (parent toggle still fires)', async () => {
    const { getByTestId, container } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(toggle);
    // expansion is async (preview loads), so wait for the aria-expanded
    // flip rather than asserting synchronously.
    await waitFor(() => {
      expect(toggle.getAttribute('aria-expanded')).toBe('true');
    });
    void container; // narrow tooling silence: container intentionally unused
  });

  it('clicking the editor-link does NOT toggle the diff', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(DiffPreview, {
      props: { meta: META, payloadId: 'p-1', item: ITEM },
    });
    const toggle = getByTestId('diff-preview-toggle');
    const link = getByTestId('editor-link-icon');

    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(link);

    // OpenInEditor was invoked with the file path.
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 0, 0]);

    // The parent's toggle did NOT fire — diff stays collapsed.
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });
});
