// The contract between a host and `ComposerInputSurface.svelte`.
//
// The surface is the composer's editing core: a textarea plus everything
// whose job is getting text and attachments into a draft — completion
// menus, image placeholders, uploads, terminal chips, command highlight.
// It owns none of the send decision, the thread lifecycle, or the pending
// prompt panels; those stay with the host (`Composer.svelte`, and the
// in-place message editor).
//
// Types live here rather than in the component so a host can name the
// handle it holds without importing the component's chunk.

// Deliberately NOT a narrow role: this surface hosts the `/` command
// runner, which reaches `makeCommandContext` and the whole command
// action surface. A role for that would just re-describe `ThreadPane`.
import type { ThreadPane } from '../../stores/thread.svelte';
import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
import type {
  AttachmentPreviewCache,
  ExpandedImagePreview,
} from '../../utils/attachmentPreview.svelte';

/** A textarea selection, as `setSelectionRange` takes it. */
export interface ComposerInputSelection {
  start: number;
  end: number;
}

export interface ComposerInputValueInfo {
  /**
   * True when the surface has ALREADY written this value to the draft —
   * an image placeholder was deleted by typing, so the draft's content and
   * attachment list moved together. The host must not write the raw value
   * over that reconciliation.
   */
  appliedToDraft: boolean;
}

export interface ComposerInputSurfaceProps {
  pane: ThreadPane;
  /** Works with a persisting or a `persistence: 'none'` store. */
  draft: ComposerDraftStore;

  // ---- textarea state, derived by the host ----
  value: string;
  disabled: boolean;
  placeholder: string;
  /** The textarea's value changed. See `ComposerInputValueInfo`. */
  oninput: (value: string, info: ComposerInputValueInfo) => void;

  // ---- keyboard ----
  /**
   * A plain Enter that no popover, IME composition or placeholder
   * deletion claimed. The surface has already called `preventDefault`.
   */
  onSubmitEnter: () => void;
  /** The host's first look at a keydown. Return true when consumed. */
  onKeydown?: (event: KeyboardEvent) => boolean;
  /**
   * The host's second look, after the completion menus have declined the
   * keystroke — an open menu owns ArrowUp/ArrowDown, so anything arrow-
   * shaped that must yield to it (history recall) claims here. Return
   * true when consumed; the claimer owns preventDefault.
   */
  onKeydownAfterPopovers?: (event: KeyboardEvent) => boolean;

  // ---- mode ----
  /**
   * Whether the textarea is editing `draft.content`. False parks the
   * draft-coupled behaviours — image-placeholder surgery and the command
   * highlight — while the textarea serves something else (the composer's
   * pending user-input answer). Default true.
   */
  editsDraft?: boolean;
  /**
   * Render the attachment row, the terminal-chip row and the command
   * error. Default true; the composer drops them while an approval or
   * user-input prompt owns the card.
   */
  showDraftRows?: boolean;

  // ---- attachments ----
  /**
   * Blob-URL cache for the attachment row's thumbnails. Without one the
   * row owns the blobs and revokes them on destroy, which is right for a
   * composer that outlives its thread; a surface mounted inside a
   * VIRTUALIZED row must pass the pane's cache
   * (`pane.attachmentCacheFor(itemId)`) or every remount re-fetches and
   * re-decodes every attached image.
   */
  attachmentCache?: AttachmentPreviewCache;
  /**
   * Host veto for the four attachment add-paths (paste + three drag
   * events). Return true to refuse the event; the host owns the reason it
   * shows. `notify` is false for the passive drag events, which must not
   * toast on every mouse move.
   */
  blockAttachment?: (event: DragEvent | ClipboardEvent, notify: boolean) => boolean;
  /**
   * Whether removing an attachment also deletes its backing record.
   * Default true. A surface editing a message it did not upload for must
   * answer false for those ids — the record still belongs to the message.
   */
  shouldDeleteAttachmentRecord?: (id: string) => boolean;
  /** Thread an upload starting now belongs to. Defaults to `pane.threadId`. */
  uploadThreadId?: () => string | null;
  /**
   * Create or adopt a thread for an upload that started without one.
   * Defaults to `pane.ensureMaterializedThread()`.
   */
  ensureUploadThreadId?: () => Promise<string | null>;

  onImageExpand?: (preview: ExpandedImagePreview) => void;
}

export interface ComposerInputSurfaceHandle {
  /** False until the textarea is in the DOM. */
  inputMounted(): boolean;
  focusInputAtEnd(): void;
  /** Focus with the caret at offset 0 — history recall's up-walk parks
   * the caret there so the next ArrowUp passes the caret gate again. */
  focusInputAtStart(): void;
  /** The textarea's current selection, or null when it is not mounted. */
  inputSelection(): ComposerInputSelection | null;
  /**
   * Put the caret back WITHOUT focusing. A host restoring a caret across
   * a remount cannot know where focus went in the meantime, and yanking
   * it back to a row that merely re-rendered is worse than a caret that
   * waits for the next click.
   */
  restoreInputSelection(selection: ComposerInputSelection): void;
  /** Collapse the textarea back to one row (after a send / a cleared answer). */
  resetInputHeight(): void;
  /**
   * Schedule a swap of the <textarea> element for a fresh one in an idle
   * slot (off the send's keydown task), refocusing only if it held focus at
   * swap time. Hosts call this after a send clears the draft: Blink retains
   * one edit command per typed character for the element's lifetime (each
   * pinning an Oilpan page), and the element swap is the only release.
   * Invisible by measurement — see the comment on `inputEpoch` in the
   * component. Skipped mid-IME-composition and when the user has resumed
   * typing before the slot fires; the next send retries.
   */
  recreateInput(): void;
  autosizeInput(): void;
  /** True while an upload batch is in flight. */
  uploading(): boolean;
  /**
   * Resolves once no upload batch is in flight. A host awaits this before it
   * snapshots the draft to send: dropping a file and pressing Enter is one
   * gesture, and the ids of uploads still in the air are not in the draft yet.
   */
  waitForUploads(): Promise<void>;

  // Drag handlers for the host's drop zone. The card the user sees is the
  // target, and it is larger than this surface, so the host owns the
  // element and the surface owns the behaviour.
  handleDragEnter(event: DragEvent): void;
  handleDragOver(event: DragEvent): void;
  handleDragLeave(event: DragEvent): void;
  handleDrop(event: DragEvent): void;

  /**
   * Consume the message if it invokes a command AO acts on rather than
   * sends. True means the host must NOT send — it is already handled.
   */
  consumeInterceptedSend(message: string): boolean;
}
