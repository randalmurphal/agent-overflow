import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
import type { Attachment } from '../../types/attachment';

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

  it('renders each attachment chip with filename and size', () => {
    const { getByText } = render(ComposerAttachmentRow, {
      props: {
        attachments: [makeAttachment('a1', 'hero.png', 2048)],
        onRemove: vi.fn(),
      },
    });
    expect(getByText('hero.png')).toBeInTheDocument();
    expect(getByText('2.0 KB')).toBeInTheDocument();
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
