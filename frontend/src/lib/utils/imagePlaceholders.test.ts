import { describe, expect, it } from 'vitest';
import type { Attachment } from '../types/attachment';
import {
  ensureImagePlaceholders,
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
});
