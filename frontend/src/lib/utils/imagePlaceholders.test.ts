import { describe, expect, it } from 'vitest';
import type { Attachment } from '../types/attachment';
import {
  ensureImagePlaceholders,
  findImagePlaceholderRanges,
  imagePlaceholderLabel,
  insertImagePlaceholder,
  reconcileImagePlaceholders,
  removeImagePlaceholderByAttachmentId,
  removeImagePlaceholderForKey,
} from './imagePlaceholders';

function attachment(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.png`,
    mimeType: 'image/png',
    size: 100,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
    kind: 'image',
  };
}

function file(id: string): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename: `${id}.pdf`,
    mimeType: 'application/pdf',
    size: 100,
    relativePath: `thread-1/${id}/${id}.pdf`,
    createdAt: 1,
    kind: 'file',
  };
}

describe('imagePlaceholders', () => {
  it('labels images with Codex-style placeholders', () => {
    expect(imagePlaceholderLabel(1)).toBe('[Image #1]');
    expect(imagePlaceholderLabel(12)).toBe('[Image #12]');
  });

  it('inserts a placeholder at the selected range with readable spacing', () => {
    const inserted = insertImagePlaceholder('see this now', '[Image #1]', 4, 8);
    expect(inserted.content).toBe('see [Image #1] now');
    expect(inserted.cursor).toBe('see [Image #1]'.length);
  });

  it('removes a placeholder atomically when backspacing from its right edge', () => {
    const result = removeImagePlaceholderForKey(
      'before [Image #1] after',
      [attachment('att-1')],
      'before [Image #1]'.length,
      'before [Image #1]'.length,
      'Backspace',
    );

    expect(result?.content).toBe('before after');
    expect(result?.attachmentIds).toEqual(['att-1']);
  });

  it('removes a placeholder atomically when deleting from its left edge', () => {
    const result = removeImagePlaceholderForKey(
      'before [Image #1] after',
      [attachment('att-1')],
      'before '.length,
      'before '.length,
      'Delete',
    );

    expect(result?.content).toBe('before after');
    expect(result?.attachmentIds).toEqual(['att-1']);
  });

  it('removes a placeholder when the cursor is inside it', () => {
    const cursor = 'before [Image'.length;
    const result = removeImagePlaceholderForKey(
      'before [Image #1] after',
      [attachment('att-1')],
      cursor,
      cursor,
      'Backspace',
    );

    expect(result?.content).toBe('before after');
  });

  it('removes every placeholder touched by a selection', () => {
    const attachments = [attachment('att-1'), attachment('att-2')];
    const content = 'a [Image #1] b [Image #2] c';
    const result = removeImagePlaceholderForKey(
      content,
      attachments,
      'a [Image'.length,
      'a [Image #1] b [Image'.length,
      'Backspace',
    );

    expect(result?.content).toBe('a  c');
    expect(result?.attachmentIds).toEqual(['att-1', 'att-2']);
  });

  it('removes by attachment id and renumbers the remaining placeholders', () => {
    const attachments = [attachment('att-1'), attachment('att-2'), attachment('att-3')];
    const result = removeImagePlaceholderByAttachmentId(
      '[Image #1] [Image #2] [Image #3]',
      attachments,
      'att-2',
    );

    expect(result?.content).toBe('[Image #1] [Image #2]');
    expect(result?.attachmentIds).toEqual(['att-2']);
  });

  it('renumbers only managed placeholders and leaves duplicate literal text alone', () => {
    const attachments = [attachment('att-1'), attachment('att-2')];
    const result = removeImagePlaceholderByAttachmentId(
      '[Image #1] literal [Image #2] and another literal [Image #2]',
      attachments,
      'att-1',
    );

    expect(result?.content).toBe('literal [Image #1] and another literal [Image #2]');
  });

  it('reconciles attachments against surviving exact placeholders', () => {
    const attachments = [attachment('att-1'), attachment('att-2')];
    const result = reconcileImagePlaceholders('[Image #2]', attachments);

    expect(result.attachments.map((item) => item.id)).toEqual(['att-2']);
    expect(result.content).toBe('[Image #1]');
    expect(result.removedAttachmentIds).toEqual(['att-1']);
  });

  it('adds missing placeholders for persisted attachment-only drafts', () => {
    expect(ensureImagePlaceholders('Look here', [attachment('att-1')]))
      .toBe('Look here [Image #1]');
  });

  // A file occupies an attachment slot but no text slot, so every number and
  // every match has to run over the IMAGE subset — otherwise `[Image #2]`
  // means the second attachment, which is the file.
  describe('a draft mixing images and files', () => {
    const mixed = () => [attachment('img-1'), file('doc-1'), attachment('img-2')];

    it('numbers images only', () => {
      const ranges = findImagePlaceholderRanges('[Image #1] [Image #2]', mixed());
      expect(ranges.map((range) => range.attachmentId)).toEqual(['img-1', 'img-2']);
      expect(ranges.map((range) => range.label)).toEqual(['[Image #1]', '[Image #2]']);
    });

    it('appends a marker for each image and none for the file', () => {
      expect(ensureImagePlaceholders('Look here', mixed()))
        .toBe('Look here [Image #1] [Image #2]');
    });

    it('deleting the [Image #2] text drops the second IMAGE and keeps the file', () => {
      const result = reconcileImagePlaceholders('[Image #1]', mixed());

      expect(result.removedAttachmentIds).toEqual(['img-2']);
      expect(result.attachments.map((item) => item.id)).toEqual(['img-1', 'doc-1']);
    });

    it('never drops a file, even when every marker is gone', () => {
      const result = reconcileImagePlaceholders('', mixed());

      expect(result.removedAttachmentIds).toEqual(['img-1', 'img-2']);
      expect(result.attachments.map((item) => item.id)).toEqual(['doc-1']);
    });

    it('removing the first image renumbers the second down to #1', () => {
      const result = removeImagePlaceholderByAttachmentId(
        '[Image #1] [Image #2]',
        mixed(),
        'img-1',
      );

      expect(result?.content).toBe('[Image #1]');
      expect(result?.attachmentIds).toEqual(['img-1']);
    });
  });
});
