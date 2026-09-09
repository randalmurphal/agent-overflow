# components/composer/

## The surface is shared, the host is not

`ComposerInputSurface.svelte` is the editing core: the textarea plus
everything whose job is getting text and attachments into a draft
(completion menus, image placeholders, uploads, terminal chips, command
highlight). It owns none of the send decision, the thread lifecycle, or
the pending prompt panels, which stay with the host. Both
`Composer.svelte` and chat's `UserMessageEditor.svelte` host it, so a new
place that edits a message extends the surface rather than growing a
parallel one. `composerInputSurface.ts` holds the prop and handle types so
a host can name what it holds without importing the component's chunk.

`Composer.svelte` stays a shell. The send and interrupt flow lives in
`composerSend.ts`, deliberately holding nothing reactive: each call
captures the current thread id and draft snapshot, then delegates back to
the pane and draft store. Drag, drop, paste and upload live in
`composerUploads.svelte.ts`, which carries an editing-context guard so a slow
upload cannot land in the wrong pane. The bytes themselves go over HTTP,
not the RPC wire: `uploadAttachmentBytes`
(`lib/transport/attachmentTransfer.ts`) mints a single-use ticket for
exactly this file and PUTs the `File` as the body, so a 10 MiB paste is
never a base64 string in a WebSocket frame. `compressImageToFit` still
runs first and is unchanged — a re-encode that fits beats a rejection,
whatever carries the result.

Direct and queued sends clear only their captured local composer snapshot.
The accepting backend operation compares `consumeDraft` to the current saved
row before consuming it; newer drafts from any frontend survive. Both send
paths use the same captured raw snapshot (including terminal chips and plan
source), independently of expanded message text. Queue dispatch does not
consume drafts again. Never issue a separate `ClearDraft` around a send. Capture the thread, attachments,
rollback snapshot and provisional row before awaiting draft-save settlement,
so switching threads during a slow save cannot mix one thread's text with
another's attachments or clear the newly opened composer. Unsaved snapshots
are recorded before issuing a save, never reinserted by its delayed rejection.

Empty-draft deletion holds `withEmptyDraftCleanup` through the RPC and local
placeholder restoration. The deletion broadcast evicts every client's row and
caches, but leaves that initiating pane for the cleanup result to restage;
closing it on its own echo would discard text typed while deletion was pending.
Other clients close panes showing the deleted thread normally.
A received deletion event proves success even if the RPC reply is lost; run
the same restoration path so newly typed content survives that interruption.
Cleanup also requires the draft store's local edit/materialization ownership.
Merely hydrating an empty remote draft never grants it: another screen may have
listed the row while its first content save is still debounced. Editing and
then clearing that remote draft locally grants cleanup normally.

## An attachment is one of two kinds

The server decides it (`attachment.classifyUpload`), the record carries
it, and `attachmentHelpers.classifyAttachment` is the frontend's copy of
that rule — used for the pre-upload size guard and for whether an
oversized payload is worth recompressing. Nothing is rejected on type any
more: an unrecognised one is a `file`. The caps differ (10 MiB image,
50 MiB file), so a rejection message names the kind.

- **An image** is bound positionally to an `[Image #N]` marker in the
  textarea, and **N counts IMAGES only**. Every numbering and matching
  pass in `utils/imagePlaceholders.ts` runs over `imageAttachments()`
  (`types/attachment.ts`), so a file sitting between two images does not
  shift the second one's number. The tile's `#N` badge is the same index.
- **A file gets no textarea text at all.** It reaches the agent as a path
  line the BACKEND appends to the provider payload, so
  `addUploadedAttachment` inserts nothing and `ensureImagePlaceholders`
  appends nothing for it. Its only removal gesture is its own chip:
  `reconcileImagePlaceholders` must never drop a file, because a file has
  no marker whose absence could mean the user deleted it.
- **A file's bytes are never served.** `GetAttachmentThumbnail` errors
  for one and the download route refuses it, so `createAttachmentPreviews`
  skips the kind entirely rather than logging a guaranteed failure per
  file per mount.

Paste stays image-only (`extractClipboardImages`); drag/drop and the attachment
picker take any file. `AttachmentPicker` exposes Take photo, Photos and Files on Android;
the shared input surface owns the file inputs and calls the same `uploadFiles`
path as drop/paste. Capture the pane generation when opening the system picker
and discard a selection returned after a thread switch. Camera capture uses
`accept="image/*" capture="environment"` with one file; clear capture and restore
multiple selection before opening Photos or Files. Keep scope/provider/
prompt gates on both opening and accepting a selection.

On compact layouts the workspace strip combines branch and worktree into one
trigger on a second logical row beneath machine/project. Tokens/cost stay
vertically centered on the right, and long location labels wrap within their
allocated width rather than clipping. Both underlying pickers stay
mounted and use that visible anchor, so keyboard commands and popup handoffs
share their existing fetch, focus and mutation paths. New-branch naming opens
inside the workspace sheet; it must not widen the footer.

An upload whose composer moved threads mid-flight DELETES its record.
The bytes finished landing on a thread nobody is looking at any more, and
nothing will ever reference the row — so it is discarded through the same
fire-and-forget `discardAbandonedAttachmentRecords` an abandoned draft
uses, rather than left as a database row and a file on disk that no
message, no draft and no later pass knows about.

Send admission is acquired synchronously before uploads, materialization, or
worktree preparation. Repeated taps and Enter share that admission; the send
button stays disabled during preflight rather than changing into Stop. Recheck
current eligibility after each wait, before consuming a snapshot. The draft's
`contextKey` changes when its editing destination changes, including a round
trip back to the same thread, but survives placeholder adoption. A stale gesture
cannot send, clear, or change the busy state of the newly opened draft.
Queue admission releases after draft-save preparation, before waiting for the
queue acknowledgement, so a distinct next message remains sendable. Each
admission owns its cleanup; a delayed old completion cannot release a newer one.
Uploads use the same context key and show a shared `Uploading…` status while
pending. A late upload from an abandoned context discards its attachment record,
even if the user has returned to the same thread.

A send awaits `waitForUploads()` before it snapshots `draft.attachments`
— dropping a file and pressing Enter is one gesture, and an upload still
in the air is not in the draft yet. Guarded on `uploading()` so the
common send stays synchronous.

A send starts `draft.prepareForSend()` before clearing locally and awaits that
captured write before admission. Dirty debounced edits must be persisted, or
a saved `a` would mismatch the sent `ab` and reappear later. Clean hydrated
drafts are never rewritten. The existing snapshot owner serializes writes per
thread, coalescing ordinary autosaves while preserving send preparation
boundaries; later typing cannot overtake the captured write or extend its wait.
Preparation failure restores the draft without sending or asking whether an
unissued message reached the agent.

## One send has one id, and a dead socket is not a verdict

`utils/sendOptions.ts#buildSendOptions` mints a `sendId` on every call,
and it is the ONLY place one is minted. Every outgoing path builds its
options there. It also sets `reconcileBySendId: true`, declaring the provisional
row ledger's capability; old servers ignore that additive field and old bundles
without it retain numeric direct IDs on new servers. Never infer this capability
from the existing idempotency `sendId`. Direct `SendMessageWithOptions`, queueing
`RegisterQueueItem`, and the Implement button in `utils/proposedPlanImplementation.ts`
all share that builder, so queued and dispatched messages carry the same fields.
Rule 7 in `lib/architecture.test.ts` is what keeps that true: a module
reaching either RPC has to build its options or take them already built.
One call is one send: a retry must re-send the options it already built
rather than rebuild them, which is what the transport's retained frame
does (`RETRY_ON_TRANSIENT_CLOSE` in `lib/transport/`).
The backend answers a repeat from the first arrival's record, so a
duplicated frame costs a duplicate answer and never a duplicate turn.

The direct composer path builds these options before its optimistic row,
uses `optimistic:<sendId>` as that row's temporary ID, and passes the SAME
options into `dispatchSend`. Never predict a canonical `user:<turn>` ID:
another client can own that turn, and stale idle state can cause the host
to accept the message into its active turn's queue instead. The pane retires
its placeholder only when a queue snapshot, flush acknowledgement, or user
item identifies that send. RPC success alone is not enough; response and
events can arrive in either order. Reconciliation walks only the optimistic
ledger, is thread-guarded, and cannot delete an already confirmed row.

That is what makes the ASK in `composerSend.ts` honest. A send whose
socket died after the transport's own retry also failed is genuinely
unknown — the frame may have reached the agent or may not — so
`dispatchSend` asks (`stores/unsentMessageConfirmation.svelte.ts`)
instead of silently putting text back that is already running. Answering
"Leave it" discards the snapshot and reports nothing further: the user
has just said they know, and an error banner underneath their own answer
is noise. Every OTHER failure, including a terminal disconnect, is a
definite "nothing happened" and restores exactly as it always did, with
no question. Keep that split — a question in front of a known failure
trains people to dismiss it.

## Rail visibility is one predicate

`activityRailHost.svelte.ts` owns the background-tasks controller, the
shared 1Hz clock, and the rail's visibility predicate. Visibility is
HOST-owned because it is load-bearing geometry: the composer mounts the
rail if and only if `railVisible`, and renders a transparent
height-reservation spacer as the exact complement, so exactly one of the
two holds the row at all times. That is what keeps the composer's measured
height, and the timeline padding it drives through `--composer-height`,
constant across turn start, turn completion and background-task end, so
the last message never jumps. Both branches flip in the same reactive
flush, so there is no one-frame double-height blip.

Do not re-derive "is the rail showing" anywhere else. The spacer once used
its own predicate without the background term and stacked a phantom second
row whenever a background task outlived its turn.

Call `createActivityRailHost` from component init (the clock uses runes),
`mount()` from `onMount`, and dispose its return value in `onDestroy`.

Background snapshots survive model/profile edits and failed list reads. Only a
successful snapshot or a switch to another thread can remove rows; a transport
error is not evidence that work stopped. The controller regression test covers
a model edit, failed refresh, and authoritative removal.

## Remote jobs share the background tray

`remote_command` rows are receipt projections from `ListLiveBackgroundTasks`,
not provider tools or timeline items. `trayRemoteJob` separates their ownership:
per-row Stop and Stop All call `CancelThreadRemoteCommand` with the source
thread and destination receipt, never provider terminal/task controls. Opening
one reads a bounded log tail lazily; closing/unmounting releases its text and
invalidates late reads. The tray snapshot rehydrates on its owning backend’s
connection edges and relevant transport gaps; retiring a connection invalidates
its pending read without clearing the last snapshot. No full-log hydration or
per-row polling. Completions
follow ordinary queue/chat presentation and the tray's existing retention.
A row names its computer once — the desktop's receipt already leads with the
profile name, so the frontend appends `· <display name>` only when the summary
does not — and holds Stop with `title="Offline"` while that computer's socket
is down. Both read the attached entry for `job.computerId`; a phone reading a
desktop's receipt knows no such entry and keeps the desktop's label and a live
Stop, which the desktop relays.

## The working indicator is stepped, not animated

`WorkingSprite.svelte` translates a horizontal strip PNG inside a
one-frame clipping window, stepped once per frame, with no timer and no
lifecycle JS. `transform` is compositable, so Blink runs it off the main
thread. Phase comes from `utils/ambientPhase.ts`, so a remount lands
mid-cycle on the same beat every other ambient indicator shares. Any new
indicator here follows the same shape.

## The workspace strip reads outer to inner

`workspace/ComposerWorkspaceStrip.svelte` is "where am I" for the draft:
machine, project, checkout, branch. `MachinePicker.svelte` leads it and
mounts only while `hasMultipleBackends()`, so a single-backend app has no
trace of it. The picker's label is the machine that owns the pane's
project; choosing another machine flips the draft to the SAME repository
there when the sidebar entry spans it (`projectSiblingOn`), otherwise asks
for that repository's checkout. Defaults and creation target that project
explicitly; successful switching remembers the frontend's choice. The project picker
beside it lists merged ENTRIES, one per repository, so a repo on two
machines is one project choice and one machine choice. Unreachable and view-only
machines stay listed, dimmed and disabled with their reason. The selection
handler rechecks reachability and `threads:operate`; browser-tool availability
does not belong in this execution-host choice. Never silently fail over.
The same reachability answer drives the composer's
disabled reason (`unreachableTarget` in `composerInputState.ts`) and the
dimmed sidebar row, all from `stores/attachedBackends.svelte.ts`.

An existing Claude/Codex conversation opens the shared Move/Copy dialog when
another capable computer is selected. It never redirects that conversation's
RPCs by changing the draft target. The transfer protocol and ownership epoch
decide when its new home becomes usable; see `components/transfers/AGENTS.md`.

Failed sends use `restoreUnsentDraftFor`: prepend submitted text to the latest
local draft (or the persisted draft after switching threads), preserving both.
Explicit editor replacement still uses `restoreDraftFor`, because edit/resend
already merged its recovery text. Both merges use `prependDraftSnapshot`, which
remaps image placeholders by attachment identity and retains terminal context.
A completed save may clear pending state only if its snapshot still matches.
