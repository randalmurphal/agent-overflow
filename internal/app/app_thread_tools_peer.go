package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The destination half of cross-computer thread tools: the five bound
// methods a paired computer reaches, and the shapes they carry.
//
// They serve whatever this computer's own thread tools switch says. The
// switch means "agents on this computer get the tools"; pairing already
// says which computers belong to the user, and a paired computer's agents
// reach this one either way (docs/specs/agent-thread-tools.md,
// "Availability and permissions").
//
// Authorization is the authenticated device of the calling computer, the
// same principal remote jobs use, plus the method's own scope. A receipt
// records that device, so a second device of the same computer cannot
// redeem another device's token.
//
// Every one of them stamps threadtools.WithForwarded on the context. A
// forwarded call runs on this computer alone: it sees no paired computers
// of its own, so a fan-out, a resolution or a search never crosses back to
// the computer that asked, and an export writes a file this computer can
// serve in chunks rather than a path the reader cannot open.

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
	// Inherit is what a forwarded spawn takes from the calling thread where
	// the call overrides nothing. It travels because the calling thread's
	// row is on the other computer; this computer still validates every
	// value against its own catalogs.
	Inherit threadtools.SpawnDefaults `json:"inherit,omitzero"`
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
	Token          string `json:"token"`
	Known          bool   `json:"known"`
	Kind           string `json:"kind,omitempty"`
	TargetThreadID string `json:"targetThreadId,omitempty"`
	Title          string `json:"title,omitempty"`
	State          string `json:"state,omitempty"`
	Answer         string `json:"answer,omitempty"`
	AnswerKind     string `json:"answerKind,omitempty"`
	// LateReply is a thread_reply that arrived after the receipt had
	// already finished. It is a later revision than Answer.
	LateReply   string `json:"lateReply,omitempty"`
	LateReplyAt int64  `json:"lateReplyAt,omitempty"`
	Revision    int64  `json:"revision,omitempty"`
	SettledAt   int64  `json:"settledAt,omitempty"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
	// Blocked is whether the target is waiting on a person right now. It
	// is a live property, never stored, and it is what lets a wait on a
	// remote request end on blocked the way a local one does.
	Blocked bool `json:"blocked,omitempty"`
}

// ThreadPeerPoll is one source's whole outstanding set for this computer:
// the tokens it wants the state of, and the settlements it has durably
// stored since it last asked.
type ThreadPeerPoll struct {
	Tokens []string        `json:"tokens,omitempty"`
	Ack    []ThreadPeerAck `json:"ack,omitempty"`
}

// ThreadPeerAck is one collected settlement. The destination may drop an
// answer only once its latest revision has been acknowledged.
type ThreadPeerAck struct {
	Token    string `json:"token"`
	Revision int64  `json:"revision"`
}

// ThreadPeerPollReply answers one poll, one entry per requested token.
type ThreadPeerPollReply struct {
	Requests []ThreadPeerRequest `json:"requests"`
}

// threadPeerTokenLimit bounds one poll. The source batches its due rows,
// and a batch larger than this is a defect rather than work.
const threadPeerTokenLimit = 256

// ThreadToolResolve answers one thread reference against this computer's
// threads. It is typed rather than a tool call so the caller can compare
// an ambiguity across computers without decoding rendered text.
//
//ao:scope threads:read
//ao:route selected
func (a *App) ThreadToolResolve(ctx context.Context, prefix string) (threadtools.Resolution, error) {
	ctx = threadtools.WithForwarded(ctx)
	if _, err := a.remoteCommandOwner(ctx); err != nil {
		return threadtools.Resolution{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "resolve a thread"); err != nil {
		return threadtools.Resolution{}, err
	}
	return a.threadToolsAdapter().ResolveThreadRef(ctx, prefix)
}

// ThreadToolQuery runs one read tool here and returns its JSON result
// unchanged. The caller stamps its own view of this computer onto the
// rows, which is what keeps grouping correct when this computer has no
// pairings of its own.
//
//ao:scope threads:read
//ao:route selected
func (a *App) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	ctx = threadtools.WithForwarded(ctx)
	if _, err := a.remoteCommandOwner(ctx); err != nil {
		return ThreadPeerReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "read a thread"); err != nil {
		return ThreadPeerReply{}, err
	}
	switch call.Tool {
	case "thread_search", "thread_show", "thread_item", "thread_options", "thread_status":
	default:
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("%s is not a thread tools read call.", call.Tool), nil)
	}
	return a.runThreadPeerTool(ctx, call)
}

// ThreadToolCall runs one write tool here. spawn, send, ask and cancel
// carry the source's token and mint or settle a receipt; thread_update
// and thread_group are ordinary tool calls with no request behind them.
//
//ao:scope terminal:operate
//ao:route selected
func (a *App) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	ctx = threadtools.WithForwarded(ctx)
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeTerminalOperate, "start work in a thread"); err != nil {
		return ThreadPeerReply{}, err
	}
	switch call.Tool {
	case "thread_update", "thread_group":
		return a.runThreadPeerTool(ctx, call)
	case "thread_spawn", "thread_send", "thread_ask", "thread_cancel":
		return a.runThreadPeerRequest(ctx, owner, call)
	}
	return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
		fmt.Sprintf("%s is not a thread tools write call.", call.Tool), nil)
}

// ThreadToolRequestStatus answers one source's outstanding set and records
// what it has collected. It is one call per source per tick, so twenty
// open asks on this computer cost one round trip rather than twenty.
//
//ao:scope threads:read
//ao:route selected
func (a *App) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	ctx = threadtools.WithForwarded(ctx)
	owner, err := a.remoteCommandOwner(ctx)
	if err != nil {
		return ThreadPeerPollReply{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "read a thread request"); err != nil {
		return ThreadPeerPollReply{}, err
	}
	if len(poll.Tokens) > threadPeerTokenLimit || len(poll.Ack) > threadPeerTokenLimit {
		return ThreadPeerPollReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			fmt.Sprintf("A request poll carries at most %d tokens.", threadPeerTokenLimit), nil)
	}
	for _, ack := range poll.Ack {
		receipt, found, err := a.store.GetThreadRequestReceipt(ack.Token)
		if err != nil {
			return ThreadPeerPollReply{}, err
		}
		// A token this device does not own is not acknowledged and not
		// reported as an error: the source learns nothing it did not
		// already know, and nothing here is written for it.
		if !found || receipt.OwnerDeviceID != owner {
			continue
		}
		if _, err := a.store.AckThreadRequestReceipt(ack.Token, ack.Revision, 0); err != nil {
			return ThreadPeerPollReply{}, err
		}
	}
	reply := ThreadPeerPollReply{Requests: make([]ThreadPeerRequest, 0, len(poll.Tokens))}
	for _, token := range poll.Tokens {
		state, err := a.threadPeerRequestState(ctx, owner, token)
		if err != nil {
			return ThreadPeerPollReply{}, err
		}
		reply.Requests = append(reply.Requests, state)
	}
	return reply, nil
}

// ThreadToolExportChunk serves one rendered transcript in transfer-sized
// pieces, under the same stamp rule a command artifact uses: a file that
// changed mid-transfer is refused rather than delivered half old.
//
//ao:scope threads:read
//ao:route selected
func (a *App) ThreadToolExportChunk(ctx context.Context, exportID string, offset int64) (RemoteArtifactChunk, error) {
	ctx = threadtools.WithForwarded(ctx)
	if _, err := a.remoteCommandOwner(ctx); err != nil {
		return RemoteArtifactChunk{}, err
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "read a thread export"); err != nil {
		return RemoteArtifactChunk{}, err
	}
	return a.readThreadExportChunk(ctx, exportID, offset)
}

// runThreadPeerTool runs one forwarded tool call against this computer's
// own threadtools server.
//
// The session's caller names THIS computer, because its shape, its
// resolution and the computer it stamps on a local row are all this
// computer's. The source's own identity travels separately, on the calls
// that write it into a receipt or a message footer.
func (a *App) runThreadPeerTool(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	caller, err := a.threadPeerCaller(call)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := a.threadToolsServer().Call(ctx, caller, call.Tool, args)
	if err != nil {
		return ThreadPeerReply{}, a.publicThreadToolError(call.Tool, caller.ThreadID, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ThreadPeerReply{}, fmt.Errorf("thread tools: encode %s result: %w", call.Tool, err)
	}
	return ThreadPeerReply{Result: encoded}, nil
}

// threadPeerCaller names the calling thread for a forwarded read or
// organize call: the source's thread, this computer's identity.
func (a *App) threadPeerCaller(call ThreadPeerCall) (threadtools.Caller, error) {
	if !entityid.Valid(strings.TrimSpace(call.Source.ThreadID)) {
		return threadtools.Caller{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded thread tools call must name the thread it came from.", nil)
	}
	backendID, _ := a.backendIdentity()
	return threadtools.Caller{
		ThreadID:     strings.TrimSpace(call.Source.ThreadID),
		Title:        threadPeerTitle(call.Source.Title),
		ComputerID:   backendID,
		ComputerName: a.backendDisplayName(),
	}, nil
}

// threadPeerTitle clips a title another computer chose to the bound this
// computer's own tools enforce. It is rendered into the trusted "Agent
// request" footer of a user message, so its length and its line count are
// this computer's business, not the sender's.
func threadPeerTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if utf8.RuneCountInString(title) <= threadtools.MaxTitleRunes {
		return title
	}
	count := 0
	for index := range title {
		if count == threadtools.MaxTitleRunes {
			return title[:index]
		}
		count++
	}
	return title
}

// refuseForeignLedgerRead refuses a call that reads the CALLER's own request
// ledger when the caller is another computer's thread.
//
// A forwarded call names a thread on the computer it came from, and this
// computer answers it with its own rows: those two meanings of "the caller's
// thread" are the same only for a local call. A forwarded `thread_status`
// with tokens or with no arguments would read whatever local thread the
// sender named, so it is refused here rather than answered. The thread_ids
// branch is the one that is legitimately forwarded, and it reads threads
// rather than the ledger.
func refuseForeignLedgerRead(ctx context.Context, what string) error {
	if !threadtools.Forwarded(ctx) {
		return nil
	}
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"A request record belongs to the computer whose thread made it, so "+what+
			" cannot be read from another computer. Call thread_status on your own computer.", nil)
}

// publicThreadToolError keeps an unreviewed cause out of the wire. The
// code and prose reach the calling model on the other computer, so an
// internal failure is logged here and answered with a stable refusal.
func (a *App) publicThreadToolError(tool, threadID string, err error) error {
	if _, _, public := errorsx.PublicDetails(err); public {
		return err
	}
	logThreadRequestSweep("serve "+tool+" for "+threadID, err)
	return errorsx.Public(threadtools.CodeInvalidRequest,
		"Thread tools could not complete that call on this computer.", err)
}

// runThreadPeerRequest accepts, or settles, one request a paired computer
// minted. The token is the source's, so a retry after a lost reply reads
// the existing receipt back instead of starting a second piece of work.
func (a *App) runThreadPeerRequest(ctx context.Context, owner string, call ThreadPeerCall) (ThreadPeerReply, error) {
	token := strings.TrimSpace(call.Token)
	// Every one of these is written into a receipt and rendered into the
	// trusted "Agent request" footer of a user message on this computer.
	// They are ids this app mints, so they are checked for the shape this
	// app mints them in rather than for being non-empty.
	if !entityid.Valid(token) {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded request must carry the token its source minted.", nil)
	}
	source := call.Source
	source.ThreadID = strings.TrimSpace(source.ThreadID)
	source.ComputerID = strings.TrimSpace(source.ComputerID)
	if !entityid.Valid(source.ThreadID) || !entityid.Valid(source.ComputerID) {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded request must name the thread and computer it came from.", nil)
	}
	source.Title = threadPeerTitle(source.Title)
	origin := threadRequestOrigin{
		caller:      source,
		foreign:     true,
		ownerDevice: owner,
		userMessage: call.UserMessage,
		inherit:     call.Inherit,
	}
	if call.Tool == "thread_cancel" {
		return a.cancelPeerThreadRequest(ctx, origin, token, call)
	}
	if err := a.refuseForeignToken(token, owner); err != nil {
		return ThreadPeerReply{}, err
	}
	adapter := threadToolsApp{app: a}
	var err error
	switch call.Tool {
	case "thread_spawn":
		var spawn threadtools.SpawnCall
		if err = json.Unmarshal(call.Args, &spawn); err == nil {
			spawn.ComputerID = ""
			_, err = adapter.acceptSpawn(ctx, origin, token, spawn)
		}
	case "thread_send":
		var send threadtools.SendCall
		if err = json.Unmarshal(call.Args, &send); err == nil {
			send.ComputerID = ""
			_, err = adapter.acceptSend(ctx, origin, token, send)
		}
	case "thread_ask":
		var ask threadtools.AskCall
		if err = json.Unmarshal(call.Args, &ask); err == nil {
			ask.ComputerID = ""
			_, err = adapter.acceptAsk(ctx, origin, token, ask)
		}
	}
	if err != nil {
		return ThreadPeerReply{}, a.publicThreadToolError(call.Tool, call.Source.ThreadID, err)
	}
	state, err := a.threadPeerRequestState(ctx, owner, token)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	return ThreadPeerReply{Request: &state}, nil
}

// refuseForeignToken rejects a token this device does not own before any
// work starts. A receipt belongs to the authenticated device that created
// it: another device of the same computer holds no claim on it, and
// reusing its token would hand that device another device's answer.
func (a *App) refuseForeignToken(token, owner string) error {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return err
	}
	if found && receipt.OwnerDeviceID != owner {
		return errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a request another device made.", nil)
	}
	// A token that already names a SOURCE row here belongs to a request one
	// of this computer's own threads made. A forwarded call carries the
	// source's token, so its token is never one of ours: accepting it would
	// let a paired computer answer, or refuse, this computer's own request.
	if _, exists, err := a.store.GetThreadRequest(token); err != nil {
		return err
	} else if exists {
		return errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a request this computer made.", nil)
	}
	return nil
}

// cancelPeerThreadRequest is the destination half of thread_cancel: stop
// one request by its token, or interrupt a thread this source started
// here.
func (a *App) cancelPeerThreadRequest(ctx context.Context, origin threadRequestOrigin, token string, call ThreadPeerCall) (ThreadPeerReply, error) {
	var cancel threadtools.CancelCall
	if len(call.Args) > 0 {
		if err := json.Unmarshal(call.Args, &cancel); err != nil {
			return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
				"That cancel could not be read on this computer.", err)
		}
	}
	if cancel.ThreadID != "" {
		report, err := threadToolsApp{app: a}.interruptForeignThread(ctx, origin, cancel.ThreadID)
		if err != nil {
			return ThreadPeerReply{}, a.publicThreadToolError("thread_cancel", origin.caller.ThreadID, err)
		}
		return ThreadPeerReply{Effect: report}, nil
	}
	if err := a.refuseForeignToken(token, origin.ownerDevice); err != nil {
		return ThreadPeerReply{}, err
	}
	effect, err := a.stopThreadRequestWork(ctx, token)
	if err != nil {
		return ThreadPeerReply{}, a.publicThreadToolError("thread_cancel", origin.caller.ThreadID, err)
	}
	state, err := a.threadPeerRequestState(ctx, origin.ownerDevice, token)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	return ThreadPeerReply{Request: &state, Effect: effect}, nil
}

// threadPeerRequestState reads one receipt for the device that owns it.
// A token this device never created is reported unknown rather than
// refused: the source settles it, and saying which other device holds it
// would leak that device's work.
func (a *App) threadPeerRequestState(ctx context.Context, owner, token string) (ThreadPeerRequest, error) {
	state := ThreadPeerRequest{Token: token}
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return ThreadPeerRequest{}, err
	}
	if !found || receipt.OwnerDeviceID != owner {
		return state, nil
	}
	state.Known = true
	state.Kind = receipt.Kind
	state.TargetThreadID = receipt.TargetThreadID
	state.State = receipt.State
	state.Answer = string(receipt.Answer)
	state.AnswerKind = receipt.AnswerKind
	state.LateReply = string(receipt.LateReply)
	state.LateReplyAt = receipt.LateReplyAt
	state.Revision = receipt.Revision
	state.SettledAt = receipt.SettledAt
	state.ExpiresAt = receipt.ExpiresAt
	if receipt.TargetThreadID != "" {
		if thread, err := a.store.GetThread(receipt.TargetThreadID); err == nil {
			state.Title = thread.Title
		}
		if receipt.State == store.ThreadReceiptAccepted || receipt.State == store.ThreadReceiptRunning {
			live, err := a.threadToolsAdapter().LiveState(ctx, receipt.TargetThreadID)
			state.Blocked = err == nil && live.Blocked()
		}
	}
	return state, nil
}
