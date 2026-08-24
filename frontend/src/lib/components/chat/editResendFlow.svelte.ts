// The edit-and-resend flow, from the pencil click to the destructive
// RPC's outcome. Extracted from `ChatView.svelte` the same way the
// scroll-session modules were extracted from `MessageTimeline.svelte`:
// the component keeps prop wiring, two thin `$effect` bodies and the
// confirm dialog's markup, and every rule lives here.
//
// One flow at a time per pane, tracked from the moment the pencil is
// clicked: 'editing' is the open editor, 'preflight' spans the
// background-task count RPC, 'confirm' parks the flow behind the
// ConfirmDialog, 'executing' spans the destructive RPC. Tracking the
// whole lifecycle in one value (instead of a busy id set only at execute
// time) is what lets `UserMessage` disable every pencil while any flow
// exists — a disabled control beats a silently swallowed click during
// the editor and preflight windows.
//
// Submitting is one-click UNLESS the revert would destroy something
// beyond the conversation tail itself — then it parks at 'confirm' and
// the dialog names exactly what else is lost: runningCount > 0 means the
// revert stops the provider session, which kills that background work
// (confirm re-runs with killRunningBackgroundTasks = true; the backend
// re-checks under the thread lock, so a task that starts between
// preflight and RPC surfaces as a loud refusal, never a silent kill).
// There is no draft-replace case: the editor owns a LOCAL draft store,
// so this flow never touches the composer's.
//
// The draft store, the seed, the session-upload set and the row's own
// view state live on the flow rather than in the row because the anchor
// row is virtualized: it can remount mid-edit, and ChatView never
// unmounts on scroll.

import {
  CountRunningBackgroundTasks,
  RevertConversationAndResendMessage,
} from '../../stores/bindings';
import {
  createComposerDraftStore,
  type ComposerDraftStore,
} from '../../stores/composerDraft.svelte';
import type { ComposerDraftSnapshot } from '../../stores/composerDraftSnapshots';
import { consumeResendRevertMarker } from '../../stores/eventsMessageRevert';
import type { ThreadPane } from '../../stores/thread.svelte';
import { addToast } from '../../stores/toast.svelte';
import {
  isTransportClassError,
  whenTransportConnected,
} from '../../stores/transportStatus.svelte';
import type { Attachment } from '../../types/attachment';
import type { Item } from '../../types/models';
import { restoredDraftSnapshotFromUserItem } from '../../utils/userMessageDraftSnapshot';
import { userFacingError } from '../../utils/userFacingError';
import { interceptedCommandNames } from '../composer/composerCommandEntries';
import { parseInterceptedCommand } from '../composer/composerCommandParse';
import { discardAbandonedAttachmentRecords } from '../composer/composerUploads.svelte';
import { createUserMessageEditUiState } from './userMessageEditUi.svelte';
import type {
  EditResendPayload,
  UserMessageEditSession,
  UserMessageEditStage,
  UserMessageEditUiState,
} from './userMessageActions';

/** Everything a session carries at every stage. */
interface EditFlowSession {
  item: Item;
  /** Local (`persistence: 'none'`) store holding the edit — a COPY. */
  draft: ComposerDraftStore;
  seeded: ComposerDraftSnapshot;
  sessionUploadedIds: Set<string>;
  /**
   * Reference-stable across the stage replacements below, which is the
   * whole point: it is the editor row's own state and the row remounts.
   */
  ui: UserMessageEditUiState;
  /**
   * A destructive RPC on this flow died with the wire, so whether its
   * saga committed — and whether a committed saga's resend then landed —
   * is unknown to this client. Set once by `handleTransportLoss` and
   * carried through every later stage: it is what stops the flow deleting
   * attachment records a sent message (or the backend's crash-copy draft
   * row) may now own. See `reclaimUploads`.
   */
  sagaOutcomeUnknown: boolean;
}

/**
 * Discriminated on `stage`, so the fields a stage does not have cannot be
 * read at all: the payload the confirm dialog consents to and the running
 * count it names exist exactly where they are meaningful, and no consumer
 * has to defend against an absent one it "knows" is there.
 *
 * `$state.raw` below: stages transition by whole-object replacement, and
 * the awaits in here compare flow identity across suspension points — a
 * deep proxy would break that identity.
 */
type EditFlow =
  | (EditFlowSession & { stage: 'editing' | 'preflight' })
  | (EditFlowSession & { stage: 'confirm'; runningCount: number; payload: EditResendPayload })
  | (EditFlowSession & { stage: 'executing'; payload: EditResendPayload });

type ExecutingFlow = Extract<EditFlow, { stage: 'executing' }>;

export interface PendingCutPosition {
  turnIndex: number;
  itemIndex: number;
}

// ---------------------------------------------------------------------------
// Per-thread execution registry.
//
// The backend serializes sagas on the thread's action lock, so a second
// concurrent RPC is answered with a refusal rather than a second
// truncation. What the lock does NOT protect is the frontend's OUTCOME
// classification: both flows consume reverted-event markers and both
// rebuild a composer from live state, so two panes editing one thread at
// the same time can attribute each other's events. Module-level because
// the panes are separate component instances with separate flows —
// there is no shared owner below the module.

const executingThreads = new Set<string>();

const CONCURRENT_SUBMIT_MESSAGE =
  'Another edit on this thread is already being sent. Wait for it to finish.';

function claimThreadExecution(threadId: string): boolean {
  if (executingThreads.has(threadId)) return false;
  executingThreads.add(threadId);
  return true;
}

function releaseThreadExecution(threadId: string): void {
  executingThreads.delete(threadId);
}

/** Test-only. Drops any lane a torn-down component never released. */
export function resetEditResendExecutionForTest(): void {
  executingThreads.clear();
}

// ---------------------------------------------------------------------------

/**
 * Merge an edit AHEAD of whatever the target draft already holds, exactly
 * as the backend's own crash copy does (`internal/composerdraft`
 * `MergeParts`): edited text first, existing content after a blank line,
 * attachments deduped with the edit's first. One helper for both recovery
 * branches — a hand-copied second version is how the two would drift.
 */
function mergeEditedAhead(
  edited: { content: string; attachments: Attachment[] },
  base: ComposerDraftSnapshot,
): ComposerDraftSnapshot {
  return {
    content: base.content.trim() === ''
      ? edited.content
      : `${edited.content}\n\n${base.content}`,
    attachments: [
      ...edited.attachments,
      ...base.attachments.filter((a) => !edited.attachments.some((e) => e.id === a.id)),
    ],
    terminalChips: [...base.terminalChips],
    sourceProposedPlan: base.sourceProposedPlan,
  };
}

export interface EditResendFlowOptions {
  /**
   * Getter, not the pane itself: the caller reads it off `$props()`, and
   * capturing that value at construction would freeze this flow to
   * whichever pane was mounted first.
   */
  getPane: () => ThreadPane;
  /** The COMPOSER's draft store — never the editor's local one. */
  getComposerDraft: () => ComposerDraftStore;
}

export interface EditResendFlowHandle {
  /** The one active session, or null. Handed to every user row. */
  readonly editSession: UserMessageEditSession | null;
  readonly stage: UserMessageEditStage | null;
  /**
   * Display position of the anchor while a revert is ACTUALLY in flight.
   * Rows strictly after it are what the committed revert will destroy,
   * and `MessageTimeline` dims exactly those. Positional, not per-turn:
   * Claude's item-granular cut can keep anchor-turn rows that PRECEDE the
   * anchor, and those are not doomed.
   */
  readonly pendingCutAfter: PendingCutPosition | null;
  readonly confirmOpen: boolean;
  readonly confirmDescription: string;
  /** Open the in-place editor on a past user message. */
  open(item: Item): void;
  /** The kill confirmation was accepted / declined. */
  confirmPending(): void;
  declinePending(): void;
  /** Invalidation passes; call from an `$effect` so their reads track. */
  invalidateOnThreadChange(): void;
  invalidateOnAnchorRemoved(): void;
  /** The host is going away mid-edit. */
  destroy(): void;
}

export function createEditResendFlow(opts: EditResendFlowOptions): EditResendFlowHandle {
  const pane = $derived(opts.getPane());

  let flow = $state.raw<EditFlow | null>(null);

  /**
   * The stage-independent half of a flow. Used instead of spreading the
   * whole object into the next stage: a spread would carry a previous
   * stage's `payload` / `runningCount` past the point where they mean
   * anything, and TypeScript does not excess-check spread properties.
   */
  function sessionOf(current: EditFlow): EditFlowSession {
    return {
      item: current.item,
      draft: current.draft,
      seeded: current.seeded,
      sessionUploadedIds: current.sessionUploadedIds,
      ui: current.ui,
      sagaOutcomeUnknown: current.sagaOutcomeUnknown,
    };
  }

  // Attachments uploaded INSIDE an abandoned edit back nothing: no
  // message references them and the local draft is gone. Deliberately NOT
  // called once the flow reaches 'executing': the send carries those ids,
  // so a committed resend owns them.
  //
  // After a transport-class failure that ownership is genuinely in doubt
  // for the REST of the flow's life, so an outcome-unknown flow never
  // reclaims. The anchor row looks like a discriminator (still there —
  // nothing committed; gone — the saga committed and the resent message
  // or the backend's merged crash-copy row references these ids), but it
  // is a STALE witness until the reconnect's resync replays the missed
  // events, and no replay-complete signal exists to wait on. The
  // asymmetry decides it: wrongly deleting a record corrupts a message
  // the user can see, wrongly keeping one leaves an invisible orphan row
  // that thread cleanup removes later.
  function reclaimUploads(current: EditFlow): void {
    if (current.sagaOutcomeUnknown) return;
    discardAbandonedAttachmentRecords(current.sessionUploadedIds);
  }

  function open(item: Item): void {
    const thread = pane.thread;
    if (!thread || flow) return;
    // The seed is the message itself; the store is LOCAL
    // (`persistence: 'none'`), so nothing typed here reaches the thread's
    // draft row or the shared snapshot registry — the composer keeps
    // holding whatever the user left in it.
    const seeded = restoredDraftSnapshotFromUserItem(item);
    const editDraft = createComposerDraftStore({ persistence: 'none' });
    editDraft.seedLocalSnapshot(thread.id, seeded);
    flow = {
      stage: 'editing',
      item,
      draft: editDraft,
      seeded,
      sessionUploadedIds: new Set<string>(),
      // Created once per session and carried by reference through every
      // stage replacement below — it is the editor ROW's state, and the
      // row remounts (see UserMessageEditUiState).
      ui: createUserMessageEditUiState(),
      // Per FLOW, never per thread or per anchor: a fresh edit has sent
      // nothing, so its own uploads are unambiguously its own even if an
      // earlier flow on this same message ended in doubt.
      sagaOutcomeUnknown: false,
    };
  }

  function cancel(): void {
    const current = flow;
    // Only the editor stage is the user's to abandon; past it the saga
    // owns the message (the editor disables its own Cancel to match).
    if (!current || current.stage !== 'editing') return;
    reclaimUploads(current);
    flow = null;
  }

  /**
   * Submit from the editor. The payload is captured by the editor and
   * passed through untouched, so the confirm dialog (if any) consents to
   * exactly what gets sent.
   */
  async function submit(payload: EditResendPayload): Promise<void> {
    const current = flow;
    if (!current || current.stage !== 'editing') return;
    const thread = pane.thread;
    if (!thread || thread.id !== current.item.threadId) return;

    // AO's intercepted commands (`/model`, `/clear`, Codex's `/review`…)
    // are consumed by the composer and never sent. There is nothing to
    // replace a message WITH here: the app would run the command and the
    // conversation would just lose its tail — and for a name the provider
    // also ships (`/model` on Claude), the resend would additionally
    // execute the CLI's own version of it. Refuse in the editor, where
    // the text still is.
    const intercepted = parseInterceptedCommand(
      payload.message,
      interceptedCommandNames(thread.provider),
    );
    if (intercepted) {
      current.ui.commandError =
        `/${intercepted.name} runs in the app and can't replace a sent message.`;
      return;
    }
    // Refuse before the preflight count goes out, so a second pane's
    // submit costs nothing and its editor keeps the user's text. The
    // atomic claim in `execute` is the one that closes the race — this
    // check is the user-facing half.
    if (executingThreads.has(thread.id)) {
      addToast('error', CONCURRENT_SUBMIT_MESSAGE);
      return;
    }

    const preflight: EditFlow = { ...sessionOf(current), stage: 'preflight' };
    flow = preflight;
    let runningCount = 0;
    try {
      runningCount = Number(await CountRunningBackgroundTasks(thread.id));
    } catch (err) {
      // The count is the only thing that failed — the message is intact,
      // so hand the editor back rather than dropping what was typed.
      if (flow === preflight) flow = { ...sessionOf(current), stage: 'editing' };
      addToast('error', `Failed to check background tasks: ${userFacingError(err)}`);
      return;
    }
    // The invalidation passes (thread switch, anchor removed) may have
    // voided the flow while the count RPC was in flight.
    if (flow !== preflight) return;
    if (runningCount > 0) {
      flow = { ...sessionOf(current), stage: 'confirm', runningCount, payload };
      return;
    }
    await execute(payload, false);
  }

  /**
   * Revert to the anchor and send the replacement, in ONE backend call
   * under one thread lock. The backend emits `user_message:reverted`
   * (carrying draftPendingResend) before it dispatches the send, and the
   * wire is FIFO, so the choreography the user sees is: the tail
   * collapses, then the edited message arrives as a normal item push.
   * Nothing is painted optimistically here — the timeline only ever
   * truncates on the backend's own event.
   */
  async function execute(
    payload: EditResendPayload,
    killRunningBackgroundTasks: boolean,
  ): Promise<void> {
    // Re-read rather than trust the caller's capture: two clicks landing
    // in one task both pass the caller's own stage check, and this is
    // where the second one stops.
    const current = flow;
    if (!current || current.stage === 'executing') return;
    const thread = pane.thread;
    if (!thread || thread.id !== current.item.threadId) {
      flow = null;
      return;
    }
    if (!claimThreadExecution(thread.id)) {
      // A second pane's flow on this thread took the lane while our
      // preflight count was in flight. Hand the editor back (or leave the
      // confirm dialog up) rather than racing it.
      addToast('error', CONCURRENT_SUBMIT_MESSAGE);
      if (current.stage === 'preflight') flow = { ...sessionOf(current), stage: 'editing' };
      return;
    }
    // Closing the "user fires a composer send during the saga" window is
    // `sendSuspended` on <Composer>, NOT `pane.setSendInFlight`. That flag
    // is the optimistic STOP-button gate: it flips the send button to Stop
    // and arms the global `thread.interrupt` keybinding, so raising it
    // here would offer an interrupt for a turn that does not exist yet and
    // let Esc race the backend's own locked revert. The saga is not this
    // composer's send and has nothing to stop — the honest affordance is a
    // disabled Send.
    const executing: ExecutingFlow = { ...sessionOf(current), stage: 'executing', payload };
    flow = executing;
    try {
      // Settle the composer's save pipeline BEFORE the destructive RPC:
      // cancel the debounce and wait out in-flight saves so a stale
      // composer save can't land after the backend stages its merged
      // crash-copy draft and clobber it. Saves the user triggers by typing
      // DURING the RPC can still land — that's why the failure recovery
      // below rebuilds from live frontend state instead of trusting the
      // row.
      await opts.getComposerDraft().prepareForExternalDraftReplace(thread.id);
      await RevertConversationAndResendMessage(thread.id, executing.item.id, {
        content: payload.message,
        attachmentIds: payload.attachmentIds,
        killRunningBackgroundTasks,
      });
      // No toast: the visible truncate plus the edited message arriving IS
      // the confirmation. The marker the reverted event recorded is
      // consumed here so it cannot linger and misclassify a later,
      // unrelated failure on this anchor.
      consumeResendRevertMarker(thread.id, executing.item.id);
      if (flow === executing) flow = null;
      // Land at the thread's new tail, following — as if the message had
      // just been sent from the bottom. A normal send never yanks a
      // scrolled-up reader, but the height this reader was parked at
      // measured rows the revert just destroyed, and they asked for this
      // message to become the tail; the resend streams there.
      // `stickToLatest` (MessageTimeline's `jumpToLatest`) reconciles a
      // windowed tail before pinning, and its post-tick bottom write
      // lands after the editor row unmounts.
      if (pane.thread?.id === thread.id) pane.scrollController?.stickToLatest?.();
    } catch (err) {
      handleFailure(executing, err);
    } finally {
      releaseThreadExecution(thread.id);
    }
  }

  /**
   * Which half of the saga failed is decided by the reverted-event marker,
   * never from the error text and never structurally from `pane.items` —
   * the event frame precedes the RPC rejection on the FIFO wire, so the
   * marker is authoritative even after a mid-RPC thread switch, when the
   * pane's items belong to ANOTHER thread and a structural check would
   * misread a plain refusal as a committed revert.
   *
   * That reasoning holds only while the socket survived the call, which is
   * why the transport-class branch comes first.
   */
  function handleFailure(failed: ExecutingFlow, err: unknown): void {
    if (isTransportClassError(err)) {
      handleTransportLoss(failed, err);
      return;
    }
    const committed = consumeResendRevertMarker(failed.item.threadId, failed.item.id);
    if (!committed) {
      // A guard refused before anything was committed (live turn,
      // unconsented background tasks, unsupported provider…). Nothing
      // happened, so hand the editor back with the user's text intact.
      if (flow === failed) {
        flow = { ...sessionOf(failed), stage: 'editing' };
      } else {
        // The flow was voided while the RPC was in flight (mid-RPC thread
        // switch): there is no editor to hand back, and the
        // executing-stage void deliberately left the session uploads for a
        // send that — it turns out — never happened. Reclaim them.
        reclaimUploads(failed);
      }
      addToast('error', `Edit failed: ${userFacingError(err)}`);
      return;
    }
    // The revert committed and the resend failed. The editor's own row is
    // gone with the truncate, so the composer is the only surface left.
    // The backend left its merged crash-copy draft in the row, but that
    // copy is only the process-crash backstop — a composer save fired by
    // typing during the RPC, or the draft store's own switch-flush on a
    // mid-RPC thread change, can have overwritten it — so the recovery
    // must not blindly trust the row.
    if (flow === failed) flow = null;
    void recoverEditedText(failed);
    addToast('error', 'Reverted, but sending failed — your message is in the composer.');
  }

  /**
   * The wire broke under the RPC. The frontend is now epistemically
   * crash-equivalent: it cannot know whether the saga committed, and for a
   * timed-out call the saga can still COMPLETE afterwards — so it cannot
   * know whether a committed saga's resend succeeded either.
   *
   * Every recovery this module does elsewhere would be a guess here, and
   * each guess destroys something real: deleting the session's attachment
   * records breaks a resend that landed, and repainting the composer from
   * live state duplicates a message the user can already see in the
   * transcript. So nothing is guessed and nothing is thrown away — the
   * flow goes BACK to its editor, still holding the edited text, and the
   * saga's own outcome resolves it once the connection returns. The
   * backend runs to completion regardless of the lost answer, and the
   * ANCHOR ROW is the frontend's existing witness for which way it went:
   *
   *   - Never arrived, or guard-refused: the anchor survives the gap
   *     replay, the editor stays open, and the user can simply send
   *     again. The composer reload finds the row unchanged.
   *   - Committed, resend succeeded: the replayed `user_message:reverted`
   *     removes the anchor, which is exactly what
   *     `invalidateOnAnchorRemoved` voids the (now 'editing') flow on —
   *     the executing-stage exemption no longer applies. The composer
   *     reload finds the untouched WIP, so nothing is duplicated.
   *   - Committed, resend failed: same void by the same route, and the
   *     composer reload finds the backend's merged crash copy, which
   *     already holds both texts.
   *
   * The one thing that must NOT follow the ordinary void path is the
   * session's attachment records — see `reclaimUploads`.
   */
  function handleTransportLoss(failed: ExecutingFlow, err: unknown): void {
    // Consumed for hygiene — a marker left behind would answer a later,
    // unrelated failure on this anchor — but its VALUE is ignored: the
    // frame may simply not have arrived before the socket died.
    consumeResendRevertMarker(failed.item.threadId, failed.item.id);
    console.error('Transport failed during an edit-and-resend:', err);
    if (flow === failed) {
      flow = { ...sessionOf(failed), sagaOutcomeUnknown: true, stage: 'editing' };
    }
    // When the flow was already voided mid-RPC (a thread switch), there is
    // no editor to hand back and its uploads stay untouched — that void
    // deliberately left them for a send that may well have landed, and
    // this failure does not disprove it.
    addToast(
      'error',
      'Connection lost while resending. If the message did not send, your edit is still in the editor.',
    );
    void restoreComposerAfterTransportLoss(failed.item.threadId);
  }

  async function restoreComposerAfterTransportLoss(threadId: string): Promise<void> {
    await whenTransportConnected();
    const composerDraft = opts.getComposerDraft();
    // `reloadFromBackend` discards unsaved local state by design (it is
    // the "the backend row is now the truth" path). That is right after a
    // saga whose own writes we cannot see, but not over text the user
    // typed while the connection was down and that has not been saved
    // yet — their keystrokes are newer than anything the row can hold.
    if (composerDraft.threadId === threadId && composerDraft.hasPendingSave) return;
    try {
      await composerDraft.reloadFromBackend(threadId);
    } catch (err) {
      console.error('Failed to reload the composer draft after a lost connection:', err);
    }
  }

  /**
   * Put the edited text back where the user can act on it after a
   * committed-revert-failed-resend. Two shapes, split on whether the
   * composer is still on the edit's thread:
   *
   *   - Same thread: rebuild from LIVE frontend state — the editor's
   *     text/attachments (still held by the flow) merged ahead of the
   *     composer's current content, keystrokes typed during the RPC
   *     included. Paint first, persist second: even if the persist fails,
   *     the text is on screen and sendable.
   *   - Thread switched mid-RPC: the composer store holds the NEW thread
   *     and must not bleed into (or be clobbered by) the old thread's
   *     recovery. Read the old thread's row through the store's own
   *     loader — the same normalization a hydrate applies, image
   *     placeholders included — and merge against what it ACTUALLY holds
   *     now. The containment check is the decider, and it is exact rather
   *     than heuristic: it tests the STAGED string (the message as
   *     `MergeParts` embedded it, trimmed), because the backend's merge is
   *     the only writer that ever puts it there. Present means the row is
   *     the intact crash copy — attachments included — and rewriting it
   *     could only race a fresher save.
   */
  async function recoverEditedText(failed: ExecutingFlow): Promise<void> {
    const threadId = failed.item.threadId;
    const edited = {
      content: failed.draft.content,
      attachments: [...failed.draft.attachments],
    };
    if (edited.content.trim() === '' && edited.attachments.length === 0) return;
    const composerDraft = opts.getComposerDraft();
    if (composerDraft.threadId === threadId) {
      const recovered = mergeEditedAhead(edited, {
        content: composerDraft.content,
        attachments: composerDraft.attachments,
        terminalChips: composerDraft.terminalChips,
        sourceProposedPlan: composerDraft.sourceProposedPlan,
      });
      composerDraft.applyOptimisticRestoredDraft(threadId, recovered);
      try {
        await composerDraft.restoreDraftFor(threadId, recovered);
      } catch (err) {
        // The paint above means the text IS on screen and sendable, so
        // this is a durability failure, not a loss — but it must not pass
        // for a save: the draft will not be there after a reload.
        addToast('error', `Recovered your message, but saving the draft failed: ${userFacingError(err)}`);
      }
      return;
    }
    try {
      const row = await composerDraft.loadPersistedSnapshot(threadId);
      const staged = failed.payload.message.trim();
      if (staged !== '' && row.content.includes(staged)) return;
      // restoreDraftFor persists to the named thread and skips the local
      // paint when the store points elsewhere — exactly the cross-thread
      // semantics wanted here.
      await composerDraft.restoreDraftFor(threadId, mergeEditedAhead(edited, row));
    } catch (err) {
      // The edited text still lives in the flow's local store until GC,
      // but there is no durable home left to put it in — say so rather
      // than fail silently.
      addToast('error', `Failed to restore edited message to the draft: ${userFacingError(err)}`);
    }
  }

  const editSession = $derived.by<UserMessageEditSession | null>(() => {
    const current = flow;
    if (!current) return null;
    return {
      itemId: current.item.id,
      draft: current.draft,
      seeded: current.seeded,
      sessionUploadedIds: current.sessionUploadedIds,
      ui: current.ui,
      stage: current.stage,
      onCancel: cancel,
      onSubmit: (payload) => { void submit(payload); },
    };
  });

  const pendingCutAfter = $derived.by<PendingCutPosition | null>(() => {
    const current = flow;
    if (current?.stage !== 'executing') return null;
    return { turnIndex: current.item.turnIndex, itemIndex: current.item.itemIndex };
  });

  const confirmDescription = $derived.by(() => {
    const current = flow;
    if (current?.stage !== 'confirm') return '';
    const noun = current.runningCount === 1 ? 'background task' : 'background tasks';
    return `Reverting stops the session, which kills ${current.runningCount} running ${noun}.`
      + ' This cannot be undone.';
  });

  return {
    get editSession() { return editSession; },
    get stage() { return flow?.stage ?? null; },
    get pendingCutAfter() { return pendingCutAfter; },
    get confirmOpen() { return flow?.stage === 'confirm'; },
    get confirmDescription() { return confirmDescription; },

    open,

    confirmPending(): void {
      const current = flow;
      if (current?.stage !== 'confirm') return;
      void execute(current.payload, current.runningCount > 0);
    },

    declinePending(): void {
      const current = flow;
      // Declining the kill returns to the editor, not to the transcript:
      // the user consented to nothing, and their edit is still valid.
      if (current?.stage !== 'confirm') return;
      flow = { ...sessionOf(current), stage: 'editing' };
    },

    // A thread switch voids the flow at any stage: the editor's consent
    // was given against the thread it was opened on, and an executing
    // flow's busy state is meaningless on another thread (the RPC itself
    // runs to completion and errors still surface via toast).
    invalidateOnThreadChange(): void {
      const current = flow;
      if (!current) return;
      if (pane.thread?.id === current.item.threadId) return;
      // Deliberately NOT once the RPC is out: the send may well succeed
      // and its message references those attachment ids.
      if (current.stage !== 'executing') reclaimUploads(current);
      flow = null;
    },

    // The flow is parked against a specific user row; if that row
    // disappears through another path — an un-send or a concurrent revert
    // reflected from a second pane on the same thread — the flow is void:
    // self-dismiss instead of letting Send fire a doomed RPC against a
    // deleted anchor. The executing stage is exempt because our own revert
    // removes the row when `user_message:reverted` lands mid-RPC.
    //
    // The `pane.items` read is deliberately behind the stage checks: the
    // array changes on every streamed item, and an unconditional read
    // would re-run the caller's effect for every one of them.
    invalidateOnAnchorRemoved(): void {
      const current = flow;
      if (!current || current.stage === 'executing') return;
      if (pane.items.some((it) => it.id === current.item.id)) return;
      reclaimUploads(current);
      flow = null;
    },

    // A pane closed mid-edit abandons the edit; same cleanup, same
    // executing-stage exemption as the invalidation passes.
    destroy(): void {
      const current = flow;
      if (current && current.stage !== 'executing') reclaimUploads(current);
    },
  };
}
