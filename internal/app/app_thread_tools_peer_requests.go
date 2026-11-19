package app

import (
	"context"
	"encoding/json"
	"strings"

	"agent-overflow/internal/agenthome"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// The destination half of a request a paired computer minted: a forwarded
// spawn, send or ask accepted under the source's token, a forwarded cancel,
// and the receipt's state as the device that owns it reads it.

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
	if call.Tool != "thread_cancel" {
		// From here until it returns, this handler may admit the token, and
		// a status answer says so (peerAdmissions).
		defer a.peerAdmissions.enter(token)()
		if hook := a.peerAdmissionForTest; hook != nil {
			hook(token)
		}
	}
	source := call.Source
	source.ThreadID = strings.TrimSpace(source.ThreadID)
	source.ComputerID = strings.TrimSpace(source.ComputerID)
	if !entityid.Valid(source.ThreadID) || !entityid.Valid(source.ComputerID) {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded request must name the thread and computer it came from.", nil)
	}
	// A thread on this computer is named without one, so a source that
	// claims this computer's id would pass for a local thread.
	if here, _ := a.backendIdentity(); source.ComputerID == here {
		return ThreadPeerReply{}, errorsx.Public(threadtools.CodeInvalidRequest,
			"A forwarded request must come from another computer.", nil)
	}
	source.Title = threadPeerTitle(source.Title)
	origin := threadRequestOrigin{
		caller:      source,
		foreign:     true,
		ownerDevice: owner,
		userMessage: call.UserMessage,
		inherit:     call.Inherit,
	}
	// What this computer calls the sender, kept on the receipt and the
	// message's attribution, not what the sender calls itself.
	origin.caller.ComputerName = a.threadTools().senderComputerName(origin)
	if call.SourceAgent != nil {
		// The role is the other computer's text going into this computer's
		// footer: bounded to one cleaned line here, whatever was sent.
		origin.agent = &threadtools.SenderAgent{Role: agenthome.BoundRole(call.SourceAgent.Role)}
	}
	if call.Tool == "thread_cancel" {
		return a.cancelPeerThreadRequest(ctx, origin, token, call)
	}
	held, err := a.refuseForeignToken(token, owner)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	if !held {
		if err := a.acceptPeerThreadRequest(ctx, origin, token, call); err != nil {
			return ThreadPeerReply{}, a.publicThreadToolError(call.Tool, call.Source.ThreadID, err)
		}
	}
	// A retry of a request this device already made here is answered from
	// its receipt as it stands, never checked or started again: the attempt
	// that wrote it owns it, and may still be making its thread.
	state, err := a.threadPeerRequestState(ctx, owner, token)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	return ThreadPeerReply{Request: &state}, nil
}

// acceptPeerThreadRequest decodes one forwarded spawn, send or ask and runs
// its destination half.
func (a *App) acceptPeerThreadRequest(ctx context.Context, origin threadRequestOrigin, token string, call ThreadPeerCall) error {
	adapter := a.threadTools()
	var err error
	switch call.Tool {
	case "thread_spawn":
		var spawn threadtools.SpawnCall
		if err = json.Unmarshal(call.Args, &spawn); err == nil {
			if err = refuseForwardedHomeFiles(spawn.HomeFiles); err == nil {
				spawn.ComputerID = ""
				_, err = adapter.acceptSpawn(ctx, origin, token, spawn)
			}
		}
	case "thread_send":
		var send threadtools.SendCall
		if err = json.Unmarshal(call.Args, &send); err == nil {
			if err = refuseForwardedHomeFiles(send.HomeFiles); err == nil {
				send.ComputerID = ""
				_, err = adapter.acceptSend(ctx, origin, token, send)
			}
		}
	case "thread_ask":
		var ask threadtools.AskCall
		if err = json.Unmarshal(call.Args, &ask); err == nil {
			ask.ComputerID = ""
			_, err = adapter.acceptAsk(ctx, origin, token, ask)
		}
	}
	return err
}

// refuseForeignToken rejects a token this device does not own before any
// work starts, and reports whether this device already holds a receipt
// under it. A receipt belongs to the authenticated device that created it:
// another device of the same computer holds no claim on it, and reusing its
// token would hand that device another device's answer.
func (a *App) refuseForeignToken(token, owner string) (bool, error) {
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return false, err
	}
	if found && receipt.OwnerDeviceID != owner {
		return false, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a request another device made.", nil)
	}
	// A token that already names a SOURCE row here belongs to a request one
	// of this computer's own threads made. A forwarded call carries the
	// source's token, so its token is never one of ours: accepting it would
	// let a paired computer answer, or refuse, this computer's own request.
	if _, exists, err := a.store.GetThreadRequest(token); err != nil {
		return false, err
	} else if exists {
		return false, errorsx.Public(threadtools.CodeRequestNotYours,
			"That token belongs to a request this computer made.", nil)
	}
	return found, nil
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
		report, err := a.threadTools().interruptForeignThread(ctx, origin, cancel.ThreadID, call.UserStop)
		if err != nil {
			return ThreadPeerReply{}, a.publicThreadToolError("thread_cancel", origin.caller.ThreadID, err)
		}
		return ThreadPeerReply{Effect: report}, nil
	}
	if _, err := a.refuseForeignToken(token, origin.ownerDevice); err != nil {
		return ThreadPeerReply{}, err
	}
	answer := threadRequestSenderCancel
	if call.UserStop {
		answer = threadRequestUserStop
	}
	effect, err := a.stopThreadRequestWorkFor(ctx, token, answer)
	if err != nil {
		return ThreadPeerReply{}, a.publicThreadToolError("thread_cancel", origin.caller.ThreadID, err)
	}
	state, err := a.threadPeerRequestState(ctx, origin.ownerDevice, token)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	return ThreadPeerReply{Request: &state, Effect: peerCancelEffect(effect)}, nil
}

// peerCancelEffect is a cancel's effect as a paired caller is told it. The
// peer call carries no version, and a caller built before the two effects
// that leave a turn alone renders an effect it does not know as an
// interruption, which would be false; nothing_to_stop is what every build
// reads as "nothing was stopped". The local caller keeps the precise word.
func peerCancelEffect(effect string) string {
	switch effect {
	case threadtools.EffectLeftRunning, threadtools.EffectLeftUnread:
		return threadtools.EffectNothing
	}
	return effect
}

// threadPeerRequestState reads one receipt for the device that owns it.
// A token this device never created is reported unknown rather than
// refused: the source settles it, and saying which other device holds it
// would leak that device's work.
func (a *App) threadPeerRequestState(_ context.Context, owner, token string) (ThreadPeerRequest, error) {
	state := ThreadPeerRequest{Token: token}
	receipt, found, err := a.store.GetThreadRequestReceipt(token)
	if err != nil {
		return ThreadPeerRequest{}, err
	}
	if !found || receipt.OwnerDeviceID != owner {
		return state, nil
	}
	state.Known = true
	state.RestRule = true
	state.Kind = receipt.Kind
	state.TargetThreadID = receipt.TargetThreadID
	state.State = receipt.State
	state.Answer = string(receipt.Answer)
	if receipt.Cause != "" {
		state.Answer = threadtools.Cause{Code: receipt.Cause, Arg: receipt.CauseArg}.Answer(state.Answer)
		state.Cause = &ThreadPeerCause{Code: receipt.Cause, Arg: receipt.CauseArg, Written: string(receipt.Answer)}
	}
	state.AnswerKind = receipt.AnswerKind
	state.LateReply = string(receipt.LateReply)
	state.LateReplyAt = receipt.LateReplyAt
	state.Revision = receipt.Revision
	state.CollectedRevision = receipt.CollectedRevision
	state.SettledAt = receipt.SettledAt
	state.ExpiresAt = receipt.ExpiresAt
	state.Instructions = receipt.SpawnInstructions
	if receipt.AnswerAgent != nil {
		state.AnswerAgent = &ThreadPeerAgent{Role: receipt.AnswerAgent.Role}
	}
	if receipt.LateReplyAgent != nil {
		state.LateReplyAgent = &ThreadPeerAgent{Role: receipt.LateReplyAgent.Role}
	}
	if receipt.TargetThreadID != "" {
		if thread, err := a.store.GetThread(receipt.TargetThreadID); err == nil {
			state.Title = thread.Title
			state.Provider = thread.Provider
			state.Placement = thread.Placement
		}
		if receipt.State == store.ThreadReceiptAccepted || receipt.State == store.ThreadReceiptRunning {
			// The same reading this computer's own Agents row makes.
			waiting := a.threadWaitingOn(receipt.TargetThreadID)
			state.Blocked, state.BlockedRoutedTo = waiting.live.Blocked(), waiting.routedTo()
			holder, err := a.routedToAsker(receipt.TargetThreadID, waiting.route, owner)
			if err != nil {
				return ThreadPeerRequest{}, err
			}
			state.BlockedRoutedToThread = holder
			state.BlockedRoutedToCaller = holder != "" && holder == receipt.SourceThreadID
			if a.restHeld(receipt.TargetThreadID, token) {
				rest, err := a.threadRest(receipt.TargetThreadID)
				if err != nil {
					return ThreadPeerRequest{}, err
				}
				state.WaitingOn = rest.waitingLine()
			}
		}
	}
	return state, nil
}
