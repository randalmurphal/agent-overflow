# Conversation transfer

Implementation plan for the approved [connected-computers](connected-computers.md)
scope. The transfer operation and UI are implemented; the complete connected
computer acceptance matrix and remaining integration work are tracked below.

Implemented: durable coordination
records (schema v86); strict streaming archives; Claude transcript/sidecar
collection; Codex current-path and compressed/prefix collection; bounded AO
history export/import with attachment and review-reference remapping; atomic
incoming history activation; restartable file installation; the bounded HTTP
handoff contract/client; source and destination coordination (restart, cancellation,
lost acknowledgments, durable activation proof, and smaller chunks after slow-link
timeouts); host-lifetime job recovery and snapshot hooks; portable Git workspace
capture/registered worktree preparation and no-overwrite publication; independent
native graph copies; and ownership epochs in catalogs, search and frontend routing. The app now wires source/destination APIs, snapshotting, host jobs,
prepared installation and the HTTP adapter into production bootstrap. An
isolated two-host TLS app test covers Copy, Move, frontend disconnect, staged vs
working bytes, and returning a continued Claude conversation to its retired
source. Complete mutation fencing and the remaining edge cases
below are still in progress; this is not yet the complete delivery gate.

## Native history is portable

The September 5 isolated probes used the installed Codex **0.153.4** and Claude
Code **2.1.261**, temporary provider homes and workspaces, and an OS sandbox
denying external network access and reads of the developer's provider homes.
Neither used live credentials or paid model calls.

- Codex: copy a synthetic native rollout to another home; `thread/read` finds
  it by its existing ID, and `thread/resume` preserves history with a different
  `cwd`. `thread/fork` creates an independent native ID. No model turn is needed
  for these operations. The tested legacy fork physically copied root history
  but retained ORIGINAL child IDs. The Go graph-copy implementation creates
  independent root/child IDs and remaps structured links; both copied sessions
  resume in a fresh home. Metadata-only graph discovery avoids thread loading
  and checks durable native queues before reading.
  Current paginated sessions use separate conversation/rollout filename IDs after
  reverting. The Go transfer materializes retained `history_base` chains into
  standalone paginated files: native resume alone retains model context but does
  not reconstruct prefix SQLite history projections in a fresh home. Go-produced
  Move and Copy exports both rebuilt the native turn list, resumed with retained
  context, excluded reverted content, and successfully reverted an old turn.
- Claude: create a session against a local mock Messages endpoint, copy its
  JSONL to another home, then resume from a different workspace. The next model
  request contains the original history and keeps the original session ID.
  `--resume … --fork-session --session-id …` creates a copy with the requested
  independent ID and the same history. Current full-history forks preserve
  message UUIDs. The Go copy implementation also preserves message UUIDs and
  child IDs, which are scoped by the NEW root directory. A real child created
  against the local mock resumed via `SendMessage` from the Go-produced copy.
  Root and sidecars must relocate together to the new workspace slug.

Provider contracts: [Codex resume/fork](https://learn.chatgpt.com/docs/app-server#start-or-resume-a-thread),
[Claude cross-host resume](https://code.claude.com/docs/en/agent-sdk/sessions#resume-across-hosts),
and [Claude session sidecars](https://code.claude.com/docs/en/agent-sdk/session-storage).
Codex's [current wire types](https://github.com/openai/codex/blob/ddf04ad26789d040f9ef6a96736f76602e35a6cc/codex-rs/app-server-protocol/src/protocol/v2/thread.rs)
also expose experimental path-based resume/fork. Prefer native ID discovery
where possible. A rollout with `history_base` depends on another rollout;
transferring only its leaf is insufficient. Unsupported history modes must
produce a specific refusal before source ownership changes.

## Contents and boundaries

A transfer contains native resumable files, AO conversation metadata and history,
attachment metadata/bytes, and the workspace changes selected by the user.
History rendering caches can be recomputed. Credentials, provider accounts,
host settings, active processes and approval handles never travel with a thread.
The destination chooses its own account and execution configuration. Before it
acknowledges preparation, it checks the selected provider binary and fresh native
account availability. A failed check keeps the source unretired; fixing the
account and retrying finishes the same accepted operation. Claude credential
checks share the managed rotation transaction. Codex reads `account/read` without
refreshing credentials or starting a thread, including custom endpoints that do
not require an OpenAI login. Native-format admission derives its floor from every graph member: legacy
Codex files use AO's normal CLI floor, and paginated history requires 0.148.0.
Unknown history modes refuse before preparation. An isolated 0.153.4 → 0.148.0
Go-export probe preserved Move/Copy history, model context and old-turn reverts;
matching source and destination CLI versions is not required.

Validate repository identity and destination workspace before mutation. Preserve
historical text; change only structured workspace/file references understood by
the owning provider adapter. Snapshot at a quiescent boundary under the existing
thread-operation lock. An idle provider process must relinquish the session
before a move can change execution ownership.

Archives stream with bounded buffers and explicit file/total limits. Extraction
refuses absolute paths, traversal, duplicate members, and escaping links. A
destination stages and validates before writing into its provider home or
workspace. Existing divergent native files are a conflict, never an overwrite.

## Workspace snapshot evidence

The September 5 isolated Git 2.52.0 probe confirmed that a non-thin object pack
can carry HEAD history and staged blobs while excluding commits already present
on the destination. Portable stage-zero index records preserve staged content
and deletions independently of working edits and untracked files. The probe
preserved distinct staged/unstaged binary content, an unpublished commit, and
both staged and unstaged deletions without changing source refs.

`internal/git/transfer_objects.go` implements bounded object streaming and index
validation/restoration. Workspace capture and inert reconstruction now preserve
intent-to-add, skip-worktree/assume-unchanged flags, symlinks, binary working
edits and file/directory replacements. The source is checked again after archive
creation to reject concurrent edits. Ignored files stay local; submodules and
unborn repositories currently refuse workspace transfer explicitly.
Preparation now registers a locked worktree in the destination repository. Its
HEAD and index retain staged-only objects through `git gc --prune=now`, verified
in an isolated Git probe. Publication uses an atomic no-overwrite rename plus
`git worktree repair`; tests cover restart between those steps. The private
operation marker proves retry ownership, including after unlocking. The source's Git index file is never
portable input; platform/stat/split-index details stay local. Primary contracts:
[pack-objects](https://git-scm.com/docs/git-pack-objects) and
[update-index](https://git-scm.com/docs/git-update-index).

## One execution owner

Move keeps the AO thread ID. Copy allocates a new AO/native identity and records
provenance so imported historical usage is not counted as new spending.
The action presents both choices explicitly: **Move** changes computers;
**Copy / fork** leaves the original usable and creates an independent continuation
on the destination. Copy is not a destructive move followed by a local restore.
Once its independent archive is sealed, the original is usable even while the
copy is uploading or awaiting confirmation. Another handoff may start then too.

The operation has a durable ID. Destination preparation is inert. For a move,
the source durably records its retirement before releasing an activation secret;
the destination knows only that secret's hash until then. Activation verifies
the secret and is idempotent. The source keeps recovery data until confirmation.
No timeout is permission to reopen execution on the source.

Every AO path that can drive a retired thread must return its new owner. Retired
source rows stay out of ordinary catalogs. After the destination confirms the
move, ordinary project/worktree cleanup may discard the old local cache without
restarting it or erasing execution/native retirement. Before confirmation,
cleanup stays fenced. A subsequent return reconstructs the cache if needed. A stale frontend learns the move and
routes to an explicitly attached destination; it never falls back to another
computer. A lost activation reply is resolved by querying the durable operation,
not by creating another conversation. Cancellation after retirement requires a
destination acknowledgment that it cannot activate; an unknown outcome remains
recoverable pending work.

Cancellation starts on the source. Its intent is durable before contacting the
destination and prevents a later source retirement, including after restart.
Destination cancellation requires the source secret as well as the offer grant;
a frontend holding the offer alone cannot independently race source retirement.
The source remains fenced until the destination acknowledges cancellation. A
canceled destination can never activate the same operation. Lost replies are
resolved by durable status, not by treating the request as undelivered.

A conversation returning to a former host replaces only its retired cached
history. Keep its parent row and existing uploads so other local forks retain
their references. The replacement and completion share a transaction; both
history counters advance past the old cache to invalidate frontend replicas.

## Catalog ownership across computers

Move keeps an AO ID, so offline catalogs can temporarily contain the same ID on
both its former and current computers. Routing cannot select whichever catalog
answered first. The implementation must carry a monotonically increasing
ownership epoch through a move, keep source retirement authoritative, and prefer
the newer attested owner when merging catalogs. An incoming transfer must not
replace a later ownership epoch on a former host. Equal-epoch contradictory
owners are an explicit conflict, never a fallback to an arbitrary socket.

Ordinary threads begin at epoch zero; each move advances it. Copies receive a
new AO ID and their own ownership history. Catalog admission, search results, row events and the thread routing index now
compare these epochs. Ownership changes invalidate cached thread history, move
live subscriptions, and refresh mounted panes without dropping their local
composer. The full application operation and stale-execution fences remain in
progress.

## Phones are not required to remain connected

A frontend may be paired independently to A and B while A has no general session
on B. Do not export either device's credentials or assume a fully connected mesh.
The destination can issue a narrowly scoped transfer offer for one operation;
the source streams directly using that offer. Its endpoint and certificate trust
come from the frontend's authenticated destination, and the operation binds the
source/destination identities and manifest. Closing the initiating frontend must
not cancel an accepted transfer. UI and optional agent tools use the same APIs.

The implemented destination offer can precede source snapshotting. Its first
upload binds the content identity once; source snapshotting runs in a host job
after the destination offer is bound. The fixed job loop keeps at most four
active operations, loads small SQL candidate pages, parks incoming waits without
polling, and rechecks them on restart. Final upload and activation wake the job.
The app wires provider/workspace callbacks and API acceptance to this lifecycle.
An unfinished authorization handshake appears as Finish setup and is recoverable
or cancelable from either the dialog or computer transfer details.

## Required failure evidence

Test both providers with a relocated checkout and attachments; copy and move;
duplicate/reordered requests; refusal before preparation; disconnect before and
after source retirement; lost activation reply; source/destination restart;
destination conflict; and stale clients attempting to send on the old owner.
Tests use mocked providers and the real two-backend transport. The external
portability probes are throwaway evidence, not an automatic real-provider test.

## Current integration follow-through

- The shared Move/Copy dialog, computer status/recovery controls and capability
  gating are implemented. Real two-host desktop/compact tests copy a conversation,
  then move the original to that same destination with native/workspace identity
  checks. Finish the ownership-aware sidebar/pane reconciliation audit.
- Lazy/pinned Claude forks materialize independent native sessions during either
  Move or Copy, keeping the parent usable. Pinned prefixes use the existing native
  resume-filter repair and exclude later parent turns; isolated CLI 2.1.261
  resume verified that behavior. Current Codex paginated Move/Copy exports now
  preserve native history editing as well as context. The mocked real-process
  reader checks every child's queue without loading any provider thread.
- Ordinary edits, drafts, uploads, queue admission, comments, terminal creation,
  forks, configuration and deletion now check the shared ownership fence. Pending
  destination offers reserve projects; history restore preserves received owners.
  Finish the remaining native-alias, scheduled-work and bulk workspace audit.
- Unprepared orphan offers can now be discarded on the destination; the source
  observes that durable cancellation before attempting a snapshot. Terminal
  operations remove private archives through restartable host jobs; compact SQL
  receipts and ownership remain. Finish address roaming,
  mixed-version/provider-format admission, and updates
  while transfers are pending. Verify supervisor rollback never rewinds ownership.
- Expanded Git blobs and final working deltas are bounded before materialization;
  source conversions travel as bytes, and destination checkout/intent creation
  runs without destination filters. The prepared recipe binds a content and
  semantic-index fingerprint; activation refuses changed files/index flags while
  allowing innocent index stat refreshes, including retries after publication.
- Native installation only replaces recorded baselines for sessions previously
  retired by this computer. Test that policy across Codex prefixes/children too.
- Extend two-host fault coverage and visual/native-shell validation, then finish
  all connected-computers delivery gates. Provider primitives passing is not a
  substitute for those end-user checks.

Execution ownership uses a stronger SQLite commit policy than the history
cache: EXTRA/FULL plus fullfsync on the reserved writer connection, restored after
the transaction. WAL+NORMAL may rewind committed transactions after a power
loss; [SQLite documents the distinction](https://sqlite.org/pragma.html#pragma_synchronous).
Unit tests check connection exclusivity and restoration, not physical power-cut
behavior. Native files, archives and operation markers have their own file and
directory flush boundaries.
