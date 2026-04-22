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
import { errString } from '../../utils/errors';
import type { Attachment } from '../../types/attachment';
import {
  DEFAULT_MAX_ATTACHMENT_SIZE,
  extractClipboardImages,
  fileToBase64,
  hasImagePayload,
  rejectionReason,
} from './attachmentHelpers';

export interface ComposerUploadsOptions {
  /**
   * Returns the thread id at the moment an upload starts. Captured per-call
   * so uploads race-safe against a thread switch mid-upload.
   */
  getThreadId: () => string | null;
  /** Fired when a freshly-uploaded Attachment should be added to the draft. */
  addAttachment: (attachment: Attachment) => void;
  /** Fired after a successful DeleteAttachment call so the draft drops it. */
  removeAttachment: (id: string) => void;
  /** Size ceiling for a single uploaded file. Defaults to 10 MiB. */
  maxAttachmentSize?: number;
}

export interface ComposerUploadsHandle {
  readonly dragActive: boolean;
  handleDragEnter(event: DragEvent): void;
  handleDragLeave(event: DragEvent): void;
  handleDragOver(event: DragEvent): void;
  handleDrop(event: DragEvent): Promise<void>;
  handlePaste(event: ClipboardEvent): Promise<void>;
  removeAttachment(id: string): Promise<void>;
}

export function createComposerUploads(opts: ComposerUploadsOptions): ComposerUploadsHandle {
  const maxSize = opts.maxAttachmentSize ?? DEFAULT_MAX_ATTACHMENT_SIZE;

  let dragDepth = $state(0);

  async function uploadOne(threadId: string, file: File): Promise<void> {
    const rejection = rejectionReason(file, maxSize);
    if (rejection) {
      addToast('warning', rejection);
      return;
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
        opts.addAttachment(record);
      }
    } catch (err) {
      console.error('UploadAttachment failed:', err);
      addToast('error', `Upload failed: ${errString(err)}`);
    }
  }

  async function uploadFiles(files: FileList | File[]): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) return;
    const list = Array.from(files);
    for (const file of list) {
      await uploadOne(threadId, file);
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

    async handleDrop(event: DragEvent): Promise<void> {
      dragDepth = 0;
      if (!event.dataTransfer) return;
      const files = event.dataTransfer.files;
      if (!files || files.length === 0) return;
      event.preventDefault();
      await uploadFiles(files);
    },

    async handlePaste(event: ClipboardEvent): Promise<void> {
      const files = extractClipboardImages(event);
      if (files.length === 0) return;
      event.preventDefault();
      await uploadFiles(files);
    },

    async removeAttachment(id: string): Promise<void> {
      opts.removeAttachment(id);
      try {
        await DeleteAttachment(id);
      } catch (err) {
        console.error('DeleteAttachment failed:', err);
        addToast('warning', `Failed to delete attachment: ${errString(err)}`);
      }
    },
  };
}
