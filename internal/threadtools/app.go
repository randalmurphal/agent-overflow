package threadtools

import (
	"context"
	"encoding/json"
)

// App is everything this package needs from the application that owns the
// threads it reads and writes. internal/app implements it in-process. A
// thread on another computer is not reached through this interface at all:
// the Peer half below forwards the call to the same Server running on that
// computer, which is why every type here is provider-neutral and
// JSON-friendly.
//
// Contract for every implementation:
//   - Reads name a thread that lives on THIS computer. A thread that does
//     not exist here returns an error carrying code thread_not_found.
//   - Writes take the Caller so the request ledger, the attribution chip
//     and the footer all name the same source thread.
//   - Nothing here renders text for the model. Rendering, budgets, cursors
//     and refusal prose belong to this package.
type App interface {
	// Thread returns one thread's row, including the derived columns the
	// state derivation needs (see State).
	Thread(ctx context.Context, threadID string) (Thread, error)

	// LiveState returns the live projection for one thread. A thread with
	// no live provider session returns the zero value and no error.
	LiveState(ctx context.Context, threadID string) (LiveState, error)

	// ResolveThreadRef matches a full thread id or a prefix of at least
	// MinPrefixLen characters against this computer's threads, bounded to
	// MaxResolutionCandidates rows. Hidden workflow threads are included;
	// scratch threads are excluded unless the caller owns them. A thread
	// this computer moved away is reported through Resolution.MovedTo
	// instead of as a miss. No match is an empty Resolution, not an error.
	ResolveThreadRef(ctx context.Context, ref string) (Resolution, error)

	// ResolveWindow turns a requested window into an absolute timeline
	// position range over the thread as it stands now. The App counts
	// turns because a turn's rows do not all fit in one rendered page.
	// A non-empty window must come back with From, To and HighWater all
	// set to real positions; a window with no items sets Empty instead.
	// There is no open-ended range and no zero that means "to the end".
	ResolveWindow(ctx context.Context, q WindowQuery) (WindowBounds, error)

	// Transcript returns one page of a thread's items in timeline order.
	// It must never return more than Limit rows, and must clip a single
	// item's body to MaxItemBytes while still reporting its whole size.
	Transcript(ctx context.Context, q TranscriptQuery) (TranscriptSlice, error)

	// ItemPayload reads a byte range of one item's stored payload and
	// reports the payload's whole size. An offset at or past the end is an
	// empty read with the size, never an error; the range reader in this
	// package streams a large item through repeated calls rather than
	// pulling it whole.
	ItemPayload(ctx context.Context, q PayloadQuery) (Payload, error)

	// SearchThreads answers one computer's half of thread_search: a ranked
	// page when Query is set, a listing by last activity when it is not.
	SearchThreads(ctx context.Context, q SearchQuery) (SearchPage, error)

	// ExportTranscript renders a whole window to a file under this
	// computer's export directory and returns its path, size and digest.
	// The file is retained until it is explicitly removed.
	ExportTranscript(ctx context.Context, q ExportQuery) (ExportFile, error)

	// ExportAnswer writes one request's whole settled answer to the same
	// directory. thread_status with to_file uses it.
	ExportAnswer(ctx context.Context, caller Caller, token string) (ExportFile, error)

	// Catalog returns what a spawn can choose from on this computer.
	Catalog(ctx context.Context, q CatalogQuery) (Catalog, error)

	// Spawn creates a visible thread, sends the prompt as its first user
	// message, starts the turn and returns the request receipt. It mints
	// the token, writes the request row and dispatches locally or to the
	// destination computer.
	Spawn(ctx context.Context, caller Caller, call SpawnCall) (RequestAck, error)

	// Send continues an existing thread as if the user had typed the
	// message: queued at the turn boundary mid-turn, sent now when idle.
	Send(ctx context.Context, caller Caller, call SendCall) (RequestAck, error)

	// Ask forks the target at its tail into a hidden read-only scratch
	// thread, sends the question there and returns the request receipt.
	Ask(ctx context.Context, caller Caller, call AskCall) (RequestAck, error)

	// Reply settles one request this thread was asked. The token must name
	// a receipt whose target is the caller's own thread.
	Reply(ctx context.Context, caller Caller, call ReplyCall) (ReplyAck, error)

	// RequestStates reads, and optionally waits on, the caller's requests
	// by token, or threads of this computer by id. A watch on another
	// computer's thread is forwarded to it as its own thread_status call.
	RequestStates(ctx context.Context, caller Caller, call StatusCall) (StatusReport, error)

	// ListRequests lists the caller's own requests, open first then
	// newest first.
	ListRequests(ctx context.Context, caller Caller, call ListCall) (RequestListing, error)

	// Cancel cancels one request by token, or interrupts a thread the
	// caller spawned, sent to or asked.
	Cancel(ctx context.Context, caller Caller, call CancelCall) (CancelReport, error)

	// Remind arms a clock-settled request that wakes the caller later.
	Remind(ctx context.Context, caller Caller, call RemindCall) (RequestAck, error)

	// UpdateThreads applies one organize patch to threads on this
	// computer. The whole patch is validated per thread before any thread
	// is touched, and a refusal names the thread and the reason without
	// stopping the other ids in the call.
	UpdateThreads(ctx context.Context, caller Caller, call UpdateCall) (UpdateReport, error)

	// UpdateGroup renames, deletes or pins one group of this computer.
	UpdateGroup(ctx context.Context, caller Caller, call GroupCall) (GroupReport, error)

	// PairedComputers lists the computers this one is paired with, never
	// including this computer. An empty list is the single-computer shape.
	PairedComputers(ctx context.Context) ([]Computer, error)

	// Peer returns a client for one paired computer. An unknown or
	// unreachable computer returns an error; the error text reaches the
	// model as an errors row or a refusal, so it must be public prose.
	Peer(ctx context.Context, computerID string) (Peer, error)
}

// Peer is one paired computer, reached through the peer methods. The two
// call methods carry the wire's scope split: Query is threads:read, Invoke
// is terminal:operate.
//
// A peer runs the same handler code against its own app and answers in its
// own shape, so a row it returns carries no computer fields. The caller
// stamps its own view of that computer onto every row it forwards, which is
// what keeps grouping correct when the destination has no pairings of its
// own.
type Peer interface {
	// Computer identifies this peer as the caller's pairing profile names
	// it. People read the name, the model reads the id.
	Computer() Computer

	// Resolve answers a thread reference on that computer. It is typed
	// rather than a tool call so an ambiguity across computers is
	// comparable without decoding a rendered result.
	Resolve(ctx context.Context, ref string) (Resolution, error)

	// Query runs one read tool there and returns its JSON result
	// unchanged: thread_search, thread_show, thread_item, thread_options.
	Query(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

	// Invoke runs one write tool there and returns its JSON result
	// unchanged: thread_update and thread_group.
	Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

	// FetchExport copies a file that peer rendered onto the calling
	// computer and returns it with a path the caller's model can open. A
	// path on the destination is of no use to a model that cannot read
	// that filesystem, so `to_file` on another computer's thread always
	// goes through here.
	FetchExport(ctx context.Context, file ExportFile) (ExportFile, error)
}
