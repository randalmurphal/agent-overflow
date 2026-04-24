import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Attachment } from '../../types/attachment';
import { createComposerUploads, type UploadInsertionPoint } from './composerUploads.svelte';

function attachment(id: string, filename = `${id}.png`): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename,
    mimeType: 'image/png',
    size: 1,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

function makeClipboardPaste(files: File[]): ClipboardEvent {
  const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(event, 'clipboardData', {
    value: {
      items: files.map((file) => ({
        kind: 'file',
        type: file.type,
        getAsFile: () => file,
      })),
    },
  });
  return event;
}

describe('createComposerUploads', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('does not spend an attachment slot on rejected files', async () => {
    const addAttachment = vi.fn();
    const upload = setBindingMock('UploadAttachment', async (
      _threadId: string,
      filename: string,
    ) => attachment(`att-${filename}`, filename));
    const uploads = createComposerUploads({
      getThreadId: () => 'thread-1',
      addAttachment,
      removeAttachment: vi.fn(),
      getAttachmentCount: () => 0,
      maxAttachments: 1,
    });

    await uploads.handlePaste(makeClipboardPaste([
      new File(['not-image'], 'first.txt', { type: 'text/plain' }),
      new File(['image'], 'second.png', { type: 'image/png' }),
    ]));

    expect(upload).toHaveBeenCalledTimes(1);
    expect(upload.mock.calls[0]?.[1]).toBe('second.png');
    expect(addAttachment).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'att-second.png' }),
      null,
    );
  });

  it('keeps paste/drop insertion points scoped to each upload batch', async () => {
    const addAttachment = vi.fn();
    setBindingMock('UploadAttachment', async (
      _threadId: string,
      filename: string,
    ) => attachment(`att-${filename}`, filename));
    const uploads = createComposerUploads({
      getThreadId: () => 'thread-1',
      addAttachment,
      removeAttachment: vi.fn(),
    });
    const firstInsertion: UploadInsertionPoint = { start: 1, end: 1 };
    const secondInsertion: UploadInsertionPoint = { start: 5, end: 5 };

    await uploads.handlePaste(makeClipboardPaste([
      new File(['one'], 'one.png', { type: 'image/png' }),
    ]), firstInsertion);
    await uploads.handlePaste(makeClipboardPaste([
      new File(['two'], 'two.png', { type: 'image/png' }),
    ]), secondInsertion);

    expect(addAttachment.mock.calls[0]?.[1]).toBe(firstInsertion);
    expect(addAttachment.mock.calls[1]?.[1]).toBe(secondInsertion);
  });
});
