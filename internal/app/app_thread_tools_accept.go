package app

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Accepting a thread request on this computer: the receipt, the thread that
// answers it and the naming write between them, and the refusal a request
// that could not be started returns and records.

// acceptRequest writes the destination row, creates the thread that answers
// when the kind has one to create, and names it on the source row when that
// row is on this computer.
//
// For this computer's own caller, request is the source row: written with
// the receipt in one transaction before any work can run under the token, so
// a failure part way through names a request the caller can ask about
// rather than leaving a thread nothing records. A paired computer's request
// has its source row there; only its token, kind and target are used here.
//
// The receipt is the record of record: a retry with the same token returns
// the existing acceptance and `create` never runs again, which is what makes
// a lost reply safe to retry with no second spawn, fork or message. A retry
// never settles the receipt either, even one whose thread is not named yet:
// the attempt that wrote it is making that thread in this process, since
// the boot sweep settles every receipt a previous process left open.
// `fresh` is false for exactly that retry, so the dispatch does not run
// twice either. A local token is fresh, so it is never a retry. own places the
// thread owned by the naming write: a local spawn's through its request, a
// paired computer's through its receipt, which is then its link. `create`
// returns what it made with the thread, a worktree cut and a group; until
// the naming write names the thread, they are this attempt's to undo.
func (t threadToolsApp) acceptRequest(
	origin threadRequestOrigin, request store.ThreadRequest,
	create func() (store.Thread, madeWithThread, error), own bool,
) (target string, fresh bool, err error) {
	token, kind := request.Token, request.Kind
	receiptRow := threadReceiptFor(origin, token, kind, request.TargetThreadID, origin.foreign && own)
	var receipt store.ThreadRequestReceipt
	created := true
	if origin.foreign {
		receipt, created, err = t.app.store.AcceptThreadRequestReceipt(receiptRow)
	} else {
		request.CallerThreadID = origin.caller.ThreadID
		request.State = store.ThreadRequestUnconfirmed
		if request.TargetThreadID != "" {
			request.State = store.ThreadRequestAccepted
		}
		receipt, err = t.app.store.AcceptLocalThreadRequest(request, receiptRow)
	}
	if err != nil {
		return "", false, err
	}
	target = receipt.TargetThreadID
	if !created {
		// Another attempt with this token accepted it, and owns it: its
		// thread may still be being made, and that attempt names it or
		// settles the receipt. This one reports the acceptance as it stands.
		return target, false, nil
	}
	made := false
	listing := store.ListingUnchanged
	if target == "" && create != nil {
		thread, madeWith, err := create()
		if err != nil {
			return "", false, err
		}
		target = thread.ID
		if hook := t.app.spawnNamingForTest; hook != nil {
			hook(origin.caller.ThreadID)
		}
		if origin.foreign {
			var named bool
			named, listing, err = t.app.store.SetThreadReceiptTarget(token, target, own)
			if err == nil && !named {
				// Settled while its thread was being made: cancelled.
				err = errorsx.Public(threadtools.CodeInvalidRequest,
					"That request was settled before its thread could be recorded, so the thread made for it was removed and nothing ran under it.", nil)
			}
		} else {
			// One write names the thread on both rows and accepts the source
			// row; a request a settlement already moved keeps its state.
			listing, err = t.app.store.NameLocalThreadRequestTarget(token, target, own)
		}
		if err != nil {
			return "", false, t.discardUnnamedThread(thread, madeWith, err)
		}
		made = true
	}
	if target == "" {
		return "", false, fmt.Errorf("thread tools: request %s has no target thread", token)
	}
	if kind == store.ThreadRequestSpawn && (made || !origin.foreign) {
		// A spawn's thread was created without a frame (createSpawnedThread)
		// and goes out now that its request names and places it, by the
		// listing change the naming write read, so an owned one is never
		// listed. An assistant's thread, made before its request, goes out
		// again naming its spawner.
		t.app.announcePlacement(target, listing)
		// A fork it made carries its source's async questions, which go to
		// this spawner now that the request names it, though a frame of the
		// fork before the naming left them the person's.
		t.app.adoptAsyncQuestionPrompts(target)
	}
	if origin.foreign {
		// This thread now answers a paired computer's request, so it serves
		// the thread tools whether or not this computer's own switch is on.
		t.app.refreshThreadToolsAdmission(target)
	}
	return target, true, nil
}

// discardUnnamedThread takes back a thread its request could not name, and
// what its creation made with it as far as that is still the creation's
// alone, and returns the refusal. Nothing names the thread: no receipt, no
// request and no owner, and its message was never sent, so it would be
// work the caller cannot reach and will make again. The cut and the group
// are undone here rather than by the delete, which never touches a
// worktree: they are this creation's own unfinished work. A checkout or
// branch the undo kept, because something not the cut's is in it, is named
// in the refusal with why.
func (t threadToolsApp) discardUnnamedThread(thread store.Thread, made madeWithThread, cause error) error {
	if err := t.app.deleteThread(thread.ID); err != nil {
		log.Printf("thread tools: delete thread %s after its request could not name it: %v", thread.ID, err)
		return errorsx.Public(threadtools.CodeInvalidRequest, fmt.Sprintf(
			"Thread %s was created but its request could not record it (%v), and removing it failed too. It is on this computer.",
			thread.ID, cause), errors.Join(cause, err))
	}
	if made.group != nil {
		t.dropSpawnGroup(*made.group)
	}
	if made.cut.Path == "" {
		return cause
	}
	// The delete stopped and joined the thread's setup run, so no recipe is
	// writing into the checkout while it goes.
	undoErr := threadWorkspacePort(t).RemoveCutWorktree(thread.ProjectPath, made.cut)
	if undoErr == nil {
		return cause
	}
	log.Printf("thread tools: undo the worktree %s of thread %s its request could not name: %v", made.cut.Path, thread.ID, undoErr)
	_, said, public := errorsx.PublicDetails(cause)
	if !public {
		said = "Its request could not record the thread made for it, so the thread was removed."
	}
	var kept *gitops.CutKept
	if errors.As(undoErr, &kept) {
		said += " " + kept.Error()
	} else {
		said += fmt.Sprintf(" Removing the worktree made for it at %s failed (%v); it is on this computer.", made.cut.Path, undoErr)
	}
	return errorsx.Public(threadtools.CodeInvalidRequest, said, errors.Join(cause, undoErr))
}

// threadReceiptFor is the receipt origin's request token is accepted
// under: owned by the device that sent it, and naming its source for the
// footer and the sender an agent row names. link marks a paired computer's
// owned spawn, whose receipt owns its thread here.
func threadReceiptFor(origin threadRequestOrigin, token, kind, targetThreadID string, link bool) store.ThreadRequestReceipt {
	caller := origin.caller
	return store.ThreadRequestReceipt{
		SpawnLink:          link,
		TrackSpawn:         origin.tracksSpawn,
		Token:              token,
		OwnerDeviceID:      origin.ownerDevice,
		SourceComputerID:   caller.ComputerID,
		SourceComputerName: caller.ComputerName,
		SourceThreadID:     caller.ThreadID,
		SourceThreadTitle:  caller.Title,
		Kind:               kind,
		TargetThreadID:     targetThreadID,
	}
}

// failRequest settles a source row on this computer whose acceptance or
// dispatch failed before the destination ever ran it, and returns the
// refusal the caller's tool call reads.
//
// The row is settled rather than left open because nothing else can settle
// it: no receipt is running, no turn will end, and no poller will ask. A row
// left `unconfirmed` would tell the agent to keep checking a token that never
// changes. A request a paired computer made has no row here at all; its
// source settles it from the refusal this returns.
//
// The call's own error IS the delivery, so the row is settled and marked
// delivered in one transaction: a request made with notify must not also
// send the same refusal as a message a moment later. For the same reason the
// error carries the cause. It is the request's stored answer, which
// `thread_status` returns to the same agent, and a refusal that hid it sent
// the agent off to find out why with another call.
//
// A receipt accepted before the failure is settled too, through the settle
// door, which drops the scratch thread an ask had already forked and tells a
// paired computer's poller what happened. Without it a forwarded request
// whose dispatch failed would leave a receipt open on this computer forever.
// The source row is settled FIRST so a local request keeps the word for what
// happened to it, `refused`, rather than the `errored` its own receipt
// collects.
func (t threadToolsApp) failRequest(token string, cause error) error {
	return t.settleRefusal(token, threadRequestRefusal(cause, ""))
}

func (t threadToolsApp) settleRefusal(token string, refusal error) error {
	_, message, _ := errorsx.PublicDetails(refusal)
	row, found, err := t.app.store.GetThreadRequest(token)
	if err != nil {
		log.Printf("thread tools: read request %s to refuse it: %v", token, err)
	}
	if found && row.TargetComputerID == "" {
		// A row naming another computer is a request THIS computer sent
		// there; a failure accepting a forwarded call is not its settlement.
		// What the receipt records below, which another computer would
		// collect.
		t.refuseRequest(token, message, t.answerAgent(token, row.TargetThreadID))
	}
	if err := t.app.settleThreadReceipt(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadReceiptErrored,
		Answer:     []byte(message),
		AnswerKind: store.ThreadAnswerError,
	}); err != nil {
		log.Printf("thread tools: settle refused receipt %s: %v", token, err)
	}
	return refusal
}

// refuseRequest settles a source row `refused` with the refusal its own call
// is about to return, already delivered. It takes the token's settle lock, as
// every settlement of a source row does, so no wake is decided between the
// two halves.
func (t threadToolsApp) refuseRequest(token, message string, agent *store.ThreadAnswerAgent) {
	unlock := t.app.threadRequestSettleLock(token)
	defer unlock()
	if _, err := t.app.store.SettleThreadRequestDelivered(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestRefused,
		Answer:     []byte(message),
		AnswerKind: store.ThreadAnswerError,
		Agent:      agent,
	}); err != nil {
		log.Printf("thread tools: settle refused request %s: %v", token, err)
	}
}

// threadRequestRefusal is the refusal a request that could not be started
// returns, and the answer its record keeps.
//
// A cause that already carries reviewed prose is said as it is. Any other is
// said in full, in the failing operation's own words, because the caller
// chose what failed (a branch git refuses, a thread whose queue is full, a
// provider that cannot start) and cannot fix a cause it is not told. The one
// cause withheld is the database failing, whose text can quote a statement
// and describes nothing the caller did: it is logged where it happened and
// answered with a fixed sentence.
//
// `spawned` is the thread a spawn created before its prompt failed to reach
// it. The refusal names it, because a caller told to make the request again
// would spawn a second thread beside the first.
func threadRequestRefusal(cause error, spawned string) error {
	code, said, reviewed := errorsx.PublicDetails(cause)
	next := " Nothing ran under it; fix the cause and make the request again."
	switch {
	case reviewed && spawned == "":
		return cause
	case reviewed:
	case store.IsDatabaseFailure(cause):
		log.Printf("thread tools: a request could not be started: %v", cause)
		code, next = threadtools.CodeInvalidRequest, " Nothing ran under it."
		said = "That request could not be started: the database on the computer that would run it failed, and the cause is in that computer's log."
	default:
		code = threadtools.CodeInvalidRequest
		said = "That request could not be started: " + strings.TrimSuffix(cause.Error(), ".") + "."
	}
	if spawned != "" {
		next = fmt.Sprintf(" Thread %s was created for it and its prompt did not run. Send the prompt to that thread with thread_send instead of spawning again.", spawned)
	}
	return errorsx.Public(code, said+next, cause)
}
