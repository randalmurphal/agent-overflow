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
`composerUploads.svelte.ts`, which carries a per-thread guard so a slow
upload cannot land in the wrong pane. The bytes themselves go over HTTP,
not the RPC wire: `uploadAttachmentBytes`
(`lib/transport/attachmentTransfer.ts`) mints a single-use ticket for
exactly this file and PUTs the `File` as the body, so a 10 MiB paste is
never a base64 string in a WebSocket frame. `compressImageToFit` still
runs first and is unchanged — a re-encode that fits beats a rejection,
whatever carries the result.

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
picker take any file. `AttachmentPicker` exposes Files and Photos on Android;
the shared input surface owns the file inputs and calls the same `uploadFiles`
path as drop/paste. Capture the pane generation when opening the system picker
and discard a selection returned after a thread switch. Keep scope/provider/
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

A send awaits `waitForUploads()` before it snapshots `draft.attachments`
— dropping a file and pressing Enter is one gesture, and an upload still
in the air is not in the draft yet. Guarded on `uploading()` so the
common send stays synchronous.

A send also awaits `draft.quiesceSaves()` before the RPC that consumes
the draft row (`SendMessageWithOptions`, `RegisterQueueItem`). The
backend runs one connection's RPCs concurrently, so a debounced
`SaveDraft` still on the wire can land AFTER the send's delete and put
the sent text back in the row for every screen to re-read. The wait
cancels the pending timer and joins the saves already issued; on the
direct path it runs under `sending`, on the queue path after the
synchronous local clear, so a second Enter during it has nothing to send.

## One send has one id, and a dead socket is not a verdict

`utils/sendOptions.ts#buildSendOptions` mints a `sendId` on every call,
and it is the ONLY place one is minted. Every outgoing path builds its
options there — the direct `SendMessageWithOptions`, the queueing
`RegisterQueueItem`, and `utils/proposedPlanImplementation.ts`'s Implement
button — so a message that queues carries the id on the same terms as one
that dispatches, and no call site can ship without one by forgetting.
Rule 7 in `lib/architecture.test.ts` is what keeps that true: a module
reaching either RPC has to build its options or take them already built.
One call is one send: a retry must re-send the options it already built
rather than rebuild them, which is what the transport's retained frame
does (`RETRY_ON_TRANSIENT_CLOSE` in `lib/transport/`).
The backend answers a repeat from the first arrival's record, so a
duplicated frame costs a duplicate answer and never a duplicate turn.

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

## The working indicator is stepped, not animated

`WorkingSprite.svelte` translates a horizontal strip PNG inside a
one-frame clipping window, stepped once per frame, with no timer and no
lifecycle JS. `transform` is compositable, so Blink runs it off the main
thread. The previous inline `background-position-x` write from a
wall-clock timer was the single most expensive thing in the renderer:
163.0ms of main-thread work per 5s at 25 frames/s, against 0.0ms now
(2026-08-23). Layer-promoting the old write still cost 95.4ms. Phase comes
from `utils/ambientPhase.ts`, so a remount lands mid-cycle on the same
beat every other ambient indicator shares. Any new indicator here follows
the same shape.

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
