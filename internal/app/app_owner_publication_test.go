package app

import (
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

func TestOwnerSeesNestedLoopOnlyAfterStateCanServeIt(t *testing.T) {
	pair := newReachPair(t)
	spawn, remoteWorker := remoteRestedWorker(t, pair)
	child, err := createTestThread(t, pair.dest, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	local := newThreadRequestToken()
	if err := pair.dest.store.InsertThreadRequest(store.ThreadRequest{
		Token: local, Kind: store.ThreadRequestSpawn, CallerThreadID: remoteWorker,
		TargetThreadID: child.ID, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.dest.store.SettleThreadRequest(local, store.ThreadRequestOpenStates(),
		store.ThreadRequestSettlement{State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal}); err != nil {
		t.Fatal(err)
	}
	pair.dest.threadRequestsWG.Wait()

	owner := pair.receipt(t, spawn.Token).OwnerDeviceID
	launch := pair.destWire.srv.LaunchID()
	feed := &pair.dest.ownerActivity
	device := watchOwner(t, feed, owner)
	first, _ := feed.snapshot(device, launch, OwnerCursor{})
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	pair.dest.beforeWorkerReassessForTest = func(id string) {
		if id == child.ID {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
	}
	armLoop(t, pair.dest, child.ID)
	within(t, "loop reassessment", entered)
	pending, _ := feed.snapshot(device, launch, first.OwnerCursor)
	unblock()
	pair.dest.threadRequestsWG.Wait()
	if slices.Contains(pending.Tokens, spawn.Token) {
		t.Fatal("owner could acknowledge the loop before its subtree was reassessed")
	}
	ready, _ := feed.snapshot(device, launch, first.OwnerCursor)
	if !slices.Contains(ready.Tokens, spawn.Token) {
		t.Fatal("no owner change after reassessment")
	}
	if err := pair.source.readOwnerChanges(t.Context(), pair.computer, launch, ready.Tokens, nil); err != nil {
		t.Fatal(err)
	}
	if !pair.request(t, spawn.Token).NestedOpen {
		t.Fatal("owner consumed loop change with nested_open=false")
	}
}
