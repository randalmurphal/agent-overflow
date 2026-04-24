import type { Attachment } from '../../types/attachment';
import {
  imagePlaceholderLabel,
  insertImagePlaceholder,
  reconcileImagePlaceholders,
  removeImagePlaceholderByAttachmentId,
  removeImagePlaceholderForKey,
} from '../../utils/imagePlaceholders';
import type { UploadInsertionPoint } from './composerUploads.svelte';

interface ComposerImagePlaceholderOptions {
  getTextarea: () => HTMLTextAreaElement | undefined;
  getContent: () => string;
  getAttachments: () => Attachment[];
  setContentAndAttachments: (content: string, attachments: Attachment[]) => void;
  removeAttachment: (id: string) => void;
  deleteAttachmentRecord: (id: string) => void;
  refreshTriggers: () => void;
  autosizeTextarea: () => void;
  hasUserInputPrompt: () => boolean;
}

export function createComposerImagePlaceholders(opts: ComposerImagePlaceholderOptions) {
  function setTextareaCursor(cursor: number): void {
    queueMicrotask(() => {
      const textarea = opts.getTextarea();
      if (!textarea) return;
      textarea.setSelectionRange(cursor, cursor);
      opts.autosizeTextarea();
      opts.refreshTriggers();
    });
  }

  function currentUploadInsertion(): UploadInsertionPoint | null {
    const textarea = opts.getTextarea();
    return textarea
      ? { start: textarea.selectionStart, end: textarea.selectionEnd }
      : null;
  }

  function addUploadedAttachment(
    attachment: Attachment,
    uploadInsertion: UploadInsertionPoint | null,
  ): void {
    const attachments = opts.getAttachments();
    const label = imagePlaceholderLabel(attachments.length + 1);
    const textarea = opts.getTextarea();
    const start = uploadInsertion?.start ?? textarea?.selectionStart ?? opts.getContent().length;
    const end = uploadInsertion?.end ?? textarea?.selectionEnd ?? start;
    const insertion = insertImagePlaceholder(opts.getContent(), label, start, end);
    opts.setContentAndAttachments(insertion.content, [...attachments, attachment]);
    if (uploadInsertion) {
      uploadInsertion.start = insertion.cursor;
      uploadInsertion.end = insertion.cursor;
    }
    setTextareaCursor(insertion.cursor);
  }

  function applyAttachmentRemoval(
    result: { attachmentIds: string[]; content: string; cursor: number },
  ): void {
    const removed = new Set(result.attachmentIds);
    const nextAttachments = opts.getAttachments()
      .filter((attachment) => !removed.has(attachment.id));
    opts.setContentAndAttachments(result.content, nextAttachments);
    setTextareaCursor(result.cursor);
    for (const attachmentId of result.attachmentIds) {
      opts.deleteAttachmentRecord(attachmentId);
    }
  }

  function removeAttachmentFromComposer(id: string): void {
    const result = removeImagePlaceholderByAttachmentId(
      opts.getContent(),
      opts.getAttachments(),
      id,
    );
    if (result) {
      applyAttachmentRemoval(result);
      return;
    }
    opts.removeAttachment(id);
    opts.deleteAttachmentRecord(id);
  }

  function handleAtomicPlaceholderKeydown(event: KeyboardEvent): boolean {
    if (event.key !== 'Backspace' && event.key !== 'Delete') return false;
    if (opts.hasUserInputPrompt() || opts.getAttachments().length === 0) return false;

    const textarea = opts.getTextarea();
    if (!textarea) return false;

    const result = removeImagePlaceholderForKey(
      opts.getContent(),
      opts.getAttachments(),
      textarea.selectionStart,
      textarea.selectionEnd,
      event.key,
    );
    if (!result) return false;

    event.preventDefault();
    applyAttachmentRemoval(result);
    return true;
  }

  function handleBeforeInput(event: InputEvent): void {
    if (!event.inputType.startsWith('delete')) return;
    if (opts.hasUserInputPrompt() || opts.getAttachments().length === 0) return;

    const textarea = opts.getTextarea();
    if (!textarea) return;

    const key = event.inputType === 'deleteContentForward' ? 'Delete' : 'Backspace';
    const result = removeImagePlaceholderForKey(
      opts.getContent(),
      opts.getAttachments(),
      textarea.selectionStart,
      textarea.selectionEnd,
      key,
    );
    if (!result) return;

    event.preventDefault();
    applyAttachmentRemoval(result);
  }

  function reconcileContent(value: string): boolean {
    const reconciled = reconcileImagePlaceholders(value, opts.getAttachments());
    if (reconciled.removedAttachmentIds.length === 0) return false;

    opts.setContentAndAttachments(reconciled.content, reconciled.attachments);
    for (const attachmentId of reconciled.removedAttachmentIds) {
      opts.deleteAttachmentRecord(attachmentId);
    }
    return true;
  }

  return {
    addUploadedAttachment,
    currentUploadInsertion,
    handleAtomicPlaceholderKeydown,
    handleBeforeInput,
    reconcileContent,
    removeAttachmentFromComposer,
  };
}
