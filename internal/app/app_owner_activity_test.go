package app

import (
	"agent-overflow/internal/store"
	"context"
	"slices"
	"testing"
	"time"
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

func TestOwnerRecoversRenamedWorkerWithoutReadingPane(t *testing.T) {
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine, `{"type":"result","subtype":"success","is_error":false}`}})
	ack := pair.spawnThere(t, "finish the work", nil)
	target := pair.receipt(t, ack.Token).TargetThreadID
	if err := pair.dest.RenameThread(target, "renamed while away"); err != nil {
		t.Fatal(err)
	}
	changed := make(chan struct{}, 1)
	pair.source.store.OnThreadRequestChanged(func(c store.ThreadRequestChange) {
		pair.source.noteThreadRequestChanged(c)
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err := pair.source.refreshOwnerPeers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		o := &pair.source.ownerUpdates
		o.mu.Lock()
		p := o.peers[pair.computer]
		o.mu.Unlock()
		if p != nil {
			p.cancel()
			<-p.done
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for pair.request(t, ack.Token).TargetThreadTitle != "renamed while away" {
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("rename never reached the spawner")
		}
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
