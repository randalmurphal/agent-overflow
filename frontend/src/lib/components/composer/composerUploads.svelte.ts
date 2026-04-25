// Composer drag / drop / paste / upload flow.
//
// Extracted from Composer.svelte so the .svelte shell stays focused on
// text entry + send. The upload flow is stateful (dragDepth counter, per-
// file rejection, per-thread guard so a slow upload can't land in the
// wrong pane) but entirely decoupled from the textarea — which is why
// it's a `.svelte.ts` module rather than a helper function.
//
// Construction: `createComposerUploads({ getThreadId, addAttachment, ... })`.
// The caller passes in the thread-id getter + attachment store mutator so
// this module doesn't need to import the ThreadPane / ComposerDraftStore
// shapes directly — keeps the coupling one-way.

import { DeleteAttachment, UploadAttachment } from '../../stores/bindings';
import { addToast } from '../../stores/toast.svelte';
import { userFacingError } from '../../utils/userFacingError';
import type { Attachment } from '../../types/attachment';
import {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  DEFAULT_MAX_ATTACHMENT_SIZE,
  extractClipboardImages,
  fileToBase64,
  hasImagePayload,
  rejectionReason,
} from './attachmentHelpers';

export interface UploadInsertionPoint {
  start: number;
  end: number;
}

export interface ComposerUploadsOptions {
  /**
   * Returns the thread id at the moment an upload starts. Captured per-call
   * so uploads race-safe against a thread switch mid-upload.
   */
  getThreadId: () => string | null;
  /** Fired when a freshly-uploaded Attachment should be added to the draft. */
  addAttachment: (attachment: Attachment, insertion: UploadInsertionPoint | null) => void;
  /** Fired after a successful DeleteAttachment call so the draft drops it. */
  removeAttachment: (id: string) => void;
  /** Returns how many attachments are already in the composer draft. */
  getAttachmentCount?: () => number;
  /** Size ceiling for a single uploaded file. Defaults to 10 MiB. */
  maxAttachmentSize?: number;
  /** Count ceiling for a single send. Defaults to 8 to match provider UX. */
  maxAttachments?: number;
}

export interface ComposerUploadsHandle {
  readonly dragActive: boolean;
  handleDragEnter(event: DragEvent): void;
  handleDragLeave(event: DragEvent): void;
  handleDragOver(event: DragEvent): void;
  handleDrop(event: DragEvent, insertion?: UploadInsertionPoint | null): Promise<void>;
  handlePaste(event: ClipboardEvent, insertion?: UploadInsertionPoint | null): Promise<void>;
  deleteAttachmentRecord(id: string): Promise<void>;
  removeAttachment(id: string): Promise<void>;
}

export function createComposerUploads(opts: ComposerUploadsOptions): ComposerUploadsHandle {
  const maxSize = opts.maxAttachmentSize ?? DEFAULT_MAX_ATTACHMENT_SIZE;
  const maxAttachments = opts.maxAttachments ?? DEFAULT_MAX_ATTACHMENT_COUNT;

  let dragDepth = $state(0);

  async function uploadOne(
    threadId: string,
    file: File,
    insertion: UploadInsertionPoint | null,
  ): Promise<boolean> {
    // Pre-upload guard: reject by size + MIME / extension before we
    // burn cycles on base64 + ship the bytes over the wire. The same
    // check runs server-side, but failing early here keeps a
    // misclicked 50MB drop from freezing the UI for the round-trip.
    const rejection = rejectionReason(file, maxSize);
    if (rejection) {
      addToast('warning', rejection);
      return false;
    }
    try {
      const base64 = await fileToBase64(file);
      const record = (await UploadAttachment(
        threadId,
        file.name,
        file.type || '',
        base64,
      )) as Attachment;
      // Guard against thread-switch-in-flight: only stamp the draft when
      // we're still on the thread the user initiated the upload from.
      if (opts.getThreadId() === threadId) {
        opts.addAttachment(record, insertion);
        return true;
      }
    } catch (err) {
      console.error('UploadAttachment failed:', err);
      addToast('error', userFacingError(err));
    }
    return false;
  }

  async function uploadFiles(
    files: FileList | File[],
    insertion: UploadInsertionPoint | null,
  ): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) return;
    const existingCount = opts.getAttachmentCount?.() ?? 0;
    const availableSlots = Math.max(0, maxAttachments - existingCount);
    if (availableSlots === 0) {
      addToast('warning', `You can attach up to ${maxAttachments} images per message.`);
      return;
    }
    const list = Array.from(files);
    let acceptedCount = 0;
    let processedCount = 0;
    for (const file of list) {
      if (acceptedCount >= availableSlots) break;
      processedCount += 1;
      const accepted = await uploadOne(threadId, file, insertion);
      if (accepted) acceptedCount += 1;
    }
    if (processedCount < list.length) {
      addToast('warning', `Only the first ${availableSlots} valid image${availableSlots === 1 ? '' : 's'} were attached.`);
    }
  }

  async function deleteAttachmentRecord(id: string): Promise<void> {
    try {
      await DeleteAttachment(id);
    } catch (err) {
      console.error('DeleteAttachment failed:', err);
      addToast('warning', userFacingError(err));
    }
  }

  return {
    get dragActive() { return dragDepth > 0; },

    handleDragEnter(event: DragEvent): void {
      if (!hasImagePayload(event)) return;
      event.preventDefault();
      dragDepth += 1;
    },

    handleDragLeave(_event: DragEvent): void {
      if (dragDepth > 0) dragDepth -= 1;
    },

    handleDragOver(event: DragEvent): void {
      if (!hasImagePayload(event)) return;
      event.preventDefault();
    },

    async handleDrop(event: DragEvent, insertion: UploadInsertionPoint | null = null): Promise<void> {
      dragDepth = 0;
      if (!event.dataTransfer) return;
      const files = event.dataTransfer.files;
      if (!files || files.length === 0) return;
      event.preventDefault();
      await uploadFiles(files, insertion);
    },

    async handlePaste(event: ClipboardEvent, insertion: UploadInsertionPoint | null = null): Promise<void> {
      const files = extractClipboardImages(event);
      if (files.length === 0) return;
      event.preventDefault();
      await uploadFiles(files, insertion);
    },

    async deleteAttachmentRecord(id: string): Promise<void> {
      await deleteAttachmentRecord(id);
    },

    async removeAttachment(id: string): Promise<void> {
      opts.removeAttachment(id);
      await deleteAttachmentRecord(id);
    },
  };
}
