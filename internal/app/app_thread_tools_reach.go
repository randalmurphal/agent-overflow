package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/rpcclient"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// Cross-computer reach for the agent thread tools: the paired computers
// the tool shapes are computed from, the client every forwarded call goes
// through, and the error shape a peer failure reaches the model in.
//
// Reach is pairing alone. The agent-commands opt-in guards a shell on the
// destination; nothing here runs one, and the destination authorizes every
// call itself against the authenticated device.

// threadPeerCallWait bounds one forwarded call: the destination method's
// declared budget and the answer margin (transport.ClientWait), so the
// destination's answer, a timeout of its own included, arrives first. A
// forwarded wait sits inside ThreadToolQuery's budget
// (threadtools.MaxForwardedWaitSeconds), and a spawn may fork and cut a
// worktree inside ThreadToolCall's.
func threadPeerCallWait(method string) time.Duration { return transport.ClientWait(method) }

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
	call, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadToolResolve"))
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
	call, cancel := context.WithTimeout(ctx, threadPeerCallWait(method))
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
// opening the RPC, so a first attempt that ends this way settles the source
// row `refused` (classifyThreadPeerFailure) instead of leaving it
// unconfirmed for a poller that would only collect the same answer.
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

// threadPeerAgent is what a forwarded request says of a standing agent
// sender.
func threadPeerAgent(agent *threadtools.SenderAgent) *ThreadPeerAgent {
	if agent == nil {
		return nil
	}
	return &ThreadPeerAgent{Role: agent.Role}
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
	// Admitted from the row to the destination's answer, so a rest of the
	// caller stops a request the destination has settled or holds.
	done, err := t.app.admitWork(caller.ThreadID, caller.Session)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	defer done()
	if err := t.app.store.InsertThreadRequest(row); err != nil {
		return threadtools.RequestAck{}, err
	}
	reply, failure, err := t.app.callThreadPeerRequest(ctx, peer.Computer(), ThreadPeerCall{
		Tool:        tool,
		Args:        args,
		Token:       row.Token,
		Source:      caller,
		UserMessage: latest,
		SourceAgent: threadPeerAgent(origin.agent),
		Inherit:     origin.inherit,
	})
	issue := ""
	if err != nil {
		_, issue, _ = threadErrorDetails(tool, err)
	}
	t.app.rescheduleThreadRequest(row, t.app.threadPollNormal(), issue)
	switch failure {
	case threadPeerRefused:
		// The error this returns is the delivery, as it is for a request
		// this computer refused itself (failRequest), and the record keeps
		// the same text: it names the computer that refused.
		_, refusal, _ := errorsx.PublicDetails(err)
		t.refuseRequest(row.Token, refusal, nil)
		return threadtools.RequestAck{}, err
	case threadPeerUnsettled:
		// The row stays `unconfirmed` for the poller, which reads the
		// destination's own record of the token.
		return threadtools.RequestAck{}, err
	case threadPeerUncertain:
		// Every attempt ended without an answer, so the destination may be
		// running this request. The model is given the token and told to
		// check it rather than an error it would answer by starting the
		// same work again; the poller reconciles the row either way.
		done()
		return t.ackRequest(ctx, caller, row.Token, waitSeconds)
	}
	if err := t.app.collectThreadPeerReply(row.Token, computerID, reply); err != nil {
		return threadtools.RequestAck{}, err
	}
	done()
	ack, err := t.ackRequest(ctx, caller, row.Token, waitSeconds)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	ack.Instructions = reply.Instructions
	return ack, nil
}

// threadPeerAdmissionAttempts is how many times one request-minting call is
// sent before the source stops waiting to hear whether it was accepted.
// Every attempt carries the same token, so a destination that already
// accepted answers with the acceptance it holds instead of starting a
// second piece of work.
const threadPeerAdmissionAttempts = 3

// threadRequestAdmissionFence keeps a freshly written source row out of the
// poller for as long as the attempts can take, plus the normal poll delay.
var threadRequestAdmissionFence = threadPeerCallWait("ThreadToolCall")*threadPeerAdmissionAttempts + threadPollNormalDelay

// threadPeerFailure is what a request-minting call that did not return an
// acceptance established about the request.
type threadPeerFailure int

const (
	// threadPeerAccepted: the call returned the destination's receipt.
	threadPeerAccepted threadPeerFailure = iota
	// threadPeerRefused: nothing runs under the token. The destination was
	// asked once and answered with a refusal of its own, or the call could
	// not reach it at all. The caller's error is the whole answer.
	threadPeerRefused
	// threadPeerUnsettled: the call failed with a known error, but the
	// destination may hold the request: an earlier attempt's reply was
	// lost, or it answered without a usable result. Its own record decides.
	threadPeerUnsettled
	// threadPeerUncertain: every attempt ended without an answer.
	threadPeerUncertain
)

// callThreadPeerRequest forwards one request-minting call and returns the
// receipt the destination reports.
//
// A failure that leaves the acceptance unknown is retried with the same
// token, because what was lost is the reply and not the work. A refusal
// settles the request only when it answers the first attempt: after a lost
// reply the destination may have accepted the request already, and a refusal
// of the retry (a switch turned off in between, a pairing that ended) says
// nothing about the work the first attempt started.
func (a *App) callThreadPeerRequest(ctx context.Context, computer threadtools.Computer, call ThreadPeerCall) (ThreadPeerRequest, threadPeerFailure, error) {
	for attempt := 1; ; attempt++ {
		rpc, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadToolCall"))
		var reply ThreadPeerReply
		err := a.backends.CallThreadPeer(rpc, computer.ID, "ThreadToolCall", &reply, call)
		cancel()
		if err != nil {
			failure, retry := classifyThreadPeerFailure(call.Tool, attempt, err)
			if retry {
				continue
			}
			return ThreadPeerRequest{}, failure, a.threadOperationError(call.Tool, computer.ID, "", err)
		}
		if reply.Request == nil {
			return ThreadPeerRequest{}, threadPeerUnsettled, errorsx.Public(threadtools.CodeUnreachable,
				fmt.Sprintf("%s accepted %s without reporting the request.", threadtools.NameOfComputer(computer), call.Tool), nil)
		}
		return *reply.Request, threadPeerAccepted, nil
	}
}

// classifyThreadPeerFailure decides what the attempt-th failure of one
// request-minting call established, and whether to send the token again.
func classifyThreadPeerFailure(tool string, attempt int, err error) (failure threadPeerFailure, retry bool) {
	code, _, uncertain := threadErrorDetails(tool, err)
	neverSent := threadRequestNeverSent(code)
	switch {
	case uncertain && !neverSent && attempt < threadPeerAdmissionAttempts:
		return threadPeerUncertain, true
	case attempt == 1 && (neverSent || threadPeerWroteRefusal(err)):
		return threadPeerRefused, false
	case uncertain && !neverSent:
		return threadPeerUncertain, false
	}
	return threadPeerUnsettled, false
}

// threadPeerWroteRefusal reports whether a failed call carries an answer the
// destination itself gave: its transport refused the pairing's credentials
// or scope, or its handler returned a thread tools refusal, which it does
// only for a request it did not start (a validation refusal before
// acceptance, or failRequest after it). A destination error this computer
// cannot read is uncertain and is retried before this is asked.
func threadPeerWroteRefusal(err error) bool {
	var remote *rpcclient.Error
	return errors.As(err, &remote)
}
