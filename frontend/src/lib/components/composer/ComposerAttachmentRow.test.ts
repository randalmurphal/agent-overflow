import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
import type { Attachment } from '../../types/attachment';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { mockAttachmentDownload } from '../../../test/mocks/attachmentTransfer';

function makeAttachment(id: string, filename = `${id}.png`, size = 512): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename,
    mimeType: 'image/png',
    size,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

describe('<ComposerAttachmentRow>', () => {
  beforeEach(() => {
    resetBindingMocks();
    // Inline grid loads thumbnails; lightbox modal reloads full-size on click.
    // Stub both so any test path produces a usable preview.
    setBindingMock('GetAttachmentThumbnail', async () => ({ data: 'iVBORw0KGgo=', mimeType: 'image/png' }));
    mockAttachmentDownload();
  });

  it('renders nothing when empty and not dragging', () => {
    const { container } = render(ComposerAttachmentRow, {
      props: { attachments: [], onRemove: vi.fn(), dragActive: false },
    });
    expect(container.textContent).toBe('');
  });

  it('shows drop hint when drag is active and no attachments', () => {
    const { getByText } = render(ComposerAttachmentRow, {
      props: { attachments: [], onRemove: vi.fn(), dragActive: true },
    });
    expect(getByText(/Drop an image/)).toBeInTheDocument();
  });

  it('renders each attachment as a thumbnail button', async () => {
    const { getByLabelText, getByText } = render(ComposerAttachmentRow, {
      props: {
        attachments: [makeAttachment('a1', 'hero.png', 2048)],
        onRemove: vi.fn(),
      },
    });
    const previewButton = getByLabelText('Preview hero.png');
    expect(previewButton).toBeInTheDocument();
    expect(getByText('#1')).toBeInTheDocument();
    await waitFor(() => {
      expect(previewButton.querySelector('img')).not.toBeNull();
    });
  });

  it('clicking remove invokes onRemove with the attachment id', async () => {
    const onRemove = vi.fn();
    const { getByLabelText } = render(ComposerAttachmentRow, {
      props: {
        attachments: [makeAttachment('a1', 'hero.png')],
        onRemove,
      },
    });
    await fireEvent.click(getByLabelText('Remove hero.png'));
    expect(onRemove).toHaveBeenCalledWith('a1');
  });
});
