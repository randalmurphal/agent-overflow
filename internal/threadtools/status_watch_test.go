package threadtools

import (
	"context"
	"encoding/json"
	"testing"
)

// statusFailingPeer is a computer that resolves its threads and then stops
// answering: the wait a thread_status forwards to it fails, as one does
// when that computer goes to sleep after the ids resolved.
type statusFailingPeer struct {
	*serverPeer
	err error
}

func (p statusFailingPeer) Watch(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, p.err
}

// TestStatusWatchKeepsWhatItObservedWhenAComputerStopsAnswering: a wait on
// a local and a remote thread returns the local wake and state, with the
// remote thread marked unavailable and the reason named, rather than one
// error that discards what the local half saw.
func TestStatusWatchKeepsWhatItObservedWhenAComputerStopsAnswering(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	p.local.status = StatusReport{
		Threads: []ThreadState{{ThreadID: localThreadID, Title: "Local", State: StateIdle}},
		WokeOn:  localThreadID,
	}
	p.local.peers["studio"] = statusFailingPeer{serverPeer: p.peer, err: publicf(CodeUnreachable, "Studio did not answer.")}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"wait_seconds":30}`)
	if result["woke_on"] != localThreadID {
		t.Fatalf("woke_on = %v, want the local thread that rested", result["woke_on"])
	}
	threads := rows(t, result["threads"])
	if len(threads) != 2 {
		t.Fatalf("threads = %v, want both threads the call named", threads)
	}
	local, remote := threads[0].(map[string]any), threads[1].(map[string]any)
	if local["thread_id"] != localThreadID || local["state"] != StateIdle || local["resting"] != true {
		t.Fatalf("the local row = %v, want the state the local wait observed", local)
	}
	if remote["thread_id"] != remoteThreadID || remote["state"] != StateUnavailable || remote["resting"] != false || remote["computer_id"] != "studio" {
		t.Fatalf("the remote row = %v, want it unavailable on Studio", remote)
	}
	failures := rows(t, result["errors"])
	if len(failures) != 1 || field(t, failures[0], "computer_id") != "studio" || field(t, failures[0], "error") != "Studio did not answer." {
		t.Fatalf("errors = %v, want Studio's failure", failures)
	}
}

// TestStatusWatchFailsWhenNoComputerAnswered: with nothing observed there
// is nothing to report, so the call fails with the computer's own refusal.
func TestStatusWatchFailsWhenNoComputerAnswered(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	p.local.peers["studio"] = statusFailingPeer{serverPeer: p.peer, err: publicf(CodeUnreachable, "Studio did not answer.")}

	callErr(t, p.server, localCaller(), "thread_status", `{"thread_ids":["`+remoteThreadID+`"],"wait_seconds":30}`, CodeUnreachable)
}

// parkedPeer is a computer whose thread never comes to rest: a forwarded
// wait parks until the caller ends it, and a read answers at once.
type parkedPeer struct {
	*serverPeer
}

func (p parkedPeer) Watch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var wait struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	if err := json.Unmarshal(args, &wait); err != nil {
		return nil, err
	}
	if wait.WaitSeconds > 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.serverPeer.Watch(ctx, args)
}

// TestStatusWatchReadsTheGroupsAWakeEndedAgain: when the local thread rests,
// the wait still parked on the other computer is ended and that computer's
// thread is read as it stands, not reported unavailable.
func TestStatusWatchReadsTheGroupsAWakeEndedAgain(t *testing.T) {
	p := newPair(t)
	p.local.addThread(Thread{ID: localThreadID, Title: "Local"})
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	p.local.status = StatusReport{
		Threads: []ThreadState{{ThreadID: localThreadID, Title: "Local", State: StateIdle}},
		WokeOn:  localThreadID,
	}
	p.remote.status = StatusReport{Threads: []ThreadState{{ThreadID: remoteThreadID, Title: "Remote", State: StateRunning}}}
	p.local.peers["studio"] = parkedPeer{serverPeer: p.peer}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+localThreadID+`","`+remoteThreadID+`"],"wait_seconds":30}`)
	if result["woke_on"] != localThreadID || result["errors"] != nil {
		t.Fatalf("result = %v, want the local wake and no failure", result)
	}
	remote := rows(t, result["threads"])[1].(map[string]any)
	if remote["state"] != StateRunning || remote["computer_id"] != "studio" {
		t.Fatalf("the remote row = %v, want Studio's thread read as it stands", remote)
	}
}

// TestAWaitOnAnotherComputerThatRanOutSaysSo: the other computer's wait
// ran to its end with its thread still running, so the call says the wait
// ran out.
func TestAWaitOnAnotherComputerThatRanOutSaysSo(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Remote"})
	p.remote.status = StatusReport{Threads: []ThreadState{{ThreadID: remoteThreadID, Title: "Remote", State: StateRunning}}, TimedOut: true}

	result := call(t, p.server, localCaller(), "thread_status", `{"thread_ids":["`+remoteThreadID+`"],"wait_seconds":30}`)
	if result["timed_out"] != true || result["woke_on"] != nil {
		t.Fatalf("result = %v, want a wait that ran out", result)
	}
}

// olderSilentReadPeer is a computer on v0.0.19 that refuses a wait over two
// threads because it no longer holds the first, then answers the read of
// the second with nothing.
type olderSilentReadPeer struct {
	*serverPeer
}

func (p olderSilentReadPeer) Watch(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var call struct {
		ThreadIDs []string `json:"thread_ids"`
	}
	if err := json.Unmarshal(args, &call); err != nil {
		return nil, err
	}
	if len(call.ThreadIDs) == 1 && call.ThreadIDs[0] == thirdThreadID {
		return nil, publicf(CodeUnreachable, "Studio did not answer.")
	}
	return nil, publicf(CodeNotFound, "No thread or assistant matches %q on this computer.", remoteThreadID)
}

// TestAThreadAnOlderComputerDidNotReadIsUnavailable: of the two threads an
// older computer's refusal covered, the one it no longer holds is still the
// wake when the read of the other goes unanswered; that one reads
// unavailable and the computer is named under errors.
func TestAThreadAnOlderComputerDidNotReadIsUnavailable(t *testing.T) {
	p := newPair(t)
	p.remote.addThread(Thread{ID: remoteThreadID, Title: "Deleted"})
	p.remote.addThread(Thread{ID: thirdThreadID, Title: "Silent"})
	studio := p.peer.computer
	p.local.peers[studio.ID] = olderSilentReadPeer{serverPeer: p.peer}

	result := call(t, p.server, localCaller(), "thread_status",
		`{"thread_ids":["`+remoteThreadID+`","`+thirdThreadID+`"],"wait_seconds":30}`)
	if result["woke_on"] != remoteThreadID {
		t.Fatalf("woke_on = %v, want the thread the computer no longer holds", result["woke_on"])
	}
	threads := rows(t, result["threads"])
	if silent := object(t, threads[1]); silent["thread_id"] != thirdThreadID || silent["state"] != StateUnavailable {
		t.Fatalf("the unread thread's row = %v, want it unavailable", silent)
	}
	if failures := rows(t, result["errors"]); len(failures) != 1 || field(t, failures[0], "computer_id") != studio.ID {
		t.Fatalf("errors = %v, want Studio, which did not answer the read", failures)
	}
}
