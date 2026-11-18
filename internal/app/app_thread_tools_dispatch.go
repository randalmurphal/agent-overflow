package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

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
	// The poll is the recovery path for a reply that never arrives. It stays
	// away while the call is out, whose frame the destination may not have
	// read yet, and an unknown answer settles the row as never started only
	// once no attempt can still be admitted (threadRequestNeverStarted).
	row.AdmitUntil = time.Now().Add(threadRequestAdmissionWindow).UnixMilli()
	row.NextCheck = row.AdmitUntil
	peer, done, err := t.admitRemoteRequest(ctx, caller, computerID, row)
	if err != nil {
		return threadtools.RequestAck{}, err
	}
	defer done()
	read := t.app.ownerReads.start(ctx, computerID, []string{row.Token})
	defer read.close()
	reply, sent, err := t.app.callThreadPeerRequest(ctx, peer.Computer(), ThreadPeerCall{
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
	// Whatever came back, the poll asks next at its own pace: a receipt the
	// destination holds is collected at once.
	t.app.rescheduleThreadRequest(row, t.app.threadPollNormal(), issue)
	if err != nil && sent.IsZero() {
		// Nothing runs under the token. The error this returns is the
		// delivery, as it is for a request this computer refused itself
		// (failRequest), and the record keeps the same text: it names the
		// computer that refused or could not be reached.
		_, refusal, _ := errorsx.PublicDetails(err)
		t.refuseRequest(row.Token, refusal, nil)
		return threadtools.RequestAck{}, err
	}
	return t.dispatchedAck(ctx, caller, row, peer.Computer(), call, reply, read, err, waitSeconds, done)
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
	call any, reply peerDispatchReply, read ownerRead, sendErr error, waitSeconds int, done func(),
) (threadtools.RequestAck, error) {
	var failed error
	if sendErr == nil {
		err := t.app.recordDispatchStatus(computer.ID, row.Token, read, reply)
		if errors.Is(err, errReadSuperseded) || errors.Is(err, errPeerReadInvalidated) {
			ctx, cancel := context.WithTimeout(ctx, threadPollCallTimeout)
			for (errors.Is(err, errReadSuperseded) || errors.Is(err, errPeerReadInvalidated)) && ctx.Err() == nil {
				err = t.app.rereadDispatchStatus(ctx, computer.ID, row.Token)
			}
			cancel()
		}
		if err != nil {
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
			ack.Notice = t.unownedSpawnNotice(call, reply.ThreadPeerRequest, computer.ID)
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

// A notification can supersede dispatch before the owner has read the new
// target. Read its receipt before returning the acknowledgment to the caller.
func (a *App) rereadDispatchStatus(ctx context.Context, computer, token string) error {
	read := a.ownerReads.start(ctx, computer, []string{token})
	defer read.close()
	var reply ThreadPeerPollReply
	origin, err := a.backends.ReadThreadPeer(ctx, computer, transport.CapabilityThreadTools, "", "ThreadToolRequestStatus", &reply, ThreadPeerPoll{Tokens: []string{token}})
	if err != nil {
		return err
	}
	if len(reply.Requests) != 1 || reply.Requests[0].Token != token || !reply.Requests[0].Known {
		return fmt.Errorf("peer did not confirm the dispatched request %s", token)
	}
	return a.recordDispatchStatus(computer, token, read, peerDispatchReply{ThreadPeerRequest: reply.Requests[0], origin: origin})
}

func (a *App) recordDispatchStatus(computer, token string, read ownerRead, reply peerDispatchReply) error {
	var wakes []threadWakeRef
	unlock := a.lockComputerPairing(computer)
	err := read.commit(reply.origin, "", nil, func() error {
		var err error
		wakes, err = a.applyCurrentPeerRow(token, computer, reply.ThreadPeerRequest, threadPeerHearing{at: time.Now()})
		return err
	})
	unlock()
	a.deliverThreadWakes(wakes)
	if err == nil {
		a.sendOwedThreadRequestAcks(computer)
	}
	return err
}

// admitRemoteRequest checks the pairing with computerID, admits the
// caller's work and writes row with the name the person knows the computer
// by, all inside the pairing's fence (lockComputerPairing), so a pairing
// being ended takes the row with the rest or refuses it. The work stays admitted from the row to the
// destination's answer, so a rest of the caller stops a request the
// destination has settled or holds; the caller ends it with done.
func (t threadToolsApp) admitRemoteRequest(
	ctx context.Context, caller threadtools.Caller, computerID string, row store.ThreadRequest,
) (_ threadtools.Peer, done func(), _ error) {
	unlock := t.app.lockComputerPairing(computerID)
	defer unlock()
	peer, err := t.Peer(ctx, computerID)
	if err != nil {
		return nil, nil, err
	}
	row.TargetComputerName = peer.Computer().Name
	done, err = t.app.admitWork(caller.ThreadID, caller.Session)
	if err != nil {
		return nil, nil, err
	}
	if err := t.app.store.InsertThreadRequest(row); err != nil {
		done()
		return nil, nil, err
	}
	return peer, done, nil
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

// threadRequestAdmissionWindow is how long after one attempt's dispatch its
// handler on the destination could still admit the token: the call's wait,
// which bounds that handler. Each attempt records the end of its own
// window on the row before it is sent (store.ThreadRequest.AdmitUntil), so
// the deadline covers exactly the attempts the call makes and survives a
// restart of this computer.
var threadRequestAdmissionWindow = threadPeerCallWait("ThreadToolCall")

type peerDispatchReply struct {
	ThreadPeerRequest
	origin attachedbackends.PeerRead
}

// callThreadPeerRequest forwards one request-minting call and returns the
// receipt the destination reports, or the last failure and when the last
// attempt that may have started the request there was sent (zero when
// none may have).
//
// An attempt may have started it unless it was never sent
// (rpcclient.NotSent) or the destination answered it with a refusal, which
// it gives only for a request it did not start. Only an attempt that may
// have started it is tried again, with the same token, because what was
// lost may be the reply and not the work. A send makes nothing and is sent
// again to any destination; a spawn or an ask only to one whose first
// attempt's connection proved thread-tools.v2, which answers a token it
// holds from its receipt. One that predates it settles a receipt whose
// thread is still being made and lets that thread run anyway, so such a
// call is left to the poll, and no attempt is recorded that is not sent. A
// refusal of a later attempt says nothing about the work an earlier one may
// have started, so the request stays open for the poller.
func (a *App) callThreadPeerRequest(ctx context.Context, computer threadtools.Computer, call ThreadPeerCall) (peerDispatchReply, time.Time, error) {
	var sent time.Time
	var failed error
	v2 := false
	for attempt := 1; ; attempt++ {
		protocol := transport.CapabilityThreadTools
		if attempt > 1 {
			if call.Tool != "thread_send" {
				if !v2 {
					return peerDispatchReply{}, sent, failed
				}
				protocol = transport.CapabilityThreadToolsV2
			}
			if err := a.store.DispatchThreadRequest(call.Token, time.Now().Add(threadRequestAdmissionWindow).UnixMilli()); err != nil {
				// Unrecorded, the poll could read this attempt's token as
				// never started while it is being admitted: it is not sent,
				// and the earlier attempt's deadline stands.
				logThreadRequestSweep("record the retry of request "+call.Token, err)
				return peerDispatchReply{}, sent, failed
			}
		}
		dispatched := time.Now()
		rpc, cancel := context.WithTimeout(ctx, threadPeerCallWait("ThreadToolCall"))
		var reply ThreadPeerReply
		origin, err := a.backends.ReadThreadPeer(rpc, computer.ID, protocol, transport.CapabilityThreadToolsV2, "ThreadToolCall", &reply, call)
		cancel()
		if attempt == 1 {
			v2 = origin.ProvedGeneration()
		}
		if err != nil {
			if attempt > 1 && attachedbackends.Unsupported(err) {
				return peerDispatchReply{}, sent, failed
			}
			_, _, uncertain := threadErrorDetails(call.Tool, err)
			if uncertain {
				sent = dispatched
			}
			failed = a.threadOperationError(call.Tool, computer.ID, "", err)
			if !uncertain || attempt == threadPeerAdmissionAttempts {
				return peerDispatchReply{}, sent, failed
			}
			continue
		}
		if reply.Request == nil {
			return peerDispatchReply{}, dispatched, errorsx.Public(threadtools.CodeUnreachable,
				fmt.Sprintf("%s accepted %s without reporting the request.", threadtools.NameOfComputer(computer), call.Tool), nil)
		}
		return peerDispatchReply{ThreadPeerRequest: *reply.Request, origin: origin}, dispatched, nil
	}
}
