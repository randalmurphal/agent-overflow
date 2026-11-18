package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

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
	reply, mayHaveRun, err := t.app.callThreadPeerRequest(ctx, peer.Computer(), ThreadPeerCall{
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
	if err != nil && !mayHaveRun {
		// Nothing runs under the token. The error this returns is the
		// delivery, as it is for a request this computer refused itself
		// (failRequest), and the record keeps the same text: it names the
		// computer that refused or could not be reached.
		_, refusal, _ := errorsx.PublicDetails(err)
		t.refuseRequest(row.Token, refusal, nil)
		return threadtools.RequestAck{}, err
	}
	return t.dispatchedAck(ctx, caller, row, peer.Computer(), call, reply, err, waitSeconds, done)
}

// dispatchedAck is every outcome of a remote request the destination may
// have: the caller is given its token, never only an error, because an
// error would be answered by starting the same work again under a new one.
//
// sendErr is the last attempt's failure, nil when the destination answered
// with reply. A local failure to record that answer or to read the request
// back is said in the acknowledgement's notice, beside the state known so
// far; the request's row stays in the poll, which reconciles it.
func (t threadToolsApp) dispatchedAck(
	ctx context.Context, caller threadtools.Caller, row store.ThreadRequest, computer threadtools.Computer,
	call any, reply ThreadPeerRequest, sendErr error, waitSeconds int, done func(),
) (threadtools.RequestAck, error) {
	var failed error
	if sendErr == nil {
		if err := t.app.collectThreadPeerReply(row.Token, computer.ID, reply); err != nil {
			failed = fmt.Errorf("record the answer: %w", err)
		}
	}
	done()
	ack, err := t.ackRequest(ctx, caller, row.Token, waitSeconds)
	if err != nil {
		failed = errors.Join(failed, fmt.Errorf("read the request back: %w", err))
	}
	if failed == nil {
		if sendErr == nil {
			ack.Notice = t.unownedSpawnNotice(call, reply, computer.ID)
		}
		return ack, nil
	}
	reference := uuid.NewString()
	log.Printf("thread tools: request %s on %s (reference %s): %v", row.Token, computer.ID, reference, failed)
	known := threadtools.RequestAck{
		RequestState: threadtools.RequestState{
			Token: row.Token, Kind: row.Kind, ComputerID: computer.ID, Computer: computer.Name,
			Notify: row.Notify, State: store.ThreadRequestUnconfirmed,
		},
		Outcome: threadtools.OutcomeUnconfirmed,
		Notice: fmt.Sprintf("This computer could not record what %s answered (reference %s).",
			threadtools.NameOfComputer(computer), reference),
	}
	if sendErr == nil && reply.Known {
		known.ThreadID, known.Title, known.State = reply.TargetThreadID, reply.Title, reply.State
	}
	return known, nil
}

// unownedSpawnNotice says when an owned spawn's thread was not owned on the
// computer that accepted it: one that predates ownership across computers
// reports no placement, ignores the level that makes the spawn its
// caller's worker there, and gives the thread a sidebar row. An acceptance
// whose thread is still being made there names no thread, and so no
// placement, yet.
func (t threadToolsApp) unownedSpawnNotice(call any, reply ThreadPeerRequest, computerID string) string {
	spawn, ok := call.(threadtools.SpawnCall)
	if !ok || spawn.Level < 1 || reply.TargetThreadID == "" || reply.Placement != "" {
		return ""
	}
	name, _ := t.threadToolsBackendName(computerID)
	return threadtools.NameOfComputer(threadtools.Computer{ID: computerID, Name: name}) +
		" runs an older version, so the thread has a sidebar row there."
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

// callThreadPeerRequest forwards one request-minting call and returns the
// receipt the destination reports, or the last failure and whether any
// attempt may have started the request there.
//
// An attempt may have started it unless it was never sent
// (rpcclient.NotSent) or the destination answered it with a refusal, which
// it gives only for a request it did not start. Only an attempt that may
// have started it is tried again, with the same token, because what was
// lost may be the reply and not the work (replayThreadPeerRequest). A
// refusal of a later attempt says nothing about the work an earlier one may
// have started, so the request stays open for the poller.
func (a *App) callThreadPeerRequest(ctx context.Context, computer threadtools.Computer, call ThreadPeerCall) (ThreadPeerRequest, bool, error) {
	mayHaveRun := false
	for attempt := 1; ; attempt++ {
		rpc, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadToolCall"))
		var reply ThreadPeerReply
		var err error
		if mayHaveRun {
			reply, err = a.replayThreadPeerRequest(rpc, computer.ID, call)
		} else {
			err = a.backends.CallThreadPeer(rpc, computer.ID, "ThreadToolCall", &reply, call)
		}
		cancel()
		if err != nil {
			_, _, uncertain := threadErrorDetails(call.Tool, err)
			mayHaveRun = mayHaveRun || uncertain
			if uncertain && attempt < threadPeerAdmissionAttempts {
				continue
			}
			return ThreadPeerRequest{}, mayHaveRun, a.threadOperationError(call.Tool, computer.ID, "", err)
		}
		if reply.Request == nil {
			return ThreadPeerRequest{}, true, errorsx.Public(threadtools.CodeUnreachable,
				fmt.Sprintf("%s accepted %s without reporting the request.", threadtools.NameOfComputer(computer), call.Tool), nil)
		}
		return *reply.Request, true, nil
	}
}

// replayThreadPeerRequest sends a request again after an attempt that may
// have reached the destination.
//
// A destination on thread-tools.v2 answers a token it holds from its
// receipt, so the call is replayed as it is. One that predates it settles
// a receipt whose thread is still being made and lets that thread run
// anyway, so a spawn or ask, which make a thread, is not replayed there:
// its receipt is read and followed, and only an answer that the token is
// unknown sends the call again. A send makes nothing and is replayed to
// either.
func (a *App) replayThreadPeerRequest(ctx context.Context, computerID string, call ThreadPeerCall) (ThreadPeerReply, error) {
	var reply ThreadPeerReply
	if call.Tool == "thread_send" {
		err := a.backends.CallThreadPeer(ctx, computerID, "ThreadToolCall", &reply, call)
		return reply, err
	}
	err := a.backends.CallThreadPeerOn(ctx, computerID, transport.CapabilityThreadToolsV2, "ThreadToolCall", &reply, call)
	if !attachedbackends.Unsupported(err) {
		return reply, err
	}
	status, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadToolRequestStatus"))
	defer cancel()
	var poll ThreadPeerPollReply
	if err := a.backends.CallThreadPeer(status, computerID, "ThreadToolRequestStatus", &poll, ThreadPeerPoll{Tokens: []string{call.Token}}); err != nil {
		return ThreadPeerReply{}, err
	}
	i := slices.IndexFunc(poll.Requests, func(answer ThreadPeerRequest) bool { return answer.Token == call.Token })
	switch {
	case i < 0:
		return ThreadPeerReply{}, errors.New("the destination did not report the request")
	case poll.Requests[i].Known:
		return ThreadPeerReply{Request: &poll.Requests[i]}, nil
	}
	err = a.backends.CallThreadPeer(ctx, computerID, "ThreadToolCall", &reply, call)
	return reply, err
}
