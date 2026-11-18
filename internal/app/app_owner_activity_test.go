package app

import (
	"slices"
	"strconv"
	"testing"
)

func TestOwnerActivityKeepsChangesUntilObserved(t *testing.T) {
	var events ownerActivity
	first, wait := events.snapshot("desk", "launch", OwnerCursor{Launch: "launch"})
	events.note("other", "hidden", true)
	select {
	case <-wait:
		t.Fatal("another device's change woke this owner")
	default:
	}
	events.note("desk", "spawn", true)
	<-wait
	events.note("desk", "spawn", false)
	seen, _ := events.snapshot("desk", "launch", first.OwnerCursor)
	if seen.Reset || !slices.Equal(seen.Tokens, []string{"spawn"}) || !slices.Equal(seen.PromptTokens, []string{"spawn"}) {
		t.Fatalf("prompt followed by ordinary activity = %+v", seen)
	}
	events.note("desk", "spawn", false)
	newer, _ := events.snapshot("desk", "launch", seen.OwnerCursor)
	if newer.Version <= seen.Version || !slices.Equal(newer.Tokens, seen.Tokens) {
		t.Fatalf("change while owner read version %d = %+v", seen.Version, newer)
	}
	other, _ := events.snapshot("other", "launch", OwnerCursor{Launch: "old"})
	if !other.Reset {
		t.Fatalf("restart without reset: %+v", other)
	}
}

func TestOwnerOverflowResetsOnlyAnOwnerBehindItsDiscardedChange(t *testing.T) {
	var feed ownerActivity
	start, _ := feed.snapshot("desk", "launch", OwnerCursor{})
	feed.note("desk", "first", true)
	first, _ := feed.snapshot("desk", "launch", start.OwnerCursor)
	for i := range ownerChangeLimit {
		feed.note("desk", strconv.Itoa(i), false)
	}
	behind, _ := feed.snapshot("desk", "launch", start.OwnerCursor)
	current, _ := feed.snapshot("desk", "launch", first.OwnerCursor)
	if !behind.Reset || current.Reset || len(current.Tokens) != ownerChangeLimit || slices.Contains(current.Tokens, "first") {
		t.Fatalf("overflow returned reset %v to old cursor and reset %v, %d tokens to covered cursor", behind.Reset, current.Reset, len(current.Tokens))
	}
}

func TestOwnerRecoversRenamedWorkerWithoutReadingPane(t *testing.T) {
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	launch := pair.destWire.srv.LaunchID()
	recover := func() {
		t.Helper()
		if err := pair.source.recoverOwnerPeer(t.Context(), pair.computer, launch); err != nil {
			t.Fatal(err)
		}
	}
	recover()
	if _, err := pair.source.store.RetireThreadRequestPoll(spawn.Token); err != nil {
		t.Fatal(err)
	}
	if err := pair.dest.RenameThread(child, "renamed while away"); err != nil {
		t.Fatal(err)
	}
	recover()
	if got := pair.request(t, spawn.Token).TargetThreadTitle; got != "renamed while away" {
		t.Fatalf("resting worker title after reconnect = %q", got)
	}
}

func TestOwnerRetriesPromptWhenTheLocalQueueWriteFails(t *testing.T) {
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	execOnStore(t, p.dbPath, `CREATE TRIGGER reject_owner_notice BEFORE INSERT ON flush_queue_items BEGIN SELECT raise(ABORT,'owner queue failure'); END`)
	read := func() error {
		return p.source.readOwnerChanges(t.Context(), p.computer, p.dest.transportServer.Load().LaunchID(), []string{p.spawn.Token}, nil)
	}
	if err := read(); err == nil {
		t.Fatal("read acknowledged despite the failed prompt queue write")
	}
	execOnStore(t, p.dbPath, `DROP TRIGGER reject_owner_notice`)
	if err := read(); err != nil {
		t.Fatal(err)
	}
	if got := remoteWakes(t, p.source, p.caller.ID); len(got) != 1 {
		t.Fatalf("retry queued %v", got)
	}
}

func TestPromptClosureQueuedBehindItsFirstListingWithdrawsTheWake(t *testing.T) {
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	listing, origin, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil || len(listing.Prompts) != 1 {
		t.Fatalf("held prompt listing: %+v, %v", listing, err)
	}
	release := make(chan struct{})
	p.source.workerPrompts.run(func() { <-release })
	first := p.source.noteRemotePrompts(origin, p.spawn, listing, nil)
	second := p.source.noteRemotePrompts(origin, p.spawn, ThreadAgentRows{}, nil)
	close(release)
	if second == nil {
		t.Fatal("the closure behind the first listing was dropped")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if wakes := remoteWakes(t, p.source, p.caller.ID); len(wakes) != 0 {
		t.Fatalf("prompt closed but its wake remains: %v", wakes)
	}
}

func TestPromptWithdrawalRetriesItsDurableQueueDelete(t *testing.T) {
	p := newRemotePromptPair(t, remoteApproveLine)
	p.openThere(t, 1)
	p.readOwnerState(t, p.source)
	if len(remoteWakes(t, p.source, p.caller.ID)) != 1 {
		t.Fatal("first notice missing")
	}
	execOnStore(t, p.dbPath, `CREATE TRIGGER fail_owner_withdraw BEFORE DELETE ON flush_queue_items BEGIN SELECT raise(ABORT,'failed owner withdrawal'); END`)
	_, origin, err := p.source.peerThreadAgentRows(t.Context(), p.spawn, 0)
	if err != nil {
		t.Fatal(err)
	}
	remove := func() error { return <-p.source.noteRemotePrompts(origin, p.spawn, ThreadAgentRows{}, nil) }
	if err := remove(); err == nil {
		t.Fatal("failed queue delete acknowledged")
	}
	execOnStore(t, p.dbPath, `DROP TRIGGER fail_owner_withdraw`)
	if err := remove(); err != nil {
		t.Fatal(err)
	}
	if got := remoteWakes(t, p.source, p.caller.ID); len(got) != 0 {
		t.Fatalf("retry left prompt wakes: %v", got)
	}
}
