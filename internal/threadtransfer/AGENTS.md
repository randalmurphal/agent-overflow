# internal/threadtransfer/

Fixed conversation handoff coordination. Provider snapshot formats and app
authorization belong to their existing owners; this package only moves verified
bytes through the durable ownership protocol. No provider state/read model and
no general orchestration engine. See `docs/specs/conversation-transfer.md`.

- Source intent and recipient offers come from immutable/private journal fields.
  Archive paths are fixed under the injected operations root; a peer never
  supplies an arbitrary source path.
- Authorize the destination before the expensive snapshot. `SourceSnapshotter`
  runs on the source host after the peer offer is bound. It writes an immutable
  archive and its completion marker, then `BindThreadTransferArchive` seals the
  receipt. A retry after that point never reads current workspace content.
  Incoming offers initially need no archive hash; their first BeginUpload binds
  it once. Declared bytes are not prepared bytes. `archive_size` and
  `manifest_hash` survive file cleanup and answer completed status.
- Copy's sealed archive has independent AO/native identities. Release the
  original at sealing, including while upload/confirmation is pending; another
  copy or move may then start on the original. Move stays fenced until canceled
  with acknowledgment or permanently retired. Native copies remain fenced on
  their source through the native-closure journal.
- Retire the source durably before releasing activation proof. Unknown outcomes
  stay pending and are queried on the next run, including after app restart.
- Source cancellation intent is durable before contacting the destination.
  Keep execution fenced until it acknowledges that activation is impossible.
  The store's commit gate prevents cancellation racing the retirement write.
- One Source per app serializes operations and bounds concurrent buffers. Use
  app-lifetime contexts for accepted jobs, not frontend connection contexts.
- The app owns job startup/status adaptation and cleanup. `Run` makes progress
  without inventing a second durable state machine; `ErrPending` means an
  asynchronous prepare is still running, not an error to show the user.
- `Jobs` is the fixed host-lifetime retry loop: four active IDs, eight small SQL
  candidates, no private blobs in a queue scan and no idle polling. Incoming
  waits park until final upload/preparation/activation wakes them; every restart
  rechecks parked work. Outgoing status waits poll; failures back off to 64s.
  Wake and finish serialize so an active worker cannot overwrite a newer wake.
  Close stops dispatch before joining every worker. Install/snapshot callbacks
  must honor that lifecycle context. Infrastructure write failures also back off.
  Never call a runner synchronously from the destination's wake hook.
  Terminal rows with `cleanup_pending` also enter this bounded queue. Remove
  private archives, extracted snapshots and crash chunks only after confirmed
  completion or acknowledged cancellation, then clear that flag. A restart
  between terminal commit and removal repeats cleanup; SQL ownership and upload
  receipts remain, so completed status never depends on retained archive bytes.

Destination preparation validates all content before publishing `prepared`.
The private installation recipe (at most 16 MiB) is durable before that phase;
a prepared retry never recomputes destination file baselines. Activation proof
is durable before acknowledging acceptance, so restart can finish without the
phone or source online. Only the installer may publish ownership, through the
store's atomic history/completion transaction. Status reads never wait behind
validation/installation. App-lifetime jobs own these long operations; HTTP
control calls only accept them. Preparation touches inert operation scratch,
never native provider files or the live workspace.

Cancellation also checks that no durable activation proof has been accepted,
even while SQL still says prepared. An accepted proof cannot be revoked by a
late cancel. App installers implement `DestinationDiscarder`: release their
inert worktree/branch reservations before acknowledging cancellation, then drop
the private upload. A failed cleanup stays retryable from the source's durable
cancel intent. Published workspace identities are never preparation cleanup.

An authorized frontend may discard an UNPREPARED destination offer even when
the source never bound its reply. Once prepared, only cancellation proof from
the source can release it. Source status checks precede snapshot work, so a
discarded offer also releases a source whose snapshot cannot be prepared.
The optional `needsAttention` status bit makes destination job failures visible
at the source while retaining the ordinary bounded retry schedule.

The app reserves destination projects at offer creation, before native files or
workspace recipes exist. A user deleting a project cannot invalidate a prepared
promise after the source has retired. History restore is refused during pending
transfers or when it would discard a currently owned incoming identity.

Preparation failures caused by corrupt uploaded bytes reset only an unprepared
checkpoint against the immutable SQL digest. Invalid content with the correct
digest remains a visible refusal, not an endless silent retransmission.
