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

import { DeleteAttachment } from '../../stores/bindings';
import { uploadAttachmentBytes } from '../../transport/attachmentTransfer';
import { addToast } from '../../stores/toast.svelte';
import { userFacingError } from '../../utils/userFacingError';
import type { Attachment } from '../../types/attachment';
import {
  DEFAULT_MAX_ATTACHMENT_COUNT,
  DEFAULT_MAX_ATTACHMENT_SIZE,
  extractClipboardImages,
  hasFilePayload,
  rejectionReason,
} from './attachmentHelpers';
import { compressImageToFit, shouldCompressImage } from './imageCompress';

export interface UploadInsertionPoint {
  start: number;
  end: number;
}

/**
 * Drop attachment records that ended up backing nothing — an abandoned
 * edit session's uploads, say, where no message references the ids and
 * the draft holding them is gone.
 *
 * Takes the owning thread because deletion is thread-scoped at the
 * boundary: the backend refuses an id that belongs to another thread.
 *
 * Here rather than at the caller because this layer owns attachment
 * record deletion; a second `DeleteAttachment` call site would be a
 * second error policy to keep in step. It is a DIFFERENT policy from the
 * handle's `deleteAttachmentRecord` on purpose: that one answers a
 * gesture the user just made (remove this image), so a failure is worth a
 * toast, while this runs after the user has already left the surface, and
 * a leaked blob is a housekeeping miss rather than something to interrupt
 * them with. Fire-and-forget, never silent.
 */
export function discardAbandonedAttachmentRecords(threadId: string, ids: Iterable<string>): void {
  for (const id of ids) {
    void DeleteAttachment(threadId, id).catch((err) => {
      console.error('Failed to delete abandoned attachment record:', err);
    });
  }
}

export interface ComposerUploadsOptions {
  /**
   * Returns the thread id at the moment an upload starts. Captured per-call
   * so uploads race-safe against a thread switch mid-upload.
   */
  getThreadId: () => string | null;
  /** Creates/loads a backend thread when the composer is on a local placeholder. */
  ensureThreadId?: () => Promise<string | null>;
  /** Fired when a freshly-uploaded Attachment should be added to the draft. */
  addAttachment: (attachment: Attachment, insertion: UploadInsertionPoint | null) => void;
  /** Fired after a successful DeleteAttachment call so the draft drops it. */
  removeAttachment: (id: string) => void;
  /** Returns how many attachments are already in the composer draft. */
  getAttachmentCount?: () => number;
  /**
   * Size ceiling for a single uploaded IMAGE, and the target recompression
   * aims at. Defaults to 10 MiB. A `file` is bounded by the policy constant
   * (`DEFAULT_MAX_FILE_ATTACHMENT_SIZE`), which nothing else consumes.
   */
  maxAttachmentSize?: number;
  /** Count ceiling for a single send. Defaults to 8 to match provider UX. */
  maxAttachments?: number;
}

export interface ComposerUploadsHandle {
  readonly dragActive: boolean;
  readonly uploading: boolean;
  handleDragEnter(event: DragEvent): void;
  handleDragLeave(event: DragEvent): void;
  handleDragOver(event: DragEvent): void;
  handleDrop(event: DragEvent, insertion?: UploadInsertionPoint | null): Promise<void>;
  handlePaste(event: ClipboardEvent, insertion?: UploadInsertionPoint | null): Promise<void>;
  deleteAttachmentRecord(id: string): Promise<void>;
  removeAttachment(id: string): Promise<void>;
  /**
   * Resolves once no upload batch is in flight. A send awaits this before it
   * snapshots the draft: dropping a file and pressing Enter is one gesture to
   * the user, and without the wait the message goes without the attachment
   * whose upload had not landed yet.
   */
  waitForUploads(): Promise<void>;
}

export function createComposerUploads(opts: ComposerUploadsOptions): ComposerUploadsHandle {
  const maxSize = opts.maxAttachmentSize ?? DEFAULT_MAX_ATTACHMENT_SIZE;
  const maxAttachments = opts.maxAttachments ?? DEFAULT_MAX_ATTACHMENT_COUNT;

  let dragDepth = $state(0);
  let activeUploadBatches = $state(0);
  // Resolved (and emptied) the moment the batch count reaches zero, so
  // `waitForUploads` costs nothing while idle and needs no polling.
  let uploadIdleWaiters: Array<() => void> = [];

  async function uploadOne(
    threadId: string,
    file: File,
    insertion: UploadInsertionPoint | null,
  ): Promise<boolean> {
    // An over-limit image gets one recompression attempt before the
    // size guard runs — a HiDPI screenshot paste routinely exceeds the
    // limit and re-encoding it beats bouncing the user to an editor.
    // Failure (undecodable, or still too large at the smallest ladder
    // step) falls through to the original file and its size rejection.
    let upload = file;
    if (shouldCompressImage(file, maxSize)) {
      try {
        upload = (await compressImageToFit(file, maxSize)) ?? file;
      } catch (err) {
        console.error('image compression failed:', err);
      }
    }
    // Pre-upload guard: reject by the kind's size ceiling before the
    // bytes go anywhere. The same check runs when the ticket is minted
    // and again in the store, but failing here keeps an over-limit drop
    // from costing a round trip at all.
    const rejection = rejectionReason(upload, maxSize);
    if (rejection) {
      addToast('warning', rejection);
      return false;
    }
    try {
      // Two hops, and the file is never a string in either: a mint that
      // authorizes exactly these bytes, then the bytes themselves as the
      // body of one PUT.
      const record = await uploadAttachmentBytes(threadId, upload);
      // Guard against thread-switch-in-flight: only stamp the draft when
      // we're still on the thread the user initiated the upload from.
      if (opts.getThreadId() === threadId) {
        opts.addAttachment(record, insertion);
        return true;
      }
    } catch (err) {
      console.error('attachment upload failed:', err);
      addToast('error', userFacingError(err));
    }
    return false;
  }

  async function uploadFiles(
    files: FileList | File[],
    insertion: UploadInsertionPoint | null,
  ): Promise<void> {
    const list = Array.from(files);
    if (list.length === 0) return;

    activeUploadBatches += 1;
    try {
      const threadId = opts.getThreadId() ?? await opts.ensureThreadId?.() ?? null;
      if (!threadId) return;
      const existingCount = opts.getAttachmentCount?.() ?? 0;
      const availableSlots = Math.max(0, maxAttachments - existingCount);
      if (availableSlots === 0) {
        addToast('warning', `You can attach up to ${maxAttachments} attachments per message.`);
        return;
      }
      let acceptedCount = 0;
      let processedCount = 0;
      for (const file of list) {
        if (acceptedCount >= availableSlots) break;
        processedCount += 1;
        const accepted = await uploadOne(threadId, file, insertion);
        if (accepted) acceptedCount += 1;
      }
      if (processedCount < list.length) {
        addToast('warning', `Only the first ${availableSlots} valid file${availableSlots === 1 ? '' : 's'} were attached.`);
      }
    } finally {
      activeUploadBatches = Math.max(0, activeUploadBatches - 1);
      if (activeUploadBatches === 0 && uploadIdleWaiters.length > 0) {
        const waiters = uploadIdleWaiters;
        uploadIdleWaiters = [];
        for (const resolve of waiters) resolve();
      }
    }
  }

  async function deleteAttachmentRecord(id: string): Promise<void> {
    // The record's thread is the composer's current one: `uploadOne` only
    // stamps a record into the draft while `getThreadId()` still matches
    // the thread it uploaded to, so anything the user can remove here
    // belongs to the thread showing it.
    const threadId = opts.getThreadId();
    if (!threadId) return;
    try {
      await DeleteAttachment(threadId, id);
    } catch (err) {
      console.error('DeleteAttachment failed:', err);
      addToast('warning', userFacingError(err));
    }
  }

  return {
    get dragActive() { return dragDepth > 0; },
    get uploading() { return activeUploadBatches > 0; },

    handleDragEnter(event: DragEvent): void {
      if (!hasFilePayload(event)) return;
      event.preventDefault();
      dragDepth += 1;
    },

    handleDragLeave(_event: DragEvent): void {
      if (dragDepth > 0) dragDepth -= 1;
    },

    handleDragOver(event: DragEvent): void {
      if (!hasFilePayload(event)) return;
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

    waitForUploads(): Promise<void> {
      if (activeUploadBatches === 0) return Promise.resolve();
      return new Promise((resolve) => {
        uploadIdleWaiters.push(resolve);
      });
    },
  };
}
