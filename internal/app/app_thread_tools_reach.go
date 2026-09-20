package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Cross-computer reach for the agent thread tools: the paired computers
// the tool shapes are computed from, the client every forwarded call goes
// through, and the error shape a peer failure reaches the model in.
//
// Reach is pairing alone. The agent-commands opt-in guards a shell on the
// destination; nothing here runs one, and the destination authorizes every
// call itself against the authenticated device.

// threadPeerCallTimeout bounds one forwarded call. A read of a long thread
// renders a page on the destination and a write starts a turn there, so it
// sits well above a round trip and well below the tool call ceiling.
const threadPeerCallTimeout = 60 * time.Second

// PairedComputers lists the computers this one is paired with, never
// including this computer. An empty list is the single-computer shape, in
// which no schema carries computer_id and no result row carries a
// computer field.
func (t threadToolsApp) PairedComputers(context.Context) ([]threadtools.Computer, error) {
	if t.app == nil || t.app.backends == nil {
		return nil, nil
	}
	rows, err := t.app.backends.List()
	if err != nil {
		return nil, err
	}
	self, _ := t.app.backendIdentity()
	out := make([]threadtools.Computer, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" || row.ID == self {
			continue
		}
		out = append(out, threadtools.Computer{ID: row.ID, Name: attachedComputerName(row)})
	}
	return out, nil
}

// attachedComputerName is what this computer calls a paired one: the
// nickname the user gave it, else the name that computer calls itself.
func attachedComputerName(row attachedbackends.Attached) string {
	if name := strings.Join(strings.Fields(row.Nickname), " "); name != "" {
		return name
	}
	return strings.Join(strings.Fields(row.Name), " ")
}

// Peer returns a client for one paired computer. An unknown or unpaired
// id is a public refusal, because it reaches the model as an errors row.
// One pairing is read, not the whole list: a fan-out asks for every peer.
func (t threadToolsApp) Peer(ctx context.Context, computerID string) (threadtools.Peer, error) {
	row, paired, err := t.pairing(computerID)
	if err != nil {
		return nil, errorsx.Public(threadtools.CodeUnreachable,
			"This computer could not read its pairings. Check Remote access on it and try again.", err)
	}
	if !paired {
		return nil, errorsx.Public(threadtools.CodeUnreachable,
			fmt.Sprintf("Computer %s is not paired with this computer. thread_options lists the computers it can reach.", computerID), nil)
	}
	source, _ := threadtools.CallerFrom(ctx)
	computer := threadtools.Computer{ID: row.ID, Name: attachedComputerName(row)}
	return threadPeerClient{app: t.app, computer: computer, source: source}, nil
}

// pairing reads one pairing by the backend id that computer calls itself
// by. This computer's own id is never a pairing.
func (t threadToolsApp) pairing(backendID string) (attachedbackends.Attached, bool, error) {
	if backendID == "" || t.app == nil || t.app.backends == nil {
		return attachedbackends.Attached{}, false, nil
	}
	if self, _ := t.app.backendIdentity(); backendID == self {
		return attachedbackends.Attached{}, false, nil
	}
	return t.app.backends.Lookup(backendID)
}

// threadPeerClient is one paired computer as threadtools reaches it. Every
// method is one authenticated RPC on the pairing's own rotating
// credential, bounded by its own timeout so one unreachable computer costs
// the fan-out one slot rather than the whole call.
type threadPeerClient struct {
	app      *App
	computer threadtools.Computer
	// source is the calling thread as this computer names it. The
	// destination records it for display and attribution; it authorizes
	// nothing, which the authenticated device does.
	source threadtools.Caller
}

func (p threadPeerClient) Computer() threadtools.Computer { return p.computer }

func (p threadPeerClient) Resolve(ctx context.Context, ref string) (threadtools.Resolution, error) {
	call, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
	defer cancel()
	var answer threadtools.Resolution
	if err := p.app.backends.CallThreadPeer(call, p.computer.ID, "ThreadToolResolve", &answer,
		ThreadPeerResolve{Ref: ref, Source: p.source}); err != nil {
		return threadtools.Resolution{}, p.app.threadOperationError("resolve", p.computer.ID, ref, err)
	}
	return answer, nil
}

func (p threadPeerClient) Query(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return p.forward(ctx, "ThreadToolQuery", name, args)
}

func (p threadPeerClient) Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return p.forward(ctx, "ThreadToolCall", name, args)
}

func (p threadPeerClient) forward(ctx context.Context, method, name string, args json.RawMessage) (json.RawMessage, error) {
	call, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
	defer cancel()
	var reply ThreadPeerReply
	err := p.app.backends.CallThreadPeer(call, p.computer.ID, method, &reply,
		ThreadPeerCall{Tool: name, Args: args, Source: p.source})
	if err != nil {
		return nil, p.app.threadOperationError(name, p.computer.ID, "", err)
	}
	if len(reply.Result) == 0 {
		return nil, errorsx.Public(threadtools.CodeUnreachable,
			fmt.Sprintf("%s answered %s with no result.", threadtools.NameOfComputer(p.computer), name), nil)
	}
	return reply.Result, nil
}

// FetchExport copies a file the destination rendered into this computer's
// own export directory, so the path the model reads is one it can open.
func (p threadPeerClient) FetchExport(ctx context.Context, file threadtools.ExportFile) (threadtools.ExportFile, error) {
	return p.app.fetchThreadExport(ctx, p.computer, file)
}

// threadOperationError names the operation, the computer and the thread a
// refusal addressed, in the remote-commands shape: a stable code, prose a
// model can act on, and the ids beside the names so a retry can carry
// them. Wrapping is idempotent; raw causes stay in the host log behind a
// reference id.
func (a *App) threadOperationError(action, computerID, threadID string, err error) error {
	var done remoteContextError
	if err == nil || errors.As(err, &done) {
		return err
	}
	code, message, _ := threadErrorDetails(action, err)
	// Verbs read "Thread cancel on X"; a tool name already names the
	// thread, so it reads "thread_spawn on X".
	prefix := "Thread " + action
	if strings.HasPrefix(action, "thread_") {
		prefix = action
	}
	if entityid.Valid(computerID) {
		if name := a.remoteComputerNames()[computerID]; name != "" {
			prefix += " on " + name + " (computer " + computerID + ")"
		} else {
			prefix += " on computer " + computerID
		}
	}
	if threadID != "" {
		prefix += " for thread " + threadID
	}
	return remoteContextError{errorsx.Public(code, fmt.Sprintf("%s: %s", prefix, message), err)}
}

// threadErrorDetails classifies one peer failure. A refusal the
// destination wrote with a thread tools code is reviewed public prose
// already and crosses unchanged, and it is certain: the destination
// answered. Everything else goes through the remote classification, so
// pairing, connection and timeout failures read the same on both
// surfaces, and uncertain means the destination may have acted.
func threadErrorDetails(action string, err error) (code, message string, uncertain bool) {
	var remote *rpcclient.Error
	if errors.As(err, &remote) && strings.HasPrefix(remote.Code, "thread_") {
		return remote.Code, remote.Message, false
	}
	if public, text, ok := errorsx.PublicDetails(err); ok && strings.HasPrefix(public, "thread_") {
		return public, text, false
	}
	return remoteErrorDetails(action, err)
}

// threadRequestNeverSent reports whether a dispatch failure happened
// before the destination could see the request. These are the refusals
// attachedbackends raises while addressing the call, loading the pairing or
// opening the RPC, so the source row is settled `refused`
// (settleUnconfirmedRequest) instead of left unconfirmed for a poller that
// would only collect the same answer.
//
// threadtools.CodeUnreachable is deliberately NOT in the set, close as it
// reads: it is also what this computer raises for a destination that DID
// answer, without a usable result (threadPeerClient.forward,
// callThreadPeerRequest). Those requests may be running on the other
// computer, and settling one refused would tell the caller nothing happened
// while the work continues.
func threadRequestNeverSent(code string) bool {
	switch code {
	case attachedbackends.CodeThreadUnsupported, attachedbackends.CodeThreadUnreachable,
		"remote_not_paired", "remote_pairing_pending", "remote_pairing_unavailable",
		"remote_pairing_expired":
		return true
	}
	return false
}

// threadPairingEnded reports the refusals that mean this pairing will not
// answer again until the user reconnects it. An open request against such
// a computer is settled rather than retried forever.
func threadPairingEnded(code string) bool {
	switch code {
	case "remote_not_paired", "remote_pairing_expired":
		return true
	}
	return false
}

// startRemoteRequest is the source half of a spawn, send or ask on another
// computer.
//
// The row is written `unconfirmed` first, so a reply lost on the way back
// leaves a token the poller can ask about rather than work nobody records.
// The destination owns everything the row cannot say: which thread ran, and
// what it answered.
func (t threadToolsApp) startRemoteRequest(
	ctx context.Context, caller threadtools.Caller, row store.ThreadRequest,
	computerID, tool string, call any, waitSeconds int,
) (threadtools.RequestAck, error) {
	origin, err := t.localOrigin(caller)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	peer, err := t.Peer(ctx, computerID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	args, err := json.Marshal(call)
	if err != nil {
		return threadtools.RequestAck{}, fmt.Errorf("thread tools: encode %s for %s: %w", tool, computerID, err)
	}
	latest, _, err := t.app.store.LatestHumanUserText(caller.ThreadID)
	if err != nil {
		return threadtools.RequestAck{}, err
	}

	row.Token = newThreadRequestToken()
	row.CallerThreadID = caller.ThreadID
	row.TargetComputerID = computerID
	row.State = store.ThreadRequestUnconfirmed
	// The poll is the recovery path for a reply that never arrives, and it
	// must not run while this call is still being attempted: a destination
	// that has not been asked yet answers `unknown`, which for an
	// unconfirmed row means refused. The fence covers the whole attempt
	// sequence and is cut to the normal delay the moment the call returns,
	// which is what makes "no retry is in flight" durable.
	row.NextCheck = time.Now().Add(threadRequestAdmissionFence).UnixMilli()
	if err := t.app.store.InsertThreadRequest(row); err != nil {
		return threadtools.RequestAck{}, err
	}
	reply, uncertain, err := t.app.callThreadPeerRequest(ctx, peer.Computer(), ThreadPeerCall{
		Tool:        tool,
		Args:        args,
		Token:       row.Token,
		Source:      caller,
		UserMessage: latest,
		Inherit:     origin.inherit,
	})
	issue := ""
	if err != nil {
		_, issue, _ = threadErrorDetails(tool, err)
	}
	t.app.rescheduleThreadRequest(row, threadPollNormalDelay, issue)
	if err != nil {
		if !uncertain {
			return threadtools.RequestAck{}, t.settleUnconfirmedRequest(row.Token, err)
		}
		// Every attempt ended without an answer, so the destination may be
		// running this request. The model is given the token and told to
		// check it rather than an error it would answer by starting the
		// same work again; the poller reconciles the row either way.
		return t.ackRequest(ctx, caller, row.Token, waitSeconds)
	}
	if _, err := t.app.applyThreadPeerRequest(row.Token, computerID, reply); err != nil {
		return threadtools.RequestAck{}, err
	}
	return t.ackRequest(ctx, caller, row.Token, waitSeconds)
}

// threadPeerAdmissionAttempts is how many times one request-minting call is
// sent before the source stops waiting to hear whether it was accepted.
// Every attempt carries the same token, so a destination that already
// accepted answers with the acceptance it holds instead of starting a
// second piece of work.
const threadPeerAdmissionAttempts = 3

// threadRequestAdmissionFence keeps a freshly written source row out of the
// poller for as long as the attempts can take, plus the normal poll delay.
const threadRequestAdmissionFence = threadPeerCallTimeout*threadPeerAdmissionAttempts + threadPollNormalDelay

// callThreadPeerRequest forwards one request-minting call and returns the
// receipt the destination reports.
//
// A failure that leaves the acceptance unknown is retried with the same
// token, because what was lost is the reply and not the work. `uncertain`
// is true when every attempt ended that way: the destination may be running
// the request, so the row stays open for the poller rather than being
// refused here.
func (a *App) callThreadPeerRequest(ctx context.Context, computer threadtools.Computer, call ThreadPeerCall) (ThreadPeerRequest, bool, error) {
	for attempt := 1; ; attempt++ {
		rpc, cancel := context.WithTimeout(ctx, threadPeerCallTimeout)
		var reply ThreadPeerReply
		err := a.backends.CallThreadPeer(rpc, computer.ID, "ThreadToolCall", &reply, call)
		cancel()
		if err != nil {
			code, _, uncertain := threadErrorDetails(call.Tool, err)
			if uncertain && !threadRequestNeverSent(code) && attempt < threadPeerAdmissionAttempts {
				continue
			}
			return ThreadPeerRequest{}, uncertain, a.threadOperationError(call.Tool, computer.ID, "", err)
		}
		if reply.Request == nil {
			return ThreadPeerRequest{}, false, errorsx.Public(threadtools.CodeUnreachable,
				fmt.Sprintf("%s accepted %s without reporting the request.", threadtools.NameOfComputer(computer), call.Tool), nil)
		}
		return *reply.Request, false, nil
	}
}

// settleUnconfirmedRequest ends a remote request whose call failed with an
// answer the destination gave, and returns the original error.
//
// A refusal raised before the destination could see the call is `refused`:
// nothing ran there, and the row would otherwise be polled forever against
// a computer that already said why. A refusal the destination itself wrote
// is left `unconfirmed` for the poller, which reads that computer's own
// record of the token rather than trusting one failed call. A call whose
// answer never came back does not reach here at all: it is unconfirmed to
// the model, with the token.
func (t threadToolsApp) settleUnconfirmedRequest(token string, cause error) error {
	code, message, _ := threadErrorDetails("request", cause)
	if !threadRequestNeverSent(code) {
		return cause
	}
	if _, err := t.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestRefused,
		Answer:     []byte(message),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		log.Printf("thread tools: settle unsent request %s: %v", token, err)
	}
	return cause
}
