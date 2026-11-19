package app

import (
	"encoding/json"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// The shapes the destination half of cross-computer thread tools carries
// (app_thread_tools_peer.go): a forwarded call and its reply, a request as
// the computer that accepted it reports it, and a source's poll.

// ThreadPeerCall is one tool call forwarded from a paired computer.
type ThreadPeerCall struct {
	// Tool is the tool name, in the same spelling the model uses.
	Tool string `json:"tool"`
	// Args are the tool's own arguments for a read or an organize call,
	// and the marshalled SpawnCall, SendCall, AskCall or CancelCall for a
	// call that mints a request.
	Args json.RawMessage `json:"args,omitempty"`
	// Token is the source's minted request token for spawn, send, ask and
	// cancel. It is the idempotency key of the receipt, so a retry after a
	// lost reply returns the existing acceptance rather than starting the
	// work a second time.
	Token string `json:"token,omitempty"`
	// Source is the calling thread as its own computer names it. It is
	// display and attribution only; the receipt's authorization input is
	// the authenticated device.
	Source threadtools.Caller `json:"source"`
	// UserMessage is the source thread's latest message no agent wrote,
	// which the request footer quotes and this computer cannot read.
	UserMessage string `json:"userMessage,omitempty"`
	// SourceAgent is set when the source thread is a standing agent, with
	// the role line the footer quotes. A computer that does not send it
	// is read as an ordinary thread.
	SourceAgent *ThreadPeerAgent `json:"sourceAgent,omitempty"`
	// Inherit is what a forwarded spawn takes from the calling thread where
	// the call overrides nothing. It travels because the calling thread's
	// row is on the other computer; this computer still validates every
	// value against its own catalogs.
	Inherit threadtools.SpawnDefaults `json:"inherit,omitzero"`
	// UserStop marks a thread_cancel a person made from the source thread's
	// Agents tray rather than the source's agent: the receipt settles saying
	// so, and a turn it interrupts names the user. A computer that does not
	// read it words it as the sender's cancel.
	UserStop bool `json:"userStop,omitempty"`
}

// ThreadPeerCause is an interruption's cause on the wire: its code and
// argument (threadtools.Cause) and what the turn had written by then.
type ThreadPeerCause struct {
	Code    string `json:"code"`
	Arg     string `json:"arg,omitempty"`
	Written string `json:"written,omitempty"`
}

// ThreadPeerAgent is a forwarded request's standing agent sender.
type ThreadPeerAgent struct {
	Role string `json:"role,omitempty"`
}

// ThreadPeerReply is one forwarded call's answer.
type ThreadPeerReply struct {
	// Request is the receipt a spawn, send, ask or cancel now owns here.
	Request *ThreadPeerRequest `json:"request,omitempty"`
	// Result is the tool's own JSON for a read or an organize call,
	// returned unchanged for the caller to stamp its own view onto.
	Result json.RawMessage `json:"result,omitempty"`
	// Effect is what a cancel stopped, in the tool's own vocabulary.
	Effect string `json:"effect,omitempty"`
}

// ThreadPeerRequest is one request as the computer that accepted it
// reports it: the receipt's state, and once settled its whole answer.
//
// Known separates a receipt this computer holds from a token it has never
// seen or has swept past the retention floor, which the source settles
// differently.
type ThreadPeerRequest struct {
	Token string `json:"token"`
	Known bool   `json:"known"`
	// Admitting marks, in a status answer, a token with no receipt here that
	// a spawn, send or ask handler is still running for, and so may still
	// admit (peerAdmissions). A computer on thread-tools.v2 sends it; from
	// one, an unknown token without it was never started there.
	Admitting      bool   `json:"admitting,omitempty"`
	Kind           string `json:"kind,omitempty"`
	TargetThreadID string `json:"targetThreadId,omitempty"`
	Title          string `json:"title,omitempty"`
	// Provider is the target thread's provider, for the caller's Agents
	// row; a computer that does not send it leaves the row without one.
	Provider   string `json:"provider,omitempty"`
	State      string `json:"state,omitempty"`
	Answer     string `json:"answer,omitempty"`
	AnswerKind string `json:"answerKind,omitempty"`
	// Cause is why an `interrupted` answer was interrupted, as this
	// computer recorded it. Answer beside it is the same in this computer's
	// words, which a computer that does not read Cause, or does not know its
	// code, shows whole.
	Cause *ThreadPeerCause `json:"cause,omitempty"`
	// LateReply is a thread_reply that arrived after the receipt had
	// already finished. It is a later revision than Answer.
	LateReply   string `json:"lateReply,omitempty"`
	LateReplyAt int64  `json:"lateReplyAt,omitempty"`
	Revision    int64  `json:"revision,omitempty"`
	// CollectedRevision is the revision the source last acknowledged. A
	// source holding Revision sends its acknowledgement again while this is
	// behind it.
	CollectedRevision int64 `json:"collectedRevision,omitempty"`
	SettledAt         int64 `json:"settledAt,omitempty"`
	ExpiresAt         int64 `json:"expiresAt,omitempty"`
	// Blocked is whether the target is waiting on a person right now. It
	// is a live property, never stored, and it is what lets a wait on a
	// remote request end on blocked the way a local one does.
	Blocked bool `json:"blocked,omitempty"`
	// BlockedRoutedTo names the thread on that computer the target's prompt
	// went to instead of the person (a worker's prompt routed to its
	// spawner), as the Agents row there names it. Empty when the person
	// owns the prompt, and from a computer that does not send it.
	BlockedRoutedTo string `json:"blockedRoutedTo,omitempty"`
	// BlockedRoutedToCaller is set when that prompt went back to the
	// request's caller, the thread whose link owns the target there
	// (routedToAsker), not a thread on that computer.
	BlockedRoutedToCaller bool `json:"blockedRoutedToCaller,omitempty"`
	// BlockedRoutedToThread names the asking computer's thread that prompt
	// went back to, whichever of its threads made the request: the
	// spawner, which a send from another thread names. A computer that
	// does not send it says only BlockedRoutedToCaller.
	BlockedRoutedToThread string `json:"blockedRoutedToThread,omitempty"`
	// Placement is the target thread's placement there: owned, grouped or
	// standalone. A computer that predates ownership across computers
	// sends none, and its spawn's thread has a sidebar row there.
	Placement string `json:"placement,omitempty"`
	// WaitingOn is what the target still waits on while its request waits
	// for its rest: a spawn, or a send from the thread that spawned it,
	// whose turn ended with work of its own left (restSettlement). Live,
	// never stored.
	WaitingOn string `json:"waitingOn,omitempty"`
	// RestRule is set by a computer that settles such a request at its
	// thread's rest. One that does not send it settles it when its turn
	// ends and cannot report rest: its `finished` answer is collected with
	// store.ThreadReceiptAnswer.RestUnreported.
	RestRule bool `json:"restRule,omitempty"`
	// AnswerAgent is set on a settled request whose thread was a standing
	// agent when the answer was written, with the role line its wake quotes.
	// A computer that does not send it is read as having answered from an
	// ordinary thread.
	AnswerAgent *ThreadPeerAgent `json:"answerAgent,omitempty"`
	// LateReplyAgent is AnswerAgent for the late reply, as it was when the
	// late reply was written.
	LateReplyAgent *ThreadPeerAgent `json:"lateReplyAgent,omitempty"`
	// Instructions are what a spawn's thread was given for its system
	// prompt, as its receipt recorded them when it named the thread. Every
	// answer about the spawn states them, so the calling computer records
	// them from the first one that reaches it, a poll included.
	Instructions string `json:"instructions,omitempty"`
}

// ThreadPeerPoll is one source's whole outstanding set for this computer:
// the tokens it wants the state of, and the settlements it has durably
// stored since it last asked.
type ThreadPeerPoll struct {
	// Launch fences owner reads and acknowledgments to the serving process.
	Launch string `json:"launch,omitempty"`

	// Links requests current state for the named spawns.
	Links []OwnerLinkRead `json:"links,omitempty"`

	// Contacts proves continuing reach without retrieving a listing.
	Contacts []OwnerLinkRead `json:"contacts,omitempty"`

	// StructuralAck names the exact spawn edits the source has applied.
	StructuralAck []store.SpawnRevisionAck `json:"structuralAck,omitempty"`

	// Tokens requests the current status of each receipt.
	Tokens []string `json:"tokens,omitempty"`

	// Ack confirms receipt settlement revisions.
	Ack []ThreadPeerAck `json:"ack,omitempty"`
	// Held names the revision the source already holds of settled tokens it
	// asks about, which includes the settlement: the answer about one leaves
	// out its Answer and Cause, and its LateReply too when the source holds
	// the receipt's revision. A source that does not send it is answered in
	// full.
	Held []ThreadPeerAck `json:"held,omitempty"`
}

// ThreadPeerAck names a request and the revision of its receipt the source
// has durably recorded. When the destination may drop that answer is the
// hold rule's (docs/specs/agent-thread-tools.md, "Wakes from another
// computer").
type ThreadPeerAck struct {
	Token    string `json:"token"`
	Revision int64  `json:"revision"`
}

// ThreadPeerPollReply answers one poll, one entry per requested token.
type ThreadPeerPollReply struct {
	// Links follows the order of the requested links.
	Links []OwnerLinkState `json:"links,omitempty"`

	// Requests follows the order of the requested tokens.
	Requests []ThreadPeerRequest `json:"requests"`
}

// threadPeerTokenLimit bounds one poll. The source batches its due rows,
// and a batch larger than this is a defect rather than work.
const threadPeerTokenLimit = 256

// ThreadPeerResolve is one forwarded thread reference.
type ThreadPeerResolve struct {
	// Ref is the id, or id prefix, to resolve.
	Ref string `json:"ref"`
	// Source is the calling thread as its own computer names it. It
	// decides what the lookup may see: a scratch fork an ask made here is
	// visible to the thread that asked and to nothing else, and that thread
	// is on the other computer.
	Source threadtools.Caller `json:"source"`
}
