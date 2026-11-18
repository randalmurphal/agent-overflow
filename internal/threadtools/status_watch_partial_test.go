package threadtools

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"
)

const thirdThreadID = "33333333-3333-4333-8333-333333333333"

// restingLocal is a local thread whose wait reports it resting at once.
func restingLocal(p *pair) {
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.local.status = StatusReport{
		Threads: []ThreadState{{ThreadID: localThreadID, Title: "Local", State: StateIdle}},
		WokeOn:  localThreadID,
	}
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	row, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T (%v)", value, value)
	}
	return row
}

// TestStatusWatchWaitsOnWhatResolvedWhenAComputerMissesTheLookup: a
// reference no answering computer holds, while another computer did not
// answer its lookup, is reported unresolved under no computer, with the
// computer that failed under errors. The thread that resolved is still
// waited on, and its wake and state are kept.
func TestStatusWatchWaitsOnWhatResolvedWhenAComputerMissesTheLookup(t *testing.T) {
	p := newPair(t)
	restingLocal(p)
	studio := p.peer.computer
	p.local.peers[studio.ID] = brokenPeer{computer: studio, err: publicf(CodeUnreachable, "Studio did not answer.")}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"wait_seconds":30}`)
	if result["woke_on"] != localThreadID {
		t.Fatalf("woke_on = %v, want the local thread that rested", result["woke_on"])
	}
	if len(p.local.states) != 1 || len(p.local.states[0].ThreadIDs) != 1 || p.local.states[0].ThreadIDs[0] != localThreadID {
		t.Fatalf("waits = %+v, want one on the thread that resolved", p.local.states)
	}
	threads := rows(t, result["threads"])
	if len(threads) != 2 || field(t, threads[0], "state") != StateIdle {
		t.Fatalf("threads = %v, want the local thread as it rested and the unresolved reference", threads)
	}
	unresolved := object(t, threads[1])
	if unresolved["thread_id"] != remoteThreadID || unresolved["state"] != StateUnresolved || unresolved["resting"] != false ||
		unresolved["error_code"] != CodeResolutionIncomplete {
		t.Fatalf("the unresolved row = %v, want the reference unresolved for an incomplete lookup", unresolved)
	}
	if _, claimed := unresolved["computer_id"]; claimed {
		t.Fatalf("the unresolved row = %v, claims a computer no answer named", unresolved)
	}
	failures := rows(t, result["errors"])
	if len(failures) != 1 || field(t, failures[0], "computer_id") != studio.ID {
		t.Fatalf("errors = %v, want Studio, which did not answer the lookup", failures)
	}
}

// silentLookupPeer is a computer that answers no lookup until the call gives
// up on it.
type silentLookupPeer struct {
	*serverPeer
}

func (p silentLookupPeer) Resolve(ctx context.Context, _ string) (Resolution, error) {
	<-ctx.Done()
	return Resolution{}, ctx.Err()
}

// TestUnansweredLookupsCostTheCallOneDeadline: two references a silent
// computer may hold, listed around a local thread that rests: one this
// computer has never seen, and one it moved to that computer, whose lookup
// there runs after the fan-out. Their lookups run together under one
// deadline, so the wake comes back within one ResolutionTimeout, not one
// per reference, and the rows keep the order the call listed them in.
func TestUnansweredLookupsCostTheCallOneDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPair(t)
		restingLocal(p)
		studio := p.peer.computer
		p.local.peers[studio.ID] = silentLookupPeer{serverPeer: p.peer}
		p.local.movedRefs = []string{thirdThreadID}
		p.local.movedTo, p.local.movedToName = studio.ID, studio.Name

		started := time.Now()
		result := call(t, p.server, localCaller(), "thread_status",
			`{"thread_ids":["`+remoteThreadID+`","`+localThreadID+`","`+thirdThreadID+`"],"wait_seconds":1}`)
		if took := time.Since(started); took > ResolutionTimeout {
			t.Fatalf("the wake came back after %v, want within one lookup deadline (%v)", took, ResolutionTimeout)
		}
		if result["woke_on"] != localThreadID {
			t.Fatalf("woke_on = %v, want the local thread", result["woke_on"])
		}
		threads := rows(t, result["threads"])
		if len(threads) != 3 {
			t.Fatalf("threads = %v, want all three references", threads)
		}
		for index, want := range []struct{ id, state, computer string }{
			{remoteThreadID, StateUnresolved, ""}, {localThreadID, StateIdle, ""}, {thirdThreadID, StateUnresolved, studio.ID},
		} {
			row := object(t, threads[index])
			if computer, _ := row["computer_id"].(string); row["thread_id"] != want.id || row["state"] != want.state ||
				(want.computer != "" && computer != want.computer) {
				t.Fatalf("row %d = %v, want %s %s", index, row, want.id, want.state)
			}
		}
		if failures := rows(t, result["errors"]); len(failures) != 1 || field(t, failures[0], "computer_id") != studio.ID {
			t.Fatalf("errors = %v, want Studio once", failures)
		}
	})
}

// TestStatusWatchNamesTheOwnerOfAMovedThreadThatDidNotAnswer: a thread this
// computer moved to Studio is Studio's, so when Studio does not answer its
// lookup the unresolved row names Studio.
func TestStatusWatchNamesTheOwnerOfAMovedThreadThatDidNotAnswer(t *testing.T) {
	p := newPair(t)
	restingLocal(p)
	studio := p.peer.computer
	p.local.movedRefs = []string{remoteThreadID}
	p.local.movedTo, p.local.movedToName = studio.ID, studio.Name
	p.local.peers[studio.ID] = brokenPeer{computer: studio, err: publicf(CodeUnreachable, "Studio did not answer.")}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"wait_seconds":30}`)
	moved := object(t, rows(t, result["threads"])[1])
	if moved["state"] != StateUnresolved || moved["computer_id"] != studio.ID {
		t.Fatalf("the moved thread's row = %v, want it unresolved on Studio", moved)
	}
	if failures := rows(t, result["errors"]); len(failures) != 1 || field(t, failures[0], "computer_id") != studio.ID {
		t.Fatalf("errors = %v, want Studio", failures)
	}
}

// TestStatusWatchKeepsItsRefusals: an ambiguous reference and the caller's
// own thread are mistakes in the call, refused whatever else resolved.
func TestStatusWatchKeepsItsRefusals(t *testing.T) {
	p := newPair(t)
	restingLocal(p)
	p.local.addThread(Thread{ID: "44444444-0000-4000-8000-000000000001", Title: "One"})
	p.local.addThread(Thread{ID: "44444444-0000-4000-8000-000000000002", Title: "Two"})
	callErr(t, p.server, localCaller(), "thread_status", `{"thread_ids":["`+localThreadID+`","44444444"],"wait_seconds":30}`, CodeAmbiguous)
	p.local.addThread(Thread{ID: localCaller().ThreadID, Title: "Caller"})
	callErr(t, p.server, localCaller(), "thread_status", `{"thread_ids":["`+localThreadID+`","`+localCaller().ThreadID+`"],"wait_seconds":30}`, CodeSelfSend)
	// A computer that answered the lookup of a thread moved to it with a
	// refusal answered it.
	studio := p.peer.computer
	p.local.movedRefs = []string{remoteThreadID}
	p.local.movedTo, p.local.movedToName = studio.ID, studio.Name
	p.local.peers[studio.ID] = brokenPeer{computer: studio, err: publicf(CodeUnsupported, "Assistants are turned off in Settings on Studio.")}
	callErr(t, p.server, localCaller(), "thread_status", `{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"wait_seconds":30}`, CodeUnsupported)
}

// TestStatusWatchReadsAComputersRefusalAsItsThreads: a computer that answered
// the wait with a refusal (one on v0.0.19 refuses the whole wait over one
// thread it no longer holds) answered: its threads carry the refusal, and it
// is not reported as a computer that did not answer.
func TestStatusWatchReadsAComputersRefusalAsItsThreads(t *testing.T) {
	p := newPair(t)
	restingLocal(p)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	studio := p.peer.computer
	p.local.peers[studio.ID] = statusFailingPeer{serverPeer: p.peer, err: publicf(CodeNotFound, "No thread %s on Studio.", remoteThreadID)}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"wait_seconds":30}`)
	if result["woke_on"] != localThreadID || result["errors"] != nil {
		t.Fatalf("result = %v, want the local wake and no computer under errors", result)
	}
	remote := object(t, rows(t, result["threads"])[1])
	if remote["state"] != StateUnavailable || remote["error_code"] != CodeNotFound || remote["computer_id"] != studio.ID {
		t.Fatalf("the remote row = %v, want Studio's refusal on its thread", remote)
	}
}

// forwardingPeer serves its watch through the destination's forwarded entry
// point, as the real peer method does, after running before.
type forwardingPeer struct {
	*serverPeer
	before func()
}

func (p forwardingPeer) Watch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	p.before()
	result, err := p.server.CallForwarded(ctx, p.caller, "thread_status", args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// TestAThreadThatLeftItsComputerDuringAWaitIsItsOwnRow: one of two threads
// on Studio is deleted between the call's resolution and Studio's wait.
// Studio reports that thread as gone, which ends the wait, and its sibling
// as it stands; Studio is not reported as a computer that did not answer.
func TestAThreadThatLeftItsComputerDuringAWaitIsItsOwnRow(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Deleted"})
	p.remote.addThread(Thread{ID: thirdThreadID, Title: "Working"})
	p.remote.status = StatusReport{Threads: []ThreadState{{ThreadID: thirdThreadID, Title: "Working", State: StateRunning}}}
	studio := p.peer.computer
	p.local.peers[studio.ID] = forwardingPeer{serverPeer: p.peer, before: func() { delete(p.remote.threads, remoteThreadID) }}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+remoteThreadID+`","`+thirdThreadID+`"],"wait_seconds":30}`)
	if result["woke_on"] != remoteThreadID || result["errors"] != nil {
		t.Fatalf("result = %v, want the deleted thread to end the wait and no computer under errors", result)
	}
	threads := rows(t, result["threads"])
	gone, working := object(t, threads[0]), object(t, threads[1])
	if gone["state"] != StateUnavailable || gone["error_code"] != CodeNotFound || gone["computer_id"] != studio.ID {
		t.Fatalf("the deleted thread's row = %v, want it gone from Studio", gone)
	}
	if working["state"] != StateRunning || working["computer_id"] != studio.ID {
		t.Fatalf("the sibling's row = %v, want it as Studio reported it", working)
	}
	if len(p.remote.states) != 1 || p.remote.states[0].WaitSeconds != 0 || len(p.remote.states[0].ThreadIDs) != 1 {
		t.Fatalf("Studio's reads = %+v, want one read of the sibling without a wait", p.remote.states)
	}
}

// unansweringPeer resolves its computer's threads and then answers no wait,
// not even a read, until the call gives up on it.
type unansweringPeer struct {
	*serverPeer
}

func (p unansweringPeer) Watch(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAWakeIsReturnedWithinOneRefreshDeadline: the local thread rests while
// the waits on two other computers are parked, and neither answers the read
// that follows. The wake comes back within one WakeRefreshTimeout, not one
// per computer and not never, with both computers' threads unavailable.
func TestAWakeIsReturnedWithinOneRefreshDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPair(t)
		restingLocal(p)
		p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
		desktopApp := newFakeApp("Desktop")
		desktopApp.addThread(Thread{ID: thirdThreadID, Title: "Third"})
		desktop := Computer{ID: "desktop", Name: "Desktop"}
		p.local.computers = append(p.local.computers, desktop)
		p.local.peers[p.peer.computer.ID] = unansweringPeer{serverPeer: p.peer}
		p.local.peers[desktop.ID] = unansweringPeer{serverPeer: &serverPeer{computer: desktop, server: New(desktopApp), caller: localCaller()}}

		started := time.Now()
		result := call(t, p.server, localCaller(), "thread_status",
			`{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`","`+thirdThreadID+`"],"wait_seconds":30}`)
		if took := time.Since(started); took > WakeRefreshTimeout {
			t.Fatalf("the wake came back after %v, want within %v", took, WakeRefreshTimeout)
		}
		if result["woke_on"] != localThreadID {
			t.Fatalf("woke_on = %v, want the local thread", result["woke_on"])
		}
		threads := rows(t, result["threads"])
		for _, row := range threads[1:] {
			if field(t, row, "state") != StateUnavailable {
				t.Fatalf("threads = %v, want the unanswered computers' threads unavailable", threads)
			}
		}
		if failures := rows(t, result["errors"]); len(failures) != 2 {
			t.Fatalf("errors = %v, want both computers that did not answer in time", failures)
		}
	})
}
