import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { mockAttachmentUpload } from '../../../test/mocks/attachmentTransfer';
import type { Attachment } from '../../types/attachment';
import { createComposerUploads, type UploadInsertionPoint } from './composerUploads.svelte';
import { compressImageToFit } from './imageCompress';

// The canvas pipeline needs a real browser; the upload flow's use of it
// (attempt on over-limit images, fall back to the size rejection) is
// what these tests pin down.
vi.mock('./imageCompress', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./imageCompress')>()),
  compressImageToFit: vi.fn(),
}));

function attachment(id: string, filename = `${id}.png`): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename,
    mimeType: 'image/png',
    size: 1,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
    kind: 'image',
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
    vi.mocked(compressImageToFit).mockReset();
  });

  it('does not spend an attachment slot on rejected files', async () => {
    const addAttachment = vi.fn();
    const upload = mockAttachmentUpload(async (
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
    mockAttachmentUpload(async (
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

  it('compresses an over-limit image and uploads the re-encoded file', async () => {
    const oversized = new File(['big'], 'huge.png', { type: 'image/png' });
    Object.defineProperty(oversized, 'size', { value: 20 * 1024 * 1024 });
    const compressed = new File(['small'], 'huge.webp', { type: 'image/webp' });
    vi.mocked(compressImageToFit).mockResolvedValue(compressed);
    const upload = mockAttachmentUpload(async (
      _threadId: string,
      filename: string,
    ) => attachment(`att-${filename}`, filename));
    const uploads = createComposerUploads({
      getThreadId: () => 'thread-1',
      addAttachment: vi.fn(),
      removeAttachment: vi.fn(),
    });

    await uploads.handlePaste(makeClipboardPaste([oversized]));

    expect(compressImageToFit).toHaveBeenCalledWith(oversized, 10 * 1024 * 1024);
    expect(upload).toHaveBeenCalledTimes(1);
    expect(upload.mock.calls[0]?.[1]).toBe('huge.webp');
    expect(upload.mock.calls[0]?.[2]).toBe('image/webp');
  });

  it('falls back to the size rejection when compression cannot fit the image', async () => {
    const oversized = new File(['big'], 'huge.png', { type: 'image/png' });
    Object.defineProperty(oversized, 'size', { value: 20 * 1024 * 1024 });
    vi.mocked(compressImageToFit).mockResolvedValue(null);
    const upload = mockAttachmentUpload(async () => attachment('att-x'));
    const uploads = createComposerUploads({
      getThreadId: () => 'thread-1',
      addAttachment: vi.fn(),
      removeAttachment: vi.fn(),
    });

    await uploads.handlePaste(makeClipboardPaste([oversized]));

    expect(upload).not.toHaveBeenCalled();
  });

  // An upload that finished after the composer moved threads backs
  // nothing: no message, no draft and no later pass knows the id, so the
  // row and its bytes on disk are a leak the user cannot see or reach.
  it('deletes the record when the composer moved threads mid-upload', async () => {
    const deleted: Array<[string, string]> = [];
    setBindingMock('DeleteAttachment', async (threadId: string, id: string) => {
      deleted.push([threadId, id]);
    });
    const addAttachment = vi.fn();
    let current = 'thread-1';
    mockAttachmentUpload(async (_threadId: string, filename: string) => {
      // The switch happens while the bytes are in flight, which is the
      // whole case: the guard below is what sees it.
      current = 'thread-2';
      return attachment(`att-${filename}`, filename);
    });
    const uploads = createComposerUploads({
      getThreadId: () => current,
      addAttachment,
      removeAttachment: vi.fn(),
    });

    await uploads.handlePaste(makeClipboardPaste([
      new File(['image'], 'moved.png', { type: 'image/png' }),
    ]));
    // The delete is fire-and-forget, so let its microtask run.
    await Promise.resolve();

    expect(addAttachment).not.toHaveBeenCalled();
    expect(deleted).toEqual([['thread-1', 'att-moved.png']]);
  });

  it('does not attempt compression for images already within the limit', async () => {
    vi.mocked(compressImageToFit).mockResolvedValue(null);
    mockAttachmentUpload(async (
      _threadId: string,
      filename: string,
    ) => attachment(`att-${filename}`, filename));
    const uploads = createComposerUploads({
      getThreadId: () => 'thread-1',
      addAttachment: vi.fn(),
      removeAttachment: vi.fn(),
    });

    await uploads.handlePaste(makeClipboardPaste([
      new File(['tiny'], 'tiny.png', { type: 'image/png' }),
    ]));

    expect(compressImageToFit).not.toHaveBeenCalled();
  });
});
