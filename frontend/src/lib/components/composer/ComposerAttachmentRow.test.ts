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
    kind: 'image',
  };
}

function makeFile(id: string, filename = `${id}.pdf`, size = 2048): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename,
    mimeType: 'application/pdf',
    size,
    relativePath: `thread-1/${id}/${filename}`,
    createdAt: 1,
    kind: 'file',
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
    expect(getByText(/Drop files/)).toBeInTheDocument();
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

  it('renders a file as a chip with its name and size, and no preview affordance', async () => {
    const thumbnail = setBindingMock('GetAttachmentThumbnail', async () => ({
      data: 'iVBORw0KGgo=',
      mimeType: 'image/png',
    }));
    const { getByTestId, getByText, queryByLabelText, queryByTestId } = render(ComposerAttachmentRow, {
      props: {
        attachments: [makeFile('f1', 'report.pdf', 2048)],
        onRemove: vi.fn(),
      },
    });

    const chip = getByTestId('attachment-file-chip');
    expect(chip).toHaveAttribute('title', 'report.pdf (2.0 KB)');
    expect(getByText('report.pdf')).toBeInTheDocument();
    expect(getByText('2.0 KB')).toBeInTheDocument();
    // Bytes are never served for a file, so nothing may ask for them, and
    // there is no `#N` badge because a file holds no `[Image #N]` slot.
    expect(queryByLabelText('Preview report.pdf')).toBeNull();
    expect(queryByTestId('attachment-thumb')).toBeNull();
    await waitFor(() => expect(thumbnail).not.toHaveBeenCalled());
  });

  it('removes a file through its chip', async () => {
    const onRemove = vi.fn();
    const { getByLabelText } = render(ComposerAttachmentRow, {
      props: { attachments: [makeFile('f1', 'report.pdf')], onRemove },
    });

    await fireEvent.click(getByLabelText('Remove report.pdf'));
    expect(onRemove).toHaveBeenCalledWith('f1');
  });

  it('numbers the badges over images, skipping the file between them', () => {
    const { getByLabelText } = render(ComposerAttachmentRow, {
      props: {
        attachments: [
          makeAttachment('a1', 'one.png'),
          makeFile('f1', 'report.pdf'),
          makeAttachment('a2', 'two.png'),
        ],
        onRemove: vi.fn(),
      },
    });

    expect(getByLabelText('Image 1')).toHaveTextContent('#1');
    expect(getByLabelText('Image 2')).toHaveTextContent('#2');
  });
});
