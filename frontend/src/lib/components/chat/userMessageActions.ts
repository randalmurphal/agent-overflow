import type { ComposerInputSelection } from '../composer/composerInputSurface';
import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
import type { ComposerDraftSnapshot } from '../../stores/composerDraftSnapshots';
import type { Item } from '../../types/models';

/** Stage of the edit-and-resend flow (see `editResendFlow.svelte.ts`). */
export type UserMessageEditStage = 'editing' | 'preflight' | 'confirm' | 'executing';

/**
 * What the editor hands the flow at submit: the message exactly as it
 * would go on the wire. Captured ONCE, at the click, so a confirm dialog
 * consents to exactly what gets sent.
 */
export interface EditResendPayload {
  message: string;
  attachmentIds: string[];
  /** Same first-word classification a fresh composer send would apply. */
}

/**
 * The editor row's own view state — what has to survive the virtualizer
 * destroying and rebuilding the row mid-edit, but which no other surface
 * reads.
 *
 * ONE mutable object rather than fields on the session because the flow
 * (`editResendFlow.svelte.ts`) rebuilds the session on every stage
 * transition: a field written by the row would be discarded by the next
 * rebuild, and a session field is a derived value rather than `$state`,
 * so writing it would not be reactive either. The owner creates this as
 * `$state` once per session and passes the same reference through every
 * rebuild.
 */
export interface UserMessageEditUiState {
  /**
   * Focus the textarea (caret at the end) on the NEXT mount, then never
   * again. The first mount is the reader opening the editor; every later
   * one is a row scrolling back into view, where grabbing focus would
   * yank the caret out of wherever the reader had moved it — including
   * out of another pane entirely.
   */
  focusPending: boolean;
  /**
   * Where the caret was when the row was last destroyed. Restored on the
   * next mount WITHOUT focusing, so a reader who moved on keeps their
   * focus and a reader who comes back finds their place.
   */
  caret: ComposerInputSelection | null;
  /**
   * The discard confirm is open. On the session because the dialog is the
   * row's, and a remount while it is open must not silently answer it.
   */
  confirmDiscard: boolean;
  /**
   * Why the last submit was refused before it reached the wire, or ''.
   *
   * Written by the flow (which owns what may be sent) and rendered by the
   * editor next to its Send button, the same posture the composer gives
   * `commandError`: the user is looking at the text they just wrote, so
   * that is where the refusal belongs — not a toast, and not the pane
   * banner. Cleared by the next input change.
   */
  commandError: string;
}

/**
 * The single active edit-and-resend session, handed down to every user
 * row so the anchor row can render its editor and the others can lock
 * their pencil. Non-null for the WHOLE lifecycle (editor open →
 * preflight count RPC → confirm dialog → destructive RPC), not just the
 * RPC — `itemId` is the one identity for both facts, so a row can never
 * see "some edit is running" and "this row is the anchor" disagree.
 *
 * The draft store lives on the session rather than in the row because a
 * virtualizer remount must not lose what the user typed: rows rebind to
 * this object, and the flow inside `ChatView` (which never unmounts on
 * scroll) owns it.
 */
export interface UserMessageEditSession {
  /** Anchor user item — the message being rewritten. */
  itemId: string;
  /**
   * Local (`persistence: 'none'`) draft holding the edit. It is a COPY
   * of the message: nothing here touches the thread's real draft row.
   */
  draft: ComposerDraftStore;
  /** What the draft was seeded with. The dirty check compares against it. */
  seeded: ComposerDraftSnapshot;
  /**
   * Ids of attachments uploaded during THIS edit session — the only ones
   * whose backing records the editor may delete, and the only ones a
   * discard cleans up. Accumulate-only, so an upload that is added and
   * then removed is still known to have been ours.
   */
  sessionUploadedIds: Set<string>;
  /** Row-local view state that must outlive a windowing remount. */
  ui: UserMessageEditUiState;
  stage: UserMessageEditStage;
  /** Abandon the edit. The host owns the dirty confirm's consequences. */
  onCancel: () => void;
  /** Submit the edit: revert to this message and send the replacement. */
  onSubmit: (payload: EditResendPayload) => void;
}

export interface UserMessageActions {
  onForkMessage?: (item: Item) => void | Promise<void>;
  forkingItemId?: string | null;
  // Open the in-place editor on this message: the bubble becomes a
  // composer, and sending runs ONE backend call that reverts the
  // conversation to this message and sends the edited replacement.
  // Distinct from fork (which clones into a new thread). Undefined when
  // the provider doesn't support it, which drops the button (UserMessage
  // derives `canRequestEdit` from `typeof onEditMessage === 'function'`).
  onEditMessage?: (item: Item) => void | Promise<void>;
  // The one active edit session, or null. UserMessage disables every
  // pencil while any session exists — one edit at a time, and a disabled
  // control beats a click the flow guard would silently swallow (the
  // session spans the preflight window and the confirm dialog, not just
  // the destructive RPC).
  editSession?: UserMessageEditSession | null;
}
